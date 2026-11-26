// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package ops

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/local"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/lock"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/telemetry"
)

var fastKDF = crypto.KDFParams{Name: "argon2id", MemoryKiB: 8 << 10, Time: 1, Threads: 1}

// fakeEngine streams deterministic archives; its fields choose failures.
type fakeEngine struct {
	engine.Engine
	baseSize  int64
	seed      uint64
	findings  []engine.Finding
	failAfter int64 // fail base.tar after this many bytes; 0 never
	onData    func()
}

func (*fakeEngine) Name() string { return "postgres" }

func (f *fakeEngine) Check(context.Context, engine.Target) (*engine.CheckResult, error) {
	return &engine.CheckResult{ServerVersion: "17.2", Findings: f.findings}, nil
}

func (*fakeEngine) Inventory(context.Context, engine.Target) (json.RawMessage, error) {
	return json.RawMessage(`{"databases":[{"name":"app"}]}`), nil
}

var errMidStream = errors.New("connection lost mid-stream")

func (f *fakeEngine) BaseBackup(ctx context.Context, _ engine.Target, sink engine.ArchiveSink) (*engine.BaseBackup, error) {
	w, err := sink.Archive(ctx, "base.tar")
	if err != nil {
		return nil, err
	}
	src := io.Reader(stream(f.seed, f.baseSize))
	if f.failAfter > 0 {
		src = io.LimitReader(src, f.failAfter)
	}
	buf := make([]byte, 64<<10)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if f.onData != nil {
				f.onData()
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return nil, err
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	if f.failAfter > 0 {
		return nil, errMidStream
	}
	for _, a := range [][2]string{{"16384.tar", strings.Repeat("tablespace", 1000)}, {"backup_manifest", `{"PostgreSQL-Backup-Manifest-Version":1}`}} {
		name, data := a[0], a[1]
		w, err := sink.Archive(ctx, name)
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(w, data); err != nil {
			return nil, err
		}
	}
	return &engine.BaseBackup{EngineVersion: "17.2", Method: "native", SystemID: "7421", Start: "0/2000028", End: "0/2000100", Timeline: "1"}, nil
}

// stream returns size bytes of deterministic content: 64 KiB blocks,
// alternately random and compressible, each a function of its position
// only, so every reader sees the same bytes.
func stream(seed uint64, size int64) io.Reader {
	return io.LimitReader(&blockStream{seed: seed}, size)
}

type blockStream struct {
	seed  uint64
	block int64
	buf   []byte
}

func (s *blockStream) Read(p []byte) (int, error) {
	if len(s.buf) == 0 {
		rng := rand.New(rand.NewPCG(s.seed, uint64(s.block))) // #nosec G404 G115 -- test data
		b := make([]byte, 64<<10)
		for i := 0; i < len(b); i += 8 {
			v := rng.Uint64()
			if s.block%2 == 1 {
				v &= 0x0101010101010101 // compressible
			}
			binary.LittleEndian.PutUint64(b[i:], v)
		}
		s.buf = b
		s.block++
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

type fixture struct {
	dir  string
	be   *local.Backend
	repo *repo.Repository
	full *crypto.Slot
	tel  *telemetry.Telemetry
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
	if _, err := repo.Create(context.Background(), be, repo.NewConfig(false, time.Now()), m, []*crypto.Slot{full, agent}); err != nil {
		t.Fatal(err)
	}
	r, err := repo.Open(context.Background(), be)
	if err != nil {
		t.Fatal(err)
	}
	// Backups run with the agent slot, as on a database host.
	if _, _, err := r.UnlockWith(context.Background(), secrets.New([]byte("agent")), repo.UnlockOptions{}); err != nil {
		t.Fatal(err)
	}
	return &fixture{dir: dir, be: be, repo: r, full: full, tel: telemetry.New(nil, nil)}
}

// admin opens the repository with the full slot.
func (f *fixture) admin(t *testing.T) *repo.Repository {
	t.Helper()
	r, err := repo.Open(context.Background(), f.be)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.UnlockWith(context.Background(), secrets.New([]byte("admin")), repo.UnlockOptions{NeedFull: true}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fixture) backup(e engine.Engine) *Backup {
	ref, _ := secrets.ParseRef("env:PG_BACKUP_PW")
	return &Backup{
		Repo: f.repo, Engine: e, Target: engine.Target{Name: "prod-pg"},
		Identity: repo.Identity{User: "ada", Host: "db-1"}, Credential: ref, Telemetry: f.tel,
		Write: repo.WriteOptions{Workers: 2},
	}
}

func (f *fixture) snapshots(t *testing.T) []repo.Snapshot {
	t.Helper()
	s, err := f.admin(t).Snapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *fixture) lastJournal(t *testing.T) repo.JournalEntry {
	t.Helper()
	j, err := f.admin(t).Journal(context.Background(), time.Time{}, "")
	if err != nil || len(j) == 0 {
		t.Fatalf("journal: %d entries, %v", len(j), err)
	}
	return j[len(j)-1]
}

func (f *fixture) noLocks(t *testing.T) {
	t.Helper()
	n := 0
	_ = f.be.List(context.Background(), "locks/", func(oi backend.ObjectInfo) error {
		if !strings.Contains(oi.Key, "probe") {
			n++
		}
		return nil
	})
	if n != 0 {
		t.Errorf("%d locks left", n)
	}
}

func TestBackup(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var counted int64
	b := f.backup(&fakeEngine{baseSize: 6 << 20, seed: 1})
	b.OnBytes = func(n int64) { counted += n }
	res, err := b.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Snapshot
	tid, _ := f.repo.TargetID("prod-pg")
	if s.Target != tid || s.Engine != "postgres" || s.EngineVersion != "17.2" || s.Method != "native" || s.SystemID != "7421" ||
		s.StartPosition != "0/2000028" || s.EndPosition != "0/2000100" || s.Timeline != "1" || s.Name != "base.tar" ||
		len(s.Parts) != 2 || string(s.Inventory) != `{"databases":[{"name":"app"}]}` {
		t.Errorf("snapshot = %+v", s)
	}
	if counted != s.TotalSize() || res.Stats.Bytes != s.TotalSize() || res.RunID == "" {
		t.Errorf("counted %d bytes, snapshot %d, stats %d", counted, s.TotalSize(), res.Stats.Bytes)
	}

	// The snapshot reads back with a full slot.
	admin := f.admin(t)
	idx, _ := admin.LoadIndex(ctx)
	loaded, err := admin.LoadSnapshot(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := admin.ReadPart(ctx, idx, loaded, "base.tar", &got); err != nil {
		t.Fatal(err)
	}
	want, _ := io.ReadAll(stream(1, 6<<20))
	if !bytes.Equal(got.Bytes(), want) {
		t.Error("base.tar reads back different data")
	}
	targets, _ := admin.Targets(ctx)
	if targets[tid].Name != "prod-pg" || targets[tid].Engine != "postgres" {
		t.Errorf("target info = %+v", targets)
	}

	j := f.lastJournal(t)
	if j.Operation != "backup" || j.Result != repo.ResultOK || j.Target != tid || j.RunID != res.RunID || j.Credential != "env:PG_BACKUP_PW" {
		t.Errorf("journal = %+v", j)
	}
	f.noLocks(t)
	if testutil.ToFloat64(f.tel.Metrics.LastSuccess.WithLabelValues("prod-pg", "backup")) == 0 || testutil.ToFloat64(f.tel.Metrics.DedupRatio) == 0 {
		t.Error("success metrics not set")
	}

	// A second backup of almost the same data stores little.
	res2, err := f.backup(&fakeEngine{baseSize: 6 << 20, seed: 1}).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Stats.NewBytes > res.Stats.NewBytes/10 {
		t.Errorf("second backup stored %d bytes, the first %d", res2.Stats.NewBytes, res.Stats.NewBytes)
	}
}

func TestBackupFailures(t *testing.T) {
	tests := []struct {
		name string
		eng  func(cancel func()) *fakeEngine
		ctx  func() (context.Context, func())
		want func(error) bool
	}{
		{"fails mid-stream", func(func()) *fakeEngine { return &fakeEngine{baseSize: 4 << 20, failAfter: 2 << 20} },
			nil, func(err error) bool { return errors.Is(err, errMidStream) }},
		{"cancelled mid-stream", func(cancel func()) *fakeEngine {
			n := 0
			return &fakeEngine{baseSize: 8 << 20, onData: func() {
				if n++; n == 20 {
					cancel()
				}
			}}
		}, nil, func(err error) bool { return errors.Is(err, context.Canceled) }},
		{"check fails", func(func()) *fakeEngine {
			return &fakeEngine{baseSize: 1 << 20, findings: []engine.Finding{
				{Level: msg.LevelWarning, ID: "PTM-2004", Fact: "superuser"},
				{Level: msg.LevelFailed, ID: "PTM-2003", Fact: "pentym_backup lacks REPLICATION", Next: "ALTER ROLE"},
			}}
		}, nil, func(err error) bool {
			var me *msg.Error
			return errors.As(err, &me) && me.Message.ID == "PTM-2003" && me.Message.Target == "prod-pg"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := f.backup(tt.eng(cancel)).Run(ctx)
			if !tt.want(err) {
				t.Fatalf("Run = %v", err)
			}
			if s := f.snapshots(t); len(s) != 0 {
				t.Errorf("%d snapshots committed", len(s))
			}
			if j := f.lastJournal(t); j.Operation != "backup" || j.Result != repo.ResultFailed {
				t.Errorf("journal = %+v", j)
			}
			f.noLocks(t)
			if testutil.ToFloat64(f.tel.Metrics.JobFailures.WithLabelValues("prod-pg", "backup")) != 1 {
				t.Error("failure not counted")
			}
		})
	}
}

// TestBackupWaitsForExclusiveLock: a backup does not run while prune's
// garbage collection holds the exclusive lock.
func TestBackupBlockedByExclusiveLock(t *testing.T) {
	f := newFixture(t)
	l, err := lock.NewLocks(f.be, lock.Holder{User: "prune", Host: "admin-1", PID: 1}).AcquireExclusive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release(context.Background()) }()
	_, err = f.backup(&fakeEngine{baseSize: 1 << 20}).Run(context.Background())
	var me *msg.Error
	if !errors.As(err, &me) || me.Message.ID != lock.IDLocked {
		t.Fatalf("Run under an exclusive lock = %v", err)
	}
	if s := f.snapshots(t); len(s) != 0 {
		t.Error("a snapshot was committed")
	}
}

// TestBackupConstantMemory backs up a stream far larger than the memory
// it may use. PENTYM_TEST_LARGE_MIB sets the size (the nightly job uses
// 4096); the default keeps the test quick.
func TestBackupConstantMemory(t *testing.T) {
	size := int64(256)
	if v, err := strconv.ParseInt(os.Getenv("PENTYM_TEST_LARGE_MIB"), 10, 64); err == nil && v > 0 {
		size = v
	}
	f := newFixture(t)
	// Sample the live heap: collect garbage first, as the repository's
	// large-snapshot test does, so the figure is what the backup holds.
	var peak atomic.Uint64
	done := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		var ms runtime.MemStats
		for {
			runtime.GC()
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak.Load() {
				peak.Store(ms.HeapAlloc)
			}
			select {
			case <-done:
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
	}()
	b := f.backup(&fakeEngine{baseSize: size << 20, seed: 9})
	b.Write.Workers = 4
	_, err := b.Run(context.Background())
	close(done)
	<-sampled
	if err != nil {
		t.Fatal(err)
	}
	const limit = 320 << 20
	p := peak.Load()
	t.Logf("backed up %d MiB with a peak live heap of %d MiB", size, p>>20)
	if p > limit {
		t.Errorf("peak live heap %d MiB exceeds %d MiB", p>>20, limit>>20)
	}
}
