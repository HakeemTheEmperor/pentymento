// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package toolrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/devtools/leaktest"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

func TestWriteSecretFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	f, err := WriteSecretFile(dir, ".pgpass", secrets.New([]byte("db:5432:*:app:pw\n")))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(f.Path); err != nil || string(b) != "db:5432:*:app:pw\n" { // #nosec G304 -- test file
		t.Fatalf("content = %q, %v", b, err)
	}
	if !strings.HasPrefix(filepath.Base(f.Path), fmt.Sprintf("secret-%d-", os.Getpid())) || !strings.HasSuffix(f.Path, ".pgpass") {
		t.Errorf("name = %s", f.Path)
	}
	assertPrivate(t, f.Path)
	assertPrivate(t, dir)
	if err := f.Remove(); err != nil {
		t.Fatal(err)
	}
	if err := f.Remove(); err != nil {
		t.Errorf("second Remove = %v", err)
	}
	if _, err := WriteSecretFile("", "", secrets.New([]byte("x"))); err == nil {
		t.Error("WriteSecretFile without a directory succeeded")
	}
}

// TestSecretFilesRemoved: a run's secret files are deleted whether the
// tool succeeds, fails, is cancelled or is refused.
func TestSecretFilesRemoved(t *testing.T) {
	r, _ := newRunner(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tt := range []struct {
		name string
		cmd  func() *Cmd
		ctx  context.Context
	}{
		{"success", func() *Cmd { return helper("stderr", "ok") }, context.Background()},
		{"failure", func() *Cmd { return helper("exit", "2", "boom") }, context.Background()},
		{"cancelled", func() *Cmd { return helper("sleep") }, cancelled},
		{"refused", func() *Cmd {
			c := helper("stderr")
			c.Tool, c.Extra = "mongodump", []string{"--gzip"}
			return c
		}, context.Background()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, err := WriteSecretFile(r.SecretDir, ".conf", secrets.New([]byte("password: x\n")))
			if err != nil {
				t.Fatal(err)
			}
			c := tt.cmd()
			c.Files = []*SecretFile{f}
			_ = r.Run(tt.ctx, c)
			if _, err := os.Stat(f.Path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the secret file is still there: %v", err)
			}
		})
	}
}

func TestCleanStale(t *testing.T) {
	r, _ := newRunner(t)
	dir := r.SecretDir
	if err := secretDir(dir); err != nil {
		t.Fatal(err)
	}
	dead := deadPID(t)
	mine, err := WriteSecretFile(dir, "", secrets.New([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{ // name → kept
		fmt.Sprintf("secret-%d-aaaa", dead): false,
		"secret-notapid-bbbb":               false,
		"unrelated.txt":                     true,
	}
	for name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A file of a process that is still running is kept.
	live, liveDone := livePID(t)
	defer liveDone()
	liveFile := fmt.Sprintf("secret-%d-cccc", live)
	if err := os.WriteFile(filepath.Join(dir, liveFile), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	files[liveFile] = true

	n, err := CleanStale(dir)
	if err != nil || n != 2 {
		t.Errorf("CleanStale = %d, %v; want 2", n, err)
	}
	for name, kept := range files {
		if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != kept {
			t.Errorf("%s: kept = %v, want %v", name, err == nil, kept)
		}
	}
	if _, err := os.Stat(mine.Path); err != nil {
		t.Error("CleanStale removed this process's file")
	}
	if n, err := CleanStale(filepath.Join(dir, "missing")); n != 0 || err != nil {
		t.Errorf("CleanStale of a missing directory = %d, %v", n, err)
	}
}

// deadPID returns the PID of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	r, _ := newRunner(t)
	pidFile := filepath.Join(t.TempDir(), "pids")
	if err := r.Run(context.Background(), helper("tree", pidFile, "exit")); err != nil {
		t.Fatal(err)
	}
	child, grandchild := readPIDs(t, pidFile)
	waitDead(t, child, grandchild)
	return child
}

// livePID starts a helper that keeps running until the returned function
// is called.
func livePID(t *testing.T) (int, func()) {
	t.Helper()
	r, _ := newRunner(t)
	pidFile := filepath.Join(t.TempDir(), "pids")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = r.Run(ctx, helper("tree", pidFile, "wait"))
		close(done)
	}()
	child, _ := readPIDs(t, pidFile)
	return child, func() { cancel(); <-done }
}

func TestPGPass(t *testing.T) {
	got := PGPass(
		PGPassEntry{Host: "db.internal", Port: "5432", Database: "app", User: "pentym_backup", Password: secrets.New([]byte(`p:a\ss`))},
		PGPassEntry{User: "repl", Password: secrets.New([]byte("x"))},
	)
	want := "db.internal:5432:app:pentym_backup:p\\:a\\\\ss\n*:*:*:repl:x\n"
	if string(got.Expose()) != want {
		t.Errorf("PGPass = %q, want %q", got.Expose(), want)
	}
}

func TestMongoConfig(t *testing.T) {
	got := MongoConfig(secrets.New([]byte("pa\"ss\\w\n")))
	if want := "password: \"pa\\\"ss\\\\w\\x0a\"\n"; string(got.Expose()) != want {
		t.Errorf("MongoConfig = %q, want %q", got.Expose(), want)
	}
}

// TestSecretLeakToolrun hands canary passwords to a child through secret
// files, the way the engines will, and checks that they reach the child
// only through the file: never its arguments, environment, logs or
// errors.
func TestSecretLeakToolrun(t *testing.T) {
	canaries := leaktest.New(t, 2)
	r, logs := newRunner(t)
	pw := secrets.New([]byte(canaries[0]))
	mpw := secrets.New([]byte(canaries[1]))
	var outputs []string

	pgpass, err := WriteSecretFile(r.SecretDir, ".pgpass", PGPass(PGPassEntry{Host: "db", User: "app", Password: pw}))
	if err != nil {
		t.Fatal(err)
	}
	mongo, err := WriteSecretFile(r.SecretDir, ".yaml", MongoConfig(mpw))
	if err != nil {
		t.Fatal(err)
	}
	// The child can read the file it is pointed at.
	var out strings.Builder
	c := helper("cat-env-file", "PGPASSFILE")
	c.Env = append(c.Env, "PGPASSFILE="+pgpass.Path)
	c.Secrets, c.Stdout = []secrets.Secret{pw}, &out
	if err := r.Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), string(canaries[0])) {
		t.Fatal("the child could not read its .pgpass file")
	}
	// Its arguments and environment hold only the files' paths.
	rec := filepath.Join(t.TempDir(), "rec")
	c = helper("record", rec, "--config", mongo.Path)
	c.Env = append(c.Env, "PGPASSFILE="+pgpass.Path)
	c.Secrets, c.Files = []secrets.Secret{pw, mpw}, []*SecretFile{pgpass, mongo}
	if err := r.Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(rec) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}
	var seen map[string][]string
	if err := json.Unmarshal(b, &seen); err != nil {
		t.Fatal(err)
	}
	if len(seen["args"]) == 0 || len(seen["env"]) == 0 {
		t.Fatal("the child recorded nothing")
	}
	outputs = append(outputs, strings.Join(seen["args"], "\n"), strings.Join(seen["env"], "\n"))
	// A failing tool that prints the password, and a refused command.
	c = helper("exit", "1", "auth failed for {SECRET}")
	c.Env, c.Secrets, c.AllowEnvSecret = append(c.Env, "TOOLRUN_SECRET="+string(canaries[0])), []secrets.Secret{pw}, true
	if err := r.Run(context.Background(), c); err != nil {
		outputs = append(outputs, err.Error())
	}
	c = helper("record", rec, "--password="+string(canaries[0]))
	c.Secrets = []secrets.Secret{pw}
	if err := r.Run(context.Background(), c); err != nil {
		outputs = append(outputs, err.Error())
	}
	outputs = append(outputs, logs.String())
	leaktest.Scan(t, "tool runner", canaries, outputs...)
}
