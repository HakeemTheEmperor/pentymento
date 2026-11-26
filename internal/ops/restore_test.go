// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package ops

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeebo/blake3"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// restoreEngine is the fake engine with a Restore that reads every
// archive, hashing it, and starts an instance over the data directory.
type restoreEngine struct {
	*fakeEngine
	be     backend.Backend
	fail   error
	sums   map[string][32]byte
	sizes  map[string]int64
	locked bool
}

func (e *restoreEngine) Restore(ctx context.Context, req engine.RestoreRequest, start engine.StartFunc) (instance.Instance, *engine.Reached, error) {
	_ = e.be.List(ctx, "locks/", func(oi backend.ObjectInfo) error {
		if strings.Contains(oi.Key, "shared") {
			e.locked = true
		}
		return nil
	})
	e.sums, e.sizes = map[string][32]byte{}, map[string]int64{}
	for _, name := range req.Archives.Names() {
		h := blake3.New()
		n := int64(0)
		cw := &countingWriter{w: h, onBytes: func(k int64) { n += k }}
		if err := req.Archives.Archive(ctx, name, cw); err != nil {
			return nil, nil, err
		}
		var sum [32]byte
		h.Sum(sum[:0])
		e.sums[name], e.sizes[name] = sum, n
	}
	if err := os.WriteFile(filepath.Join(req.DataDir, "PG_VERSION"), []byte("17\n"), 0o600); err != nil {
		return nil, nil, err
	}
	if e.fail != nil {
		return nil, nil, e.fail
	}
	inst, err := start(ctx, "17", req.DataDir)
	if err != nil {
		return nil, nil, err
	}
	return inst, &engine.Reached{Position: req.Backup.End}, nil
}

// fakeStarter starts fake instances and remembers the specs.
type fakeStarter struct {
	specs []instance.Spec
	inst  *fakeInstance
}

type fakeInstance struct{ stopped atomic.Bool }

func (*fakeInstance) Conn() instance.Conn {
	return instance.Conn{Host: "127.0.0.1", Port: 1, User: "pentym_side"}
}
func (f *fakeInstance) Stop(context.Context) error { f.stopped.Store(true); return nil }

func (s *fakeStarter) Start(_ context.Context, spec instance.Spec) (instance.Instance, error) {
	s.specs = append(s.specs, spec)
	s.inst = &fakeInstance{}
	return s.inst, nil
}

func (f *fixture) restore(t *testing.T, e engine.Engine, starter Starter, workDir string) *Restore {
	t.Helper()
	return &Restore{
		Repo: f.admin(t), Engine: e, Target: engine.Target{Name: "prod-pg"},
		Instances: starter, WorkDir: workDir,
		Identity: repo.Identity{User: "ada", Host: "admin-1"}, Telemetry: f.tel,
	}
}

func sumOf(r io.Reader) [32]byte {
	h := blake3.New()
	_, _ = io.Copy(h, r)
	var s [32]byte
	h.Sum(s[:0])
	return s
}

func TestRestore(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, err := f.backup(&fakeEngine{baseSize: 3 << 20, seed: 1}).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.backup(&fakeEngine{baseSize: 5 << 20, seed: 2}).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "work")

	// The latest snapshot by default.
	e := &restoreEngine{fakeEngine: &fakeEngine{}, be: f.be}
	st := &fakeStarter{}
	var read int64
	o := f.restore(t, e, st, work)
	o.OnBytes = func(n int64) { read += n }
	res, err := o.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Snapshot.ID != second.Snapshot.ID || res.Reached.Position != "0/2000100" || !res.Reached.Time.Equal(res.Snapshot.End) {
		t.Errorf("restored %s, reached %+v; want the latest, %s", res.Snapshot.ID, res.Reached, second.Snapshot.ID)
	}
	if e.sums["base.tar"] != sumOf(stream(2, 5<<20)) || e.sizes["16384.tar"] != 10000 || e.sizes["backup_manifest"] == 0 {
		t.Errorf("archives read back wrong: sizes %v", e.sizes)
	}
	if read != res.Snapshot.TotalSize() {
		t.Errorf("progress counted %d bytes of %d", read, res.Snapshot.TotalSize())
	}
	if !e.locked {
		t.Error("the restore ran without a shared lock")
	}
	if len(st.specs) != 1 || st.specs[0].Engine != "postgres" || st.specs[0].Version != "17" ||
		st.specs[0].DataDir != res.DataDir || st.specs[0].RunID != res.RunID || filepath.Dir(res.DataDir) != work {
		t.Errorf("specs = %+v, data dir %s", st.specs, res.DataDir)
	}
	if j := f.lastJournal(t); j.Operation != "restore" || j.Result != repo.ResultOK || j.Target == "" {
		t.Errorf("journal = %+v", j)
	}
	f.noLocks(t)
	if err := res.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.DataDir); !errors.Is(err, os.ErrNotExist) || !st.inst.stopped.Load() {
		t.Errorf("Close left the data directory (%v) or the instance running", err)
	}

	// A chosen snapshot.
	o = f.restore(t, e, &fakeStarter{}, work)
	o.SnapshotID = first.Snapshot.ID
	res, err = o.Run(ctx)
	if err != nil || res.Snapshot.ID != first.Snapshot.ID || e.sums["base.tar"] != sumOf(stream(1, 3<<20)) {
		t.Fatalf("chosen snapshot: %v", err)
	}
	_ = res.Close(ctx)
}

func TestRestoreRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	work := filepath.Join(t.TempDir(), "work")
	e := &restoreEngine{fakeEngine: &fakeEngine{}, be: f.be}

	var me *msg.Error
	if _, err := f.restore(t, e, &fakeStarter{}, work).Run(ctx); !errors.As(err, &me) || me.Code != msg.ExitNotFound ||
		me.Message.Fact != "the repository has no snapshots of prod-pg" {
		t.Errorf("no snapshots: %v", err)
	}
	b, err := f.backup(&fakeEngine{baseSize: 1 << 20, seed: 3}).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	o := f.restore(t, e, &fakeStarter{}, work)
	o.SnapshotID = strings.Repeat("ab", 16)
	if _, err := o.Run(ctx); !errors.As(err, &me) || me.Code != msg.ExitNotFound {
		t.Errorf("unknown snapshot: %v", err)
	}
	o = f.restore(t, e, &fakeStarter{}, work)
	o.Target.Name, o.SnapshotID = "other-pg", b.Snapshot.ID
	if _, err := o.Run(ctx); !errors.As(err, &me) || me.Code != msg.ExitUsage {
		t.Errorf("another target's snapshot: %v", err)
	}
	// A failing engine leaves no directory behind.
	e.fail = errors.New("the server would not start")
	if _, err := f.restore(t, e, &fakeStarter{}, work).Run(ctx); !errors.Is(err, e.fail) {
		t.Errorf("failing engine: %v", err)
	}
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Errorf("a failed restore left %v", entries)
	}
	if j := f.lastJournal(t); j.Operation != "restore" || j.Result == repo.ResultOK {
		t.Errorf("journal = %+v", j)
	}
	f.noLocks(t)
}

// TestRestoreConstantMemory restores a snapshot far larger than the memory
// the read may use. PENTYM_TEST_LARGE_MIB sets the size.
func TestRestoreConstantMemory(t *testing.T) {
	size := int64(256)
	if v, err := strconv.ParseInt(os.Getenv("PENTYM_TEST_LARGE_MIB"), 10, 64); err == nil && v > 0 {
		size = v
	}
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.backup(&fakeEngine{baseSize: size << 20, seed: 9}).Run(ctx); err != nil {
		t.Fatal(err)
	}
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
	e := &restoreEngine{fakeEngine: &fakeEngine{}, be: f.be}
	res, err := f.restore(t, e, &fakeStarter{}, filepath.Join(t.TempDir(), "work")).Run(ctx)
	close(done)
	<-sampled
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Close(ctx)
	if e.sizes["base.tar"] != size<<20 {
		t.Fatalf("read %d bytes", e.sizes["base.tar"])
	}
	const limit = 200 << 20
	p := peak.Load()
	t.Logf("restored %d MiB with a peak live heap of %d MiB", size, p>>20)
	if p > limit {
		t.Errorf("peak live heap %d MiB exceeds %d MiB", p>>20, limit>>20)
	}
}
