// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/local"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/devtools/leaktest"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

var fastKDF = crypto.KDFParams{Name: "argon2id", MemoryKiB: 8 << 10, Time: 1, Threads: 1}

// fixture is a new encrypted repository on a local backend.
type fixture struct {
	dir      string
	be       *local.Backend
	repo     *Repository
	material *crypto.Material
	full     *crypto.Slot // passphrase "admin"
	agent    *crypto.Slot // passphrase "agent"
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	be, err := local.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := crypto.Generate()
	if err != nil {
		t.Fatal(err)
	}
	full, err := crypto.NewPassphraseSlot("admin", m.Unlocked(), secrets.New([]byte("admin")), fastKDF)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := crypto.NewPassphraseSlot("agent", m.Unlocked().Agent(), secrets.New([]byte("agent")), fastKDF)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Create(context.Background(), be, NewConfig(false, time.Now()), m, []*crypto.Slot{full, agent})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{dir: dir, be: be, repo: r, material: m, full: full, agent: agent}
}

// reopen opens the repository afresh and unlocks it with a slot.
func (f *fixture) reopen(t *testing.T, slot *crypto.Slot, pass string) *Repository {
	t.Helper()
	r, err := Open(context.Background(), f.be)
	if err != nil {
		t.Fatal(err)
	}
	u, err := slot.UnlockPassphrase(secrets.New([]byte(pass)))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Unlock(context.Background(), *u); err != nil {
		t.Fatal(err)
	}
	return r
}

func wantMsgID(t *testing.T, err error, id msg.ID) {
	t.Helper()
	var me *msg.Error
	if !errors.As(err, &me) || me.Message.ID != id {
		t.Fatalf("error = %v, want message %s", err, id)
	}
}

func TestCreateAndOpen(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r, err := Open(ctx, f.be)
	if err != nil {
		t.Fatal(err)
	}
	c := r.Config()
	if c.Version != FormatVersion || len(c.ID) != 32 || c.Insecure() || c.Chunker.Avg != 1<<20 {
		t.Errorf("config = %+v", c)
	}
	if r.CanRead() {
		t.Error("a locked repository claims it can read")
	}
	slots, err := r.Slots(ctx)
	if err != nil || len(slots) != 2 {
		t.Fatalf("Slots = %d, %v", len(slots), err)
	}

	// The config is plaintext JSON with no secrets.
	raw, err := os.ReadFile(filepath.Join(f.dir, "config")) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}

	full := f.reopen(t, f.full, "admin")
	if !full.CanRead() || full.PublicKey().String() != f.material.Public().String() {
		t.Error("full unlock did not load the key pair")
	}
	agent := f.reopen(t, f.agent, "agent")
	if agent.CanRead() || agent.PublicKey().String() != f.material.Public().String() {
		t.Error("agent unlock can read, or lacks the public key")
	}

	// A slot from another repository does not unlock this one.
	other := newFixture(t)
	u, _ := other.full.UnlockPassphrase(secrets.New([]byte("admin")))
	if err := r.Unlock(ctx, *u); err == nil {
		t.Error("another repository's slot unlocked this one")
	}
}

func TestCreateTwice(t *testing.T) {
	f := newFixture(t)
	_, err := Create(context.Background(), f.be, NewConfig(false, time.Now()), f.material, []*crypto.Slot{f.full})
	wantMsgID(t, err, IDExists)
}

func TestOpenMissing(t *testing.T) {
	be, _ := local.New(t.TempDir())
	_, err := Open(context.Background(), be)
	wantMsgID(t, err, IDNotFound)
}

// TestFormatVersionRefusal: a binary refuses a newer repository format.
func TestFormatVersionRefusal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cfg := f.repo.Config()
	cfg.Version = FormatVersion + 1
	b, _ := json.Marshal(cfg)
	if err := f.be.Put(ctx, configKey, bytes.NewReader(b), backend.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := Open(ctx, f.be)
	wantMsgID(t, err, IDFormatNewer)
	if !strings.Contains(err.Error(), "newer than this pentym understands") {
		t.Errorf("error = %v", err)
	}

	// A newer format is reported as such even with fields this version
	// does not know.
	b = []byte(`{"version": 99, "new_field": true}`)
	_ = f.be.Put(ctx, configKey, bytes.NewReader(b), backend.PutOptions{})
	_, err = Open(ctx, f.be)
	wantMsgID(t, err, IDFormatNewer)
}

func TestParseConfigRejects(t *testing.T) {
	good, _ := json.Marshal(NewConfig(false, time.Now()))
	mutate := func(f func(map[string]any)) []byte {
		var m map[string]any
		_ = json.Unmarshal(good, &m)
		f(m)
		b, _ := json.Marshal(m)
		return b
	}
	for name, b := range map[string][]byte{
		"not json":        []byte("{"),
		"no version":      mutate(func(m map[string]any) { delete(m, "version") }),
		"short id":        mutate(func(m map[string]any) { m["id"] = "abc" }),
		"encryption":      mutate(func(m map[string]any) { m["encryption"] = "rot13" }),
		"unknown field":   mutate(func(m map[string]any) { m["extra"] = 1 }),
		"chunk sizes":     mutate(func(m map[string]any) { m["chunker"].(map[string]any)["avg"] = 2 << 20 }),
		"version is text": mutate(func(m map[string]any) { m["version"] = "1" }),
	} {
		if _, err := ParseConfig(b); err == nil {
			t.Errorf("%s: ParseConfig accepted it", name)
		}
	}
	if _, err := ParseConfig(good); err != nil {
		t.Errorf("good config rejected: %v", err)
	}
}

func TestSlotsAddRemove(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	paper, _, err := crypto.NewPaperSlot("paper", f.material.Unlocked(), fastKDF)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.AddSlot(ctx, paper); err != nil {
		t.Fatal(err)
	}
	slots, _ := f.repo.Slots(ctx)
	if len(slots) != 3 || crypto.FullSlots(slots) != 2 {
		t.Fatalf("after add: %d slots, %d full", len(slots), crypto.FullSlots(slots))
	}
	if err := f.repo.RemoveSlot(ctx, f.full.ID); err != nil {
		t.Fatalf("removing one of two full slots: %v", err)
	}
	wantMsgID(t, f.repo.RemoveSlot(ctx, paper.ID), crypto.IDLastFullSlot)
	if err := f.repo.RemoveSlot(ctx, f.agent.ID); err != nil {
		t.Errorf("removing the agent slot: %v", err)
	}
	slots, _ = f.repo.Slots(ctx)
	if len(slots) != 1 || slots[0].ID != paper.ID {
		t.Errorf("slots left: %v", slots)
	}
}

func TestTargetIDs(t *testing.T) {
	f := newFixture(t)
	a, err := f.repo.TargetID("prod-pg")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := f.repo.TargetID("prod-pg")
	c, _ := f.repo.TargetID("prod-mg")
	other, _ := newFixture(t).repo.TargetID("prod-pg")
	if a != b || a == c || a == other || len(a) != 16 || strings.Contains(a, "prod") {
		t.Errorf("target IDs: %s %s %s %s", a, b, c, other)
	}
	if err := backend.ValidateKey("targets/" + a + "/info"); err != nil {
		t.Errorf("target ID not usable in a key: %v", err)
	}
	locked, _ := Open(context.Background(), f.be)
	if _, err := locked.TargetID("prod-pg"); !errors.Is(err, ErrLocked) {
		t.Errorf("locked TargetID = %v", err)
	}
}

func TestTargetInfo(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tid, err := f.repo.PutTargetInfo(ctx, TargetInfo{Name: "prod-pg", Engine: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if again, err := f.repo.PutTargetInfo(ctx, TargetInfo{Name: "prod-pg", Engine: "postgres"}); err != nil || again != tid {
		t.Errorf("second PutTargetInfo = %s, %v", again, err)
	}
	_, _ = f.repo.PutTargetInfo(ctx, TargetInfo{Name: "prod-mg", Engine: "mongodb"})
	// An agent writes and reads metadata too.
	agent := f.reopen(t, f.agent, "agent")
	targets, err := agent.Targets(ctx)
	if err != nil || len(targets) != 2 || targets[tid].Name != "prod-pg" {
		t.Errorf("Targets = %v, %v", targets, err)
	}
}

func TestCheckpoints(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tid, _ := f.repo.TargetID("prod-pg")
	if c, err := f.repo.LatestCheckpoint(ctx, tid); err != nil || c != nil {
		t.Fatalf("no checkpoints: %v, %v", c, err)
	}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	for _, c := range []Checkpoint{
		{Target: tid, Generation: 1, Seq: 1, Position: "0/1000000"},
		{Target: tid, Generation: 1, Seq: 2, Position: "0/2000000"},
		{Target: tid, Generation: 2, Seq: 1, Position: "0/2100000"},
		{Target: tid, Generation: 1, Seq: 10, Position: "0/9000000"}, // a stale holder, older generation
	} {
		c.Time = now
		if err := f.repo.PutCheckpoint(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	c, err := f.repo.LatestCheckpoint(ctx, tid)
	if err != nil || c.Generation != 2 || c.Position != "0/2100000" {
		t.Errorf("LatestCheckpoint = %+v, %v; want generation 2", c, err)
	}
	// Writing the same checkpoint twice is refused, never overwritten.
	err = f.repo.PutCheckpoint(ctx, Checkpoint{Target: tid, Generation: 2, Seq: 1, Position: "x"})
	if !errors.Is(err, backend.ErrExists) {
		t.Errorf("rewriting a checkpoint = %v", err)
	}
}

func TestHeartbeats(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 25, 14, 0, 0, 0, time.UTC)
	for i, h := range []Heartbeat{
		{Host: "db-1", AgentID: "a1", State: StateRunning, Time: t0},
		{Host: "db-1", AgentID: "a1", State: StateRunning, Time: t0.Add(30 * time.Second)},
		{Host: "backup host.example", AgentID: "a2", State: StateRunning, Time: t0},
		{Host: "db-1", AgentID: "a1", State: StateStopped, Time: t0.Add(60 * time.Second)},
	} {
		if err := f.repo.PutHeartbeat(ctx, h); err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
	}
	hs, err := f.repo.LatestHeartbeats(ctx)
	if err != nil || len(hs) != 2 {
		t.Fatalf("LatestHeartbeats = %v, %v", hs, err)
	}
	for _, h := range hs {
		if h.Host == "db-1" && (h.State != StateStopped || !h.Time.Equal(t0.Add(60*time.Second))) {
			t.Errorf("db-1 latest = %+v", h)
		}
	}
}

func TestEvents(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tid, _ := f.repo.TargetID("prod-pg")
	t0 := time.Date(2026, 11, 25, 13, 41, 7, 0, time.UTC)
	_ = f.repo.PutEvent(ctx, Event{Target: tid, Type: "DROP TABLE", Object: "public.orders", XID: "8812", Time: t0.Add(22 * time.Minute)})
	_ = f.repo.PutEvent(ctx, Event{Target: tid, Type: "TRUNCATE", Object: "public.sessions", XID: "8790", Time: t0})
	all, err := f.repo.Events(ctx, tid, time.Time{})
	if err != nil || len(all) != 2 || all[0].Type != "TRUNCATE" || all[1].Object != "public.orders" {
		t.Fatalf("Events = %+v, %v", all, err)
	}
	recent, _ := f.repo.Events(ctx, tid, t0.Add(time.Minute))
	if len(recent) != 1 || recent[0].XID != "8812" {
		t.Errorf("Events since = %+v", recent)
	}
}

func TestJournal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 30, 23, 0, 0, 0, time.UTC)
	f.repo.Now = func() time.Time { return clock }
	id := Identity{User: "ada", Host: "db-1"}
	ref, _ := secrets.ParseRef("env:PGPASSWORD")

	op := f.repo.BeginOperation(id, "backup", "t1", "abc123def456", ref)
	clock = clock.Add(2 * time.Minute)
	if err := op.Finish(ctx, ResultOK, nil); err != nil {
		t.Fatal(err)
	}
	clock = time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	op = f.repo.BeginOperation(id, "verify", "t1", "", secrets.Ref{})
	_ = op.Finish(ctx, ResultOK, errors.New("connect postgres://app:hunter2@db/x: refused"))
	clock = clock.Add(time.Second)
	op = f.repo.BeginOperation(id, "prune", "", "", secrets.Ref{})
	_ = op.Finish(ctx, ResultSkipped, errors.New("lock held"))

	all, err := f.repo.Journal(ctx, time.Time{}.Add(1), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("Journal = %d entries", len(all))
	}
	b := all[0]
	if b.Who != "ada" || b.Host != "db-1" || b.Operation != "backup" || b.Credential != "env:PGPASSWORD" ||
		b.Result != ResultOK || b.End.Sub(b.Start) != 2*time.Minute || b.RunID != "abc123def456" {
		t.Errorf("backup entry = %+v", b)
	}
	if v := all[1]; v.Result != ResultFailed || strings.Contains(v.Detail, "hunter2") || !strings.Contains(v.Detail, "[redacted]") {
		t.Errorf("failed entry = %+v", v)
	}
	if all[2].Result != ResultSkipped {
		t.Errorf("skipped entry = %+v", all[2])
	}
	nov, _ := f.repo.Journal(ctx, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), "")
	if len(nov) != 2 {
		t.Errorf("entries since November = %d", len(nov))
	}
	t1, _ := f.repo.Journal(ctx, time.Time{}.Add(1), "t1")
	if len(t1) != 2 {
		t.Errorf("entries for t1 = %d", len(t1))
	}
	if CurrentIdentity().User == "" || CurrentIdentity().Host == "" {
		t.Error("CurrentIdentity is empty")
	}
}

// TestMetadataBound: a metadata object copied to another key does not
// decrypt.
func TestMetadataBound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tid, _ := f.repo.TargetID("prod-pg")
	_ = f.repo.PutEvent(ctx, Event{Target: tid, Type: "DROP TABLE", Time: time.Now()})
	keys, _ := f.repo.listKeys(ctx, eventPrefix(tid))
	b, _ := readAll(ctx, f.be, keys[0], 1<<20)
	_ = f.be.Put(ctx, eventPrefix(tid)+"9999999999999999999-moved", bytes.NewReader(b), backend.PutOptions{})
	if _, err := f.repo.Events(ctx, tid, time.Time{}); !errors.Is(err, crypto.ErrDecrypt) {
		t.Errorf("moved event read: %v", err)
	}
}

// TestSecretLeakRepository writes every kind of object and checks that no
// file in the repository holds a key, the private key, a target name or
// an event's object name in plaintext.
func TestSecretLeakRepository(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tid, _ := f.repo.PutTargetInfo(ctx, TargetInfo{Name: "orders-db-canary", Engine: "postgres"})
	_ = f.repo.PutEvent(ctx, Event{Target: tid, Type: "DROP TABLE", Object: "public.secret_customers_canary", Time: time.Now()})
	_ = f.repo.PutCheckpoint(ctx, Checkpoint{Target: tid, Generation: 1, Seq: 1, Position: "0/16B3748"})
	_ = f.repo.PutHeartbeat(ctx, Heartbeat{Host: "db-1", AgentID: "agent-canary-id", State: StateRunning, Time: time.Now()})
	op := f.repo.BeginOperation(Identity{User: "user-canary", Host: "h"}, "backup", tid, "", secrets.Ref{})
	_ = op.Finish(ctx, ResultOK, nil)

	var files []string
	_ = filepath.WalkDir(f.dir, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			b, _ := os.ReadFile(p) // #nosec G304 G122 -- reading a test repository in a temporary directory
			files = append(files, string(b))
		}
		return nil
	})
	canaries := []leaktest.Canary{"orders-db-canary", "secret_customers_canary", "agent-canary-id", "user-canary", "0/16B3748"}
	for _, k := range []crypto.Key{f.material.Master, f.material.Metadata, f.material.ChunkID} {
		canaries = append(canaries, leaktest.Canary(k.Bytes()))
	}
	leaktest.Scan(t, "repository files", canaries, files...)
	for _, s := range files {
		if strings.Contains(s, "AGE-SECRET-KEY") {
			t.Fatal("the private key is stored in plaintext")
		}
	}
}

func TestInsecureRepository(t *testing.T) {
	dir := t.TempDir()
	be, _ := local.New(dir)
	ctx := context.Background()
	r, err := Create(ctx, be, NewConfig(true, time.Now()), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tid, _ := r.PutTargetInfo(ctx, TargetInfo{Name: "dev", Engine: "postgres"})
	r2, err := Open(ctx, be)
	if err != nil || !r2.Config().Insecure() || !r2.CanRead() {
		t.Fatalf("Open insecure = %v", err)
	}
	targets, err := r2.Targets(ctx)
	if err != nil || targets[tid].Name != "dev" {
		t.Errorf("Targets = %v, %v", targets, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "targets", tid, "info")) // #nosec G304 -- test file
	if !strings.Contains(string(raw), `"dev"`) {
		t.Error("insecure repository did not store plaintext")
	}
	if _, err := Create(ctx, be, NewConfig(false, time.Now()), nil, nil); err == nil {
		t.Error("encrypted repository created without keys")
	}
}

func TestKeySafe(t *testing.T) {
	for in, want := range map[string]string{
		"db-1": "db-1", "backup host.example": "backup-host.example", ".hidden": "-hidden",
		"ünï": "-n-", "": "unknown", "a/b": "a-b",
	} {
		if got := keySafe(in); got != want {
			t.Errorf("keySafe(%q) = %q, want %q", in, got, want)
		}
	}
}
