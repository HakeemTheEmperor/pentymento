// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package mongodb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/devtools/leaktest"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// The test binary doubles as a fake mongodump when fakeEnv is set.
const fakeEnv = "PENTYM_MONGODUMP_FAKE"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeEnv); mode != "" {
		fakeMongodump(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeMongodump records its arguments, environment and --config file in
// PENTYM_MONGODUMP_FAKE_LOG, then behaves as mode says.
func fakeMongodump(mode string) {
	var conf []byte
	for _, a := range os.Args[1:] {
		if p, ok := strings.CutPrefix(a, "--config="); ok {
			conf, _ = os.ReadFile(p) // #nosec G304 G703 -- test fake
		}
	}
	var stdin []byte
	if strings.HasPrefix(mode, "restore") {
		stdin, _ = io.ReadAll(os.Stdin)
	}
	b, _ := json.Marshal(map[string]any{"args": os.Args[1:], "env": os.Environ(), "config": string(conf), "stdin": stdin})
	_ = os.WriteFile(os.Getenv("PENTYM_MONGODUMP_FAKE_LOG"), b, 0o600) // #nosec G703 -- test fake
	switch mode {
	case "restore-fail":
		fmt.Fprintln(os.Stderr, "Failed: app.orders: error restoring from archive: E11000 duplicate key")
		os.Exit(1)
	case "version":
		fmt.Println("mongodump version: 100.10.0")
	case "legacy":
		fmt.Println("mongodump version: r4.4.29")
	case "ok":
		_, _ = os.Stdout.Write(sampleArchive())
		fmt.Fprintln(os.Stderr, "2026-11-26T10:00:00.000+0000	writing app.orders to archive on stdout")
	case "garbage":
		_, _ = os.Stdout.WriteString("this is not an archive")
	case "fail":
		_, _ = os.Stdout.Write(sampleArchive()[:100])
		fmt.Fprintln(os.Stderr, "Failed: error writing data for collection `app.orders` to disk: connection reset")
		os.Exit(1)
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

// fakeEngine returns an engine whose mongodump is the fake, run in mode,
// with its record written to the returned log path.
func fakeEngine(t *testing.T, mode string, env map[string]string) (*Engine, string) {
	t.Helper()
	self, _ := os.Executable()
	dir := t.TempDir()
	b, err := os.ReadFile(self) // #nosec G304 -- the test binary
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"mongodump", "mongorestore"} {
		if err := os.WriteFile(filepath.Join(dir, toolrun.ExeName(tool)), b, 0o700); err != nil { // #nosec G306 G703 -- an executable
			t.Fatal(err)
		}
	}
	log := filepath.Join(t.TempDir(), "log.json")
	r := &toolrun.Runner{
		SecretDir: filepath.Join(t.TempDir(), "secrets"),
		Environ:   func() []string { return append(os.Environ(), fakeEnv+"="+mode, "PENTYM_MONGODUMP_FAKE_LOG="+log) },
	}
	e := &Engine{
		Resolver: secrets.Resolver{Getenv: func(k string) string { return env[k] }},
		Runner:   r, ToolDirs: []string{dir}, RunID: "run1",
		LookPath: func(string) (string, error) { return "", errors.New("not on PATH") },
	}
	return e, log
}

func mgTarget(uri, pw string) engine.Target {
	ref, _ := secrets.ParseRef(pw)
	return engine.Target{Name: "prod-mg", Config: config.Target{
		Engine: "mongodb", URI: uri,
		Credentials: config.Credentials{Backup: config.Credential{User: "pentym_backup", Password: ref}},
	}}
}

type fakeRecord struct {
	Args   []string `json:"args"`
	Env    []string `json:"env"`
	Config string   `json:"config"`
	Stdin  []byte   `json:"stdin"`
}

func readRecord(t *testing.T, log string) fakeRecord {
	t.Helper()
	b, err := os.ReadFile(log) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}
	var rec fakeRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestMongodump(t *testing.T) {
	canary := leaktest.New(t, 1)[0]
	e, log := fakeEngine(t, "ok", map[string]string{"MG_PW": string(canary)})
	sink := &memSink{}
	uri := "mongodb://db1:27017,db2:27017/?replicaSet=rs0"
	sum, err := e.mongodump(context.Background(), mgTarget(uri, "env:MG_PW"), sink, true)
	if err != nil {
		t.Fatal(err)
	}
	if sum.OplogEntries != 3 || Position(sum.OplogFirst) != "1732600000:1" || Position(sum.OplogLast) != "1732600005:1" {
		t.Errorf("summary = %+v", sum)
	}
	if len(sink.names) != 1 || sink.names[0] != ArchiveName || !bytes.Equal(sink.data[ArchiveName].Bytes(), sampleArchive()) {
		t.Errorf("archives = %v", sink.names)
	}
	rec := readRecord(t, log)
	if len(rec.Args) != 5 || rec.Args[0] != "--uri="+uri || rec.Args[1] != "--username=pentym_backup" ||
		!strings.HasPrefix(rec.Args[2], "--config=") || rec.Args[3] != "--archive" || rec.Args[4] != "--oplog" {
		t.Errorf("args = %q", rec.Args)
	}
	if rec.Config != `password: "`+string(canary)+`"`+"\n" {
		t.Errorf("--config file = %q", rec.Config)
	}
	leaktest.Scan(t, "mongodump arguments and environment", []leaktest.Canary{canary}, strings.Join(rec.Args, " "), strings.Join(rec.Env, "\n"))
	if entries, _ := os.ReadDir(e.Runner.SecretDir); len(entries) != 0 {
		t.Errorf("secret files left: %v", entries)
	}
	for _, a := range rec.Args {
		if a == "--gzip" {
			t.Error("mongodump was asked to compress")
		}
	}
}

func TestMongodumpStandaloneNoPassword(t *testing.T) {
	e, log := fakeEngine(t, "ok", nil)
	if _, err := e.mongodump(context.Background(), mgTarget("", ""), &memSink{}, false); err != nil {
		t.Fatal(err)
	}
	rec := readRecord(t, log)
	if !slices.Equal(rec.Args, []string{"--uri=" + defaultURI, "--username=pentym_backup", "--archive"}) {
		t.Errorf("args = %q", rec.Args)
	}
	// A client certificate needs no user or password.
	tg := mgTarget("mongodb://db/?tls=true&authMechanism=MONGODB-X509&tlsCertificateKeyFile=/etc/pentym/client.pem", "")
	tg.Config.Credentials.Backup.User = ""
	if _, err := e.mongodump(context.Background(), tg, &memSink{}, true); err != nil {
		t.Fatal(err)
	}
	if rec := readRecord(t, log); len(rec.Args) != 3 || rec.Args[1] != "--archive" {
		t.Errorf("x.509 args = %q", rec.Args)
	}
}

func TestMongodumpFailures(t *testing.T) {
	e, _ := fakeEngine(t, "fail", nil)
	_, err := e.mongodump(context.Background(), mgTarget("", ""), &memSink{}, true)
	var ee *toolrun.ExitError
	if !errors.As(err, &ee) || !strings.Contains(ee.Error(), "connection reset") {
		t.Errorf("a failed dump = %v", err)
	}
	e, _ = fakeEngine(t, "garbage", nil)
	if _, err := e.mongodump(context.Background(), mgTarget("", ""), &memSink{}, true); err == nil ||
		!strings.Contains(err.Error(), "the mongodump archive does not parse: not a mongodump archive") {
		t.Errorf("an unparseable dump = %v", err)
	}
	e, _ = fakeEngine(t, "ok", nil)
	e.ToolDirs = nil
	if _, err := e.mongodump(context.Background(), mgTarget("", ""), &memSink{}, true); err == nil || err.Error() != "mongodump is not in tools.mongodb or on PATH" {
		t.Errorf("without the tool = %v", err)
	}
}

func TestToolFindings(t *testing.T) {
	e, _ := fakeEngine(t, "version", nil)
	if f := e.toolFindings(context.Background()); len(f) != 0 {
		t.Errorf("Database Tools 100: %+v", f)
	}
	e, _ = fakeEngine(t, "legacy", nil)
	if f := e.toolFindings(context.Background()); len(f) != 1 || f[0].Level != msg.LevelFailed || !strings.Contains(f[0].Fact, "legacy server tools") {
		t.Errorf("legacy tools: %+v", f)
	}
	e.ToolDirs = nil
	if f := e.toolFindings(context.Background()); len(f) != 1 || f[0].Fact != "mongodump is not in tools.mongodb or on PATH" {
		t.Errorf("missing: %+v", f)
	}
}

func TestBackupCred(t *testing.T) {
	tg := mgTarget("", "")
	tg.Config.Credentials.Backup.User = ""
	_, err := backupCred(tg)
	var me *msg.Error
	if !errors.As(err, &me) || me.Code != msg.ExitConfig || !strings.Contains(me.Message.Fact, "credentials.backup.user") {
		t.Errorf("without a user = %v", err)
	}
	tg.Config.URI = "mongodb://db/?authMechanism=mongodb-x509&tls=true"
	if _, err := backupCred(tg); err != nil {
		t.Errorf("x.509 without a user = %v", err)
	}
}

func TestConnectError(t *testing.T) {
	tg := mgTarget("", "")
	pw := secrets.New([]byte("hunter2-secret"))
	for _, tt := range []struct {
		err  error
		code msg.ExitCode
	}{
		{mongo.CommandError{Code: 18, Message: "Authentication failed."}, msg.ExitAccess},
		{errors.New("connection() error occurred during connection handshake: auth error: sasl conversation error"), msg.ExitAccess},
		{errors.New("server selection error: context deadline exceeded (mongodb://u:hunter2-secret@db)"), msg.ExitConnect},
	} {
		var me *msg.Error
		err := connectError(tg, "pentym_backup", tt.err, pw)
		if !errors.As(err, &me) || me.Message.ID != IDConnect || me.Code != tt.code || me.Message.Fact != "cannot connect to MongoDB as pentym_backup" {
			t.Errorf("connectError(%v) = %+v", tt.err, me)
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(me.Message.Cause, "hunter2") {
			t.Errorf("the password is in the error: %v / %s", err, me.Message.Cause)
		}
	}
}

func TestSupported(t *testing.T) {
	for v, want := range map[string]bool{"7.0.14": true, "8.0.4": true, "9.0.0-rc1": true, "6.0.19": false, "8.2.1": false, "": false} {
		if supported(v) != want {
			t.Errorf("supported(%q) = %v", v, !want)
		}
	}
}

func strp(s string) *string { return &s }

func TestPrivilegeFindings(t *testing.T) {
	backup := []roleRef{{"backup", "admin"}, {"read", "local"}}
	findAll := privilege{Actions: []string{"listCollections", "find"}}
	findAll.Resource.DB, findAll.Resource.Collection = strp(""), strp("")
	findOne := privilege{Actions: []string{"find"}}
	findOne.Resource.DB, findOne.Resource.Collection = strp("app"), strp("")
	for _, tt := range []struct {
		name   string
		roles  []roleRef
		privs  []privilege
		denied bool
		want   []msg.ID
	}{
		{"minimal", backup, nil, false, nil},
		{"custom role with find everywhere", []roleRef{{"dumper", "admin"}}, []privilege{findAll}, false, nil},
		{"read on one database", []roleRef{{"read", "app"}}, []privilege{findOne}, false, []msg.ID{IDPrivilege}},
		{"no oplog access", []roleRef{{"backup", "admin"}}, nil, true, []msg.ID{IDPrivilege}},
		{"root", []roleRef{{"root", "admin"}}, nil, false, []msg.ID{IDTooMuchPrivilege}},
		{"backup and readWrite", append(backup, roleRef{"readWrite", "app"}), nil, false, []msg.ID{IDTooMuchPrivilege}},
	} {
		got := privilegeFindings("pentym_backup", tt.roles, tt.privs, tt.denied)
		var ids []msg.ID
		for _, f := range got {
			ids = append(ids, f.ID)
		}
		if !slices.Equal(ids, tt.want) {
			t.Errorf("%s: findings = %+v", tt.name, got)
		}
	}
	f := privilegeFindings("pentym_backup", []roleRef{{"root", "admin"}, {"readWrite", "app"}}, nil, false)
	if len(f) != 1 || f[0].Level != msg.LevelWarning || f[0].Fact != "pentym_backup has readWrite@app, root@admin, which is more privilege than backups need" {
		t.Errorf("too much = %+v", f)
	}
}

func TestEngineRegistered(t *testing.T) {
	e, err := engine.New("mongodb", engine.Env{Tools: config.Tools{MongoDB: []string{"/opt/mongodb/bin"}}})
	if err != nil {
		t.Fatal(err)
	}
	m := e.(*Engine)
	if m.Name() != "mongodb" || !slices.Equal(m.ToolDirs, []string{"/opt/mongodb/bin"}) || !m.Capabilities().PITR || m.Capabilities().PITRNote != "replica sets only" {
		t.Errorf("engine = %+v", m)
	}
}
