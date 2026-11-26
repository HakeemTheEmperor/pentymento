// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/devtools/leaktest"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// The test binary doubles as a fake pg_basebackup when fakeEnv is set.
const fakeEnv = "PENTYM_PG_FAKE"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeEnv); mode != "" {
		fakePGBasebackup(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakePGBasebackup records its arguments, environment and .pgpass file
// in PENTYM_PG_FAKE_LOG, then behaves as mode says.
func fakePGBasebackup(mode string) {
	pgpass, _ := os.ReadFile(os.Getenv("PGPASSFILE")) // #nosec G304 G703 -- test fake
	b, _ := json.Marshal(map[string]any{"args": os.Args[1:], "env": os.Environ(), "pgpass": string(pgpass)})
	_ = os.WriteFile(os.Getenv("PENTYM_PG_FAKE_LOG"), b, 0o600) // #nosec G703 -- test fake
	switch mode {
	case "ok":
		fmt.Fprintln(os.Stderr, "pg_basebackup: initiating base backup, waiting for checkpoint to complete")
		fmt.Fprintln(os.Stderr, "pg_basebackup: write-ahead log start point: 0/2000028 on timeline 1")
		_, _ = os.Stdout.WriteString("fake tar content")
		fmt.Fprintln(os.Stderr, "pg_basebackup: write-ahead log end point: 0/2000100")
		fmt.Fprintln(os.Stderr, "pg_basebackup: base backup completed")
	case "tablespaces":
		fmt.Fprintln(os.Stderr, "pg_basebackup: error: can only write single tablespace to stdout, database has 2")
		os.Exit(1)
	case "silent":
		_, _ = os.Stdout.WriteString("tar")
	}
}

// memSink keeps each archive in memory, in order.
type memSink struct {
	names []string
	data  map[string]*bytes.Buffer
}

func (s *memSink) Archive(_ context.Context, name string) (io.Writer, error) {
	if s.data == nil {
		s.data = map[string]*bytes.Buffer{}
	}
	s.names = append(s.names, name)
	s.data[name] = &bytes.Buffer{}
	return s.data[name], nil
}

func TestConninfo(t *testing.T) {
	tg := config.Target{Host: "db.internal", Port: 5433, SSLMode: "verify-full", SSLRootCert: `/etc/it's\ca.pem`}
	got := conninfo(tg, config.Credential{User: "pentym_backup"}, "postgres")
	want := `host='db.internal' port='5433' dbname='postgres' user='pentym_backup' sslmode='verify-full' sslrootcert='/etc/it\'s\\ca.pem' application_name='pentym'`
	if got != want {
		t.Errorf("conninfo =\n%s\nwant\n%s", got, want)
	}
	// Quoted values survive a round trip through pgconn's parser.
	odd := config.Target{Host: "db", SSLMode: "disable"}
	cfg, err := pgconn.ParseConfig(conninfo(odd, config.Credential{User: `o'brien\x y`}, `my db`))
	if err != nil || cfg.User != `o'brien\x y` || cfg.Database != "my db" || cfg.Host != "db" {
		t.Errorf("parsed = %+v, %v", cfg, err)
	}
	if got := conninfo(config.Target{}, config.Credential{User: "u"}, ""); got != `user='u' application_name='pentym'` {
		t.Errorf("minimal conninfo = %s", got)
	}
}

func TestFallbackWorthy(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want bool
	}{
		{errors.New("unexpected message type"), true},
		{errors.Join(errConnect, errors.New("refused")), false},
		{errors.Join(errUnsupported, errors.New("15")), false},
		{fmt.Errorf("BASE_BACKUP: %w", &pgconn.PgError{Code: "28000"}), false},
		{fmt.Errorf("BASE_BACKUP: %w", &pgconn.PgError{Code: "42601"}), true},
	} {
		if got := fallbackWorthy(tt.err); got != tt.want {
			t.Errorf("fallbackWorthy(%v) = %v", tt.err, got)
		}
	}
}

func TestConnectError(t *testing.T) {
	tg := engine.Target{Name: "prod-pg", Config: config.Target{Credentials: config.Credentials{Backup: config.Credential{User: "pentym_backup"}}}}
	pw := secrets.New([]byte("hunter2-secret"))
	for _, tt := range []struct {
		err         error
		replication bool
		id          msg.ID
		code        msg.ExitCode
	}{
		{&pgconn.PgError{Code: "28000", Message: "no pg_hba.conf entry for replication"}, true, IDReplicationHBA, msg.ExitAccess},
		{&pgconn.PgError{Code: "28P01", Message: "password authentication failed"}, false, IDConnect, msg.ExitAccess},
		{errors.New("dial tcp: connection refused (password hunter2-secret)"), false, IDConnect, msg.ExitConnect},
	} {
		var me *msg.Error
		err := connectError(tg, "pentym_backup", tt.replication, tt.err, pw)
		if !errors.As(err, &me) || me.Message.ID != tt.id || me.Code != tt.code || me.Message.Next == "" {
			t.Errorf("connectError(%v) = %+v", tt.err, me)
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(me.Message.Cause, "hunter2") {
			t.Errorf("the password is in the error: %v / %s", err, me.Message.Cause)
		}
	}
}

func TestBackupCredRequired(t *testing.T) {
	_, err := backupCred(engine.Target{Name: "prod-pg"})
	var me *msg.Error
	if !errors.As(err, &me) || me.Code != msg.ExitConfig || !strings.Contains(me.Message.Fact, "credentials.backup.user") {
		t.Errorf("backupCred without a user = %v", err)
	}
}

// fakeEngine returns an engine whose pg_basebackup is the fake, run in
// mode, with its record written to the returned log path.
func fakeEngine(t *testing.T, mode string, env map[string]string) (*Engine, string) {
	t.Helper()
	self, _ := os.Executable()
	dir := t.TempDir()
	b, err := os.ReadFile(self) // #nosec G304 -- the test binary
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, toolrun.ExeName("pg_basebackup")), b, 0o700); err != nil { // #nosec G306 G703 -- an executable
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "log.json")
	r := &toolrun.Runner{
		SecretDir: filepath.Join(t.TempDir(), "secrets"),
		Environ:   func() []string { return append(os.Environ(), fakeEnv+"="+mode, "PENTYM_PG_FAKE_LOG="+log) },
	}
	e := &Engine{
		Resolver: secrets.Resolver{Getenv: func(k string) string { return env[k] }},
		Runner:   r, ToolDirs: []string{dir}, RunID: "run1",
		LookPath: func(string) (string, error) { return "", errors.New("not on PATH") },
	}
	return e, log
}

func pgTarget(pw string) engine.Target {
	ref, _ := secrets.ParseRef(pw)
	return engine.Target{Name: "prod-pg", Config: config.Target{
		Engine: "postgres", Host: "127.0.0.1", Port: 1, SSLMode: "disable",
		Credentials: config.Credentials{Backup: config.Credential{User: "pentym_backup", Password: ref}},
	}}
}

func TestPGBasebackupFallback(t *testing.T) {
	canary := leaktest.New(t, 1)[0]
	e, log := fakeEngine(t, "ok", map[string]string{"PG_PW": string(canary)})
	sink := &memSink{}
	bb, err := e.pgBasebackup(context.Background(), pgTarget("env:PG_PW"), sink)
	if err != nil {
		t.Fatal(err)
	}
	if bb.Method != "pg_basebackup" || bb.Start != "0/2000028" || bb.End != "0/2000100" || bb.Timeline != "1" {
		t.Errorf("base backup = %+v", bb)
	}
	if len(sink.names) != 1 || sink.names[0] != BaseArchive || sink.data[BaseArchive].String() != "fake tar content" {
		t.Errorf("archives = %v", sink.names)
	}
	b, err := os.ReadFile(log) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Args   []string `json:"args"`
		Env    []string `json:"env"`
		PGPass string   `json:"pgpass"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(rec.Args, " ")
	for _, want := range []string{"--pgdata=-", "--format=tar", "--wal-method=fetch", "--no-manifest",
		"--dbname=host='127.0.0.1' port='1' user='pentym_backup' sslmode='disable'"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q: %s", want, joined)
		}
	}
	if rec.PGPass != "127.0.0.1:1:replication:pentym_backup:"+string(canary)+"\n" {
		t.Errorf(".pgpass = %q", rec.PGPass)
	}
	leaktest.Scan(t, "pg_basebackup arguments and environment", []leaktest.Canary{canary}, joined, strings.Join(rec.Env, "\n"))
	if entries, _ := os.ReadDir(e.Runner.SecretDir); len(entries) != 0 {
		t.Errorf("secret files left: %v", entries)
	}
}

func TestPGBasebackupFailures(t *testing.T) {
	e, _ := fakeEngine(t, "tablespaces", nil)
	_, err := e.pgBasebackup(context.Background(), pgTarget(""), &memSink{})
	var ee *toolrun.ExitError
	if !errors.As(err, &ee) || !strings.Contains(ee.Error(), "can only write single tablespace") {
		t.Errorf("with tablespaces = %v", err)
	}
	e, _ = fakeEngine(t, "silent", nil)
	if _, err := e.pgBasebackup(context.Background(), pgTarget(""), &memSink{}); err == nil || !strings.Contains(err.Error(), "did not report") {
		t.Errorf("without WAL points = %v", err)
	}
	e, _ = fakeEngine(t, "ok", nil)
	e.ToolDirs = nil
	if _, err := e.pgBasebackup(context.Background(), pgTarget(""), &memSink{}); err == nil || err.Error() != "pg_basebackup is not in tools.postgres or on PATH" {
		t.Errorf("without the tool = %v", err)
	}
}

func TestArchiveNames(t *testing.T) {
	for name, ok := range map[string]bool{"16384.tar": true, "1.tar": true, "base.tar": false, "../x.tar": false, "16384.tar.gz": false} {
		if tablespaceArchiveRE.MatchString(name) != ok {
			t.Errorf("%q: match = %v", name, !ok)
		}
	}
}
