// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// Config is the pentym configuration. Keys are added by the chunks that use
// them; unknown keys are rejected.
type Config struct {
	// Include lists config files applied before this one.
	Include []string `yaml:"include"`
	// AllowPlaintextSecrets permits secret values written directly in the
	// config, provided the file is not readable by group or others.
	AllowPlaintextSecrets bool `yaml:"allow_plaintext_secrets"`
	// Repository is where backups are stored.
	Repository Repository `yaml:"repository"`
	// InstanceProvider lists the side-instance providers to try, in order:
	// "docker" and "local". Empty means DefaultInstanceProviders.
	InstanceProvider []string `yaml:"instance_provider"`
	// WorkDir holds side-instance files and temporary credential files.
	// Empty means DefaultWorkDir.
	WorkDir string `yaml:"work_dir"`
	// Tools are directories searched for database binaries before PATH.
	Tools Tools `yaml:"tools"`
	// Targets are the configured databases, by name.
	Targets map[string]Target `yaml:"targets"`
}

// Repository is the repository location and how to unlock it.
type Repository struct {
	// Path is the directory of a repository on local storage. It must be
	// absolute.
	Path string `yaml:"path"`
	// Key is a reference to the passphrase, recovery code or age identity
	// that unlocks a key slot when there is no terminal to ask in.
	Key secrets.Ref `yaml:"key"`
}

// Tools lists, per engine, directories holding its binaries (postgres,
// pg_ctl, pg_dump …; mongod, mongodump …). Several can be listed, such as
// one per Postgres major version; the version check picks the match.
type Tools struct {
	Postgres []string `yaml:"postgres"`
	MongoDB  []string `yaml:"mongodb"`
}

// Instance providers.
const (
	ProviderDocker = "docker"
	ProviderLocal  = "local"
)

// DefaultInstanceProviders is the provider order when none is configured
// (PRD 7.6).
var DefaultInstanceProviders = []string{ProviderDocker, ProviderLocal}

// Providers returns the configured provider order, or the default.
func (c *Config) Providers() []string {
	if len(c.InstanceProvider) == 0 {
		return append([]string(nil), DefaultInstanceProviders...)
	}
	return append([]string(nil), c.InstanceProvider...)
}

// DefaultWorkDir returns the work directory used when work_dir is not
// set: pentym/work in the user's cache directory.
func DefaultWorkDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("no work_dir configured and no cache directory: %w", err)
	}
	return filepath.Join(dir, "pentym", "work"), nil
}

// WorkDirPath returns work_dir, or the default.
func (c *Config) WorkDirPath() (string, error) {
	if c.WorkDir != "" {
		return c.WorkDir, nil
	}
	return DefaultWorkDir()
}

// Target is one configured database.
type Target struct {
	// Engine is postgres or mongodb.
	Engine string `yaml:"engine"`

	// Connection (Postgres). Host is a host name or a Unix socket
	// directory; empty means the libpq default.
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Database string `yaml:"database"`
	// SSLMode is a libpq sslmode: disable, allow, prefer (the default),
	// require, verify-ca or verify-full.
	SSLMode     string `yaml:"sslmode"`
	SSLRootCert string `yaml:"sslrootcert"`
	SSLCert     string `yaml:"sslcert"`
	SSLKey      string `yaml:"sslkey"`

	// URI is a MongoDB target's connection string, such as
	// mongodb://db1:27017,db2:27017/?replicaSet=rs0&tls=true. It holds no
	// user name or password: those come from Credentials. Empty means
	// mongodb://localhost:27017.
	URI string `yaml:"uri"`

	// Credentials are the roles pentym connects as, one per purpose
	// (PRD 8.3).
	Credentials Credentials `yaml:"credentials"`
}

// Credentials are a target's roles by purpose. Replication (capture) and
// restore credentials are added by the chunks that use them.
type Credentials struct {
	// Backup takes base backups.
	Backup Credential `yaml:"backup"`
}

// Credential is a role and, optionally, a reference to its password;
// certificates, peer authentication and .pgpass need none.
type Credential struct {
	User     string      `yaml:"user"`
	Password secrets.Ref `yaml:"password"`
}

// SSLModes are the accepted sslmode values.
var SSLModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

// Engines are the supported engine names.
var Engines = []string{"postgres", "mongodb"}

// targetNameRE is the form of a target name: it is typed in commands and
// mapped to environment-variable names.
var targetNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// EnvConfigPath names the environment variable that points to the config
// file when --config is not given.
const EnvConfigPath = "PENTYM_CONFIG"

// DefaultPath returns the config file location for an operating system
// (PRD 7.14): /etc/pentym/config.yaml on Linux and other Unix systems,
// /usr/local/etc/pentym/config.yaml on macOS, and
// %ProgramData%\pentym\config.yaml on Windows.
func DefaultPath(goos string, getenv func(string) string) string {
	switch goos {
	case "darwin":
		return "/usr/local/etc/pentym/config.yaml"
	case "windows":
		pd := getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return pd + `\pentym\config.yaml`
	}
	return "/etc/pentym/config.yaml"
}

// Path returns the config file to use: flag if set, else $PENTYM_CONFIG,
// else DefaultPath for this system.
func Path(flag string, getenv func(string) string) string {
	if flag != "" {
		return flag
	}
	if p := getenv(EnvConfigPath); p != "" {
		return p
	}
	return filepath.FromSlash(DefaultPath(runtime.GOOS, getenv))
}

// Load reads the config file at path with its includes and environment
// overrides, and validates it. Errors are *msg.Error values with exit code
// ExitConfig.
func Load(path string, environ []string) (*Config, error) {
	c, src, err := load[Config](path, environ)
	if err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, invalid(src.main, 0, err.Error())
	}
	return c, nil
}

func (c *Config) validate() error {
	if p := c.Repository.Path; p != "" && !filepath.IsAbs(p) {
		return fmt.Errorf("repository.path must be an absolute path, not %q", p)
	}
	if c.WorkDir != "" && !filepath.IsAbs(c.WorkDir) {
		return fmt.Errorf("work_dir must be an absolute path, not %q", c.WorkDir)
	}
	seenProvider := map[string]bool{}
	for _, p := range c.InstanceProvider {
		if p != ProviderDocker && p != ProviderLocal {
			return fmt.Errorf("instance_provider: %q is not a provider; use docker and/or local", p)
		}
		if seenProvider[p] {
			return fmt.Errorf("instance_provider lists %s twice", p)
		}
		seenProvider[p] = true
	}
	for _, t := range []struct {
		engine string
		dirs   []string
	}{{"postgres", c.Tools.Postgres}, {"mongodb", c.Tools.MongoDB}} {
		for _, d := range t.dirs {
			if !filepath.IsAbs(d) {
				return fmt.Errorf("tools.%s: %q must be an absolute path", t.engine, d)
			}
		}
	}
	names := make([]string, 0, len(c.Targets))
	for name := range c.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := map[string]string{}
	for _, name := range names {
		if !targetNameRE.MatchString(name) {
			return fmt.Errorf("target name %q must be lowercase letters, digits, - and _, starting with a letter or digit, at most 63 characters", name)
		}
		if other, ok := seen[normalize(name)]; ok {
			return fmt.Errorf("target names %q and %q differ only in - and _, so environment overrides cannot tell them apart", other, name)
		}
		seen[normalize(name)] = name
		t := c.Targets[name]
		if t.Engine == "" {
			return fmt.Errorf("target %s has no engine; set engine: postgres or engine: mongodb", name)
		}
		if !contains(Engines, t.Engine) {
			return fmt.Errorf("target %s: engine must be %s, not %q", name, strings.Join(Engines, " or "), t.Engine)
		}
		if err := t.validateConnection(name); err != nil {
			return err
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// validateConnection checks a target's connection settings: Postgres
// targets use host, port, database and the ssl settings; MongoDB targets
// use uri.
func (t Target) validateConnection(name string) error {
	if t.Engine == "mongodb" {
		for _, f := range [][2]string{{"host", t.Host}, {"database", t.Database}, {"sslmode", t.SSLMode},
			{"sslrootcert", t.SSLRootCert}, {"sslcert", t.SSLCert}, {"sslkey", t.SSLKey}} {
			if f[1] != "" {
				return fmt.Errorf("target %s: %s is for postgres targets; a mongodb target sets its connection in uri", name, f[0])
			}
		}
		if t.Port != 0 {
			return fmt.Errorf("target %s: port is for postgres targets; a mongodb target sets its connection in uri", name)
		}
		return validateMongoURI(name, t.URI)
	}
	if t.URI != "" {
		return fmt.Errorf("target %s: uri is for mongodb targets; a postgres target uses host, port and database", name)
	}
	if t.Port < 0 || t.Port > 65535 {
		return fmt.Errorf("target %s: port %d is not a TCP port", name, t.Port)
	}
	if t.SSLMode != "" && !contains(SSLModes, t.SSLMode) {
		return fmt.Errorf("target %s: sslmode must be one of %s, not %q", name, strings.Join(SSLModes, ", "), t.SSLMode)
	}
	for _, f := range [][2]string{{"sslrootcert", t.SSLRootCert}, {"sslcert", t.SSLCert}, {"sslkey", t.SSLKey}} {
		if f[1] != "" && !filepath.IsAbs(f[1]) {
			return fmt.Errorf("target %s: %s must be an absolute path, not %q", name, f[0], f[1])
		}
	}
	return nil
}

// validateMongoURI checks the form of a MongoDB connection string without
// resolving it (a mongodb+srv URI needs DNS, which waits for connecting),
// and refuses one that holds credentials: the config holds secrets only
// as references. Errors never quote the URI.
func validateMongoURI(name, uri string) error {
	if uri == "" {
		return nil
	}
	rest, ok := strings.CutPrefix(uri, "mongodb://")
	if !ok {
		if rest, ok = strings.CutPrefix(uri, "mongodb+srv://"); !ok {
			return fmt.Errorf("target %s: uri must start with mongodb:// or mongodb+srv://", name)
		}
	}
	authority, query := rest, ""
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		authority = rest[:i]
		if _, q, found := strings.Cut(rest[i:], "?"); found {
			query = q
		}
	}
	if strings.Contains(authority, "@") {
		return fmt.Errorf("target %s: uri must not hold a user name or password; set credentials.backup.user, and its password as a secret reference", name)
	}
	if authority == "" {
		return fmt.Errorf("target %s: uri names no host", name)
	}
	for _, kv := range strings.Split(query, "&") {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToLower(k) {
		case "tlscertificatekeyfilepassword", "sslpemkeypassword":
			return fmt.Errorf("target %s: uri must not hold a password (%s)", name, k)
		}
	}
	return nil
}
