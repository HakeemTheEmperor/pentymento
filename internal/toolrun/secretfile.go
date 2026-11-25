// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package toolrun

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// Secret files hand credentials to child processes without putting them on
// the command line or in the environment (PRD 7.2): a .pgpass file for
// Postgres tools, a --config file for MongoDB tools. They live in their
// own directory, readable only by pentym's user, are named after the
// process that wrote them, and are deleted when the child exits; files of
// processes that no longer exist are deleted by CleanStale.

// secretFilePrefix starts every secret file's name: secret-<pid>-<rand>.
const secretFilePrefix = "secret-"

// SecretFile is a temporary file holding a credential.
type SecretFile struct {
	// Path is the file to point the tool at.
	Path string
}

// Remove deletes the file; a missing file is not an error.
func (f *SecretFile) Remove() error {
	if f == nil || f.Path == "" {
		return nil
	}
	if err := os.Remove(f.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// secretDir creates the secret-file directory, owner-only.
func secretDir(dir string) error {
	if dir == "" {
		return errors.New("no directory for temporary credential files")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return restrict(dir, true)
}

// WriteSecretFile writes content to a new owner-only file in dir. The
// content is the secret's bytes, already in the tool's format (see PGPass
// and MongoConfig).
func WriteSecretFile(dir, suffix string, content secrets.Secret) (*SecretFile, error) {
	if err := secretDir(dir); err != nil {
		return nil, err
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s%d-%s%s", secretFilePrefix, os.Getpid(), hex.EncodeToString(b), suffix))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a name pentym builds in its own directory
	if err != nil {
		return nil, err
	}
	sf := &SecretFile{Path: path}
	// Restrict before writing, so the secret is never readable by others.
	if err := restrict(path, false); err != nil {
		_ = f.Close()
		_ = sf.Remove()
		return nil, err
	}
	data := content.Expose()
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = sf.Remove()
		return nil, err
	}
	return sf, nil
}

// CleanStale deletes secret files left in dir by processes that no longer
// run, such as a pentym that crashed while a tool was running. It returns
// how many it deleted.
func CleanStale(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, secretFilePrefix) {
			continue
		}
		pidText, _, _ := strings.Cut(strings.TrimPrefix(name, secretFilePrefix), "-")
		pid, err := strconv.Atoi(pidText)
		if err == nil && pid != os.Getpid() && pidAlive(pid) {
			continue // in use by a running pentym
		}
		if err == nil && pid == os.Getpid() {
			continue // ours, still in use
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return n, err
		}
		n++
	}
	return n, nil
}

// PGPassEntry is one line of a .pgpass file.
type PGPassEntry struct {
	Host, Port, Database, User string
	Password                   secrets.Secret
}

// PGPass builds a .pgpass file: host:port:database:user:password per line,
// with ":" and "\" escaped. Empty fields match anything ("*").
func PGPass(entries ...PGPassEntry) secrets.Secret {
	var b []byte
	field := func(s string) {
		if s == "" {
			b = append(b, '*')
			return
		}
		b = appendEscaped(b, []byte(s))
	}
	for _, e := range entries {
		field(e.Host)
		b = append(b, ':')
		field(e.Port)
		b = append(b, ':')
		field(e.Database)
		b = append(b, ':')
		field(e.User)
		b = append(b, ':')
		b = appendEscaped(b, e.Password.Expose())
		b = append(b, '\n')
	}
	s := secrets.New(b)
	clear(b)
	return s
}

func appendEscaped(b, s []byte) []byte {
	for _, c := range s {
		if c == ':' || c == '\\' {
			b = append(b, '\\')
		}
		b = append(b, c)
	}
	return b
}

// MongoConfig builds a MongoDB database tools --config file holding the
// password: a YAML document with one double-quoted value.
func MongoConfig(password secrets.Secret) secrets.Secret {
	b := []byte(`password: "`)
	for _, c := range password.Expose() {
		switch {
		case c == '"' || c == '\\':
			b = append(b, '\\', c)
		case c < 0x20:
			b = append(b, fmt.Sprintf(`\x%02x`, c)...)
		default:
			b = append(b, c)
		}
	}
	b = append(b, "\"\n"...)
	s := secrets.New(b)
	clear(b)
	return s
}
