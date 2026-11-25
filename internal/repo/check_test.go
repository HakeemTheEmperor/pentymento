// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/compress"
)

func check(t *testing.T, r *Repository, opts CheckOptions) *CheckReport {
	t.Helper()
	rep, err := r.Check(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func problemsText(rep *CheckReport) string {
	var b strings.Builder
	for _, p := range rep.Problems {
		fmt.Fprintf(&b, "  %s %s %s %s: %s\n", p.Step, p.Severity, p.Kind, p.Object, p.Detail)
	}
	return b.String()
}

// hasProblem reports whether rep holds a problem matching every non-empty
// field of want.
func hasProblem(rep *CheckReport, want Problem) bool {
	for _, p := range rep.Problems {
		if (want.Step == "" || p.Step == want.Step) && (want.Severity == "" || p.Severity == want.Severity) &&
			(want.Kind == "" || p.Kind == want.Kind) && (want.Object == "" || p.Object == want.Object) &&
			strings.Contains(p.Detail, want.Detail) {
			return true
		}
	}
	return false
}

func TestCheckClean(t *testing.T) {
	d := newDataRepo(t)
	rep := check(t, d.open(t), CheckOptions{Full: true})
	if len(rep.Problems) != 0 {
		t.Fatalf("problems in a clean repository:\n%s", problemsText(rep))
	}
	if rep.Slots != 3 || rep.FullSlots != 2 || rep.Snapshots != 2 || rep.Segments != 2 || rep.SegmentsRead != 2 ||
		rep.IndexedPacks < 2 || rep.PacksChecked != rep.IndexedPacks || rep.PacksRead != rep.IndexedPacks ||
		rep.ChunksVerified != rep.IndexedChunks || rep.SnapshotChunks != d.snapA.Chunks+d.snapB.Chunks {
		t.Errorf("report = %+v", rep)
	}
}

func TestCheckNeedsFullSlot(t *testing.T) {
	d := newDataRepo(t)
	agent := d.reopen(t, d.agent, "agent")
	if _, err := agent.Check(context.Background(), CheckOptions{}); err != ErrNoPrivateKey {
		t.Errorf("Check with an agent slot = %v", err)
	}
}

// TestCheckFindsCorruption injects one kind of damage at a time.
func TestCheckFindsCorruption(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, d *dataRepo) Problem // returns a problem that must be reported
		errors bool                                    // whether error-severity problems are expected
	}{
		{"pack deleted", func(t *testing.T, d *dataRepo) Problem {
			k := d.packOf(t, d.snapA)
			d.remove(t, k)
			return Problem{Step: CheckPacks, Severity: SeverityError, Kind: ProblemMissing, Object: k, Detail: "pack is missing"}
		}, true},
		{"byte flipped in a pack", func(t *testing.T, d *dataRepo) Problem {
			k := d.packOf(t, d.snapA)
			d.flip(t, k, 100)
			return Problem{Step: CheckData, Severity: SeverityError, Kind: ProblemCorrupt, Object: k, Detail: "does not match its name"}
		}, true},
		{"pack truncated", func(t *testing.T, d *dataRepo) Problem {
			k := d.packOf(t, d.snapA)
			d.mutate(t, k, func(b []byte) []byte { return b[:len(b)/2] })
			return Problem{Step: CheckPacks, Severity: SeverityError, Kind: ProblemCorrupt, Object: k, Detail: "too short"}
		}, true},
		{"index file corrupted", func(t *testing.T, d *dataRepo) Problem {
			k := d.keys(t, indexPrefix)[0]
			d.flip(t, k, 50)
			return Problem{Step: CheckIndex, Severity: SeverityError, Kind: ProblemCorrupt, Object: k}
		}, true},
		{"index files deleted", func(t *testing.T, d *dataRepo) Problem {
			for _, k := range d.keys(t, indexPrefix) {
				d.remove(t, k)
			}
			return Problem{Step: CheckSnapshots, Severity: SeverityError, Kind: ProblemMissing, Detail: "not in the index"}
		}, true},
		{"snapshot corrupted", func(t *testing.T, d *dataRepo) Problem {
			k := snapshotKey(d.snapB.ID)
			d.flip(t, k, 40)
			return Problem{Step: CheckSnapshots, Severity: SeverityError, Kind: ProblemCorrupt, Object: k}
		}, true},
		{"log segment corrupted", func(t *testing.T, d *dataRepo) Problem {
			d.flip(t, d.walKey, -5)
			return Problem{Step: CheckSegments, Severity: SeverityError, Kind: ProblemCorrupt, Object: d.walKey}
		}, true},
		{"oplog batch deleted is just gone", func(t *testing.T, d *dataRepo) Problem {
			d.remove(t, d.opKey)
			return Problem{} // nothing references a segment yet; capture coverage checks come later
		}, false},
		{"stray object under data/", func(t *testing.T, d *dataRepo) Problem {
			if err := d.be.Put(context.Background(), "data/zz/not-a-pack", strings.NewReader("x"), backend.PutOptions{}); err != nil {
				t.Fatal(err)
			}
			return Problem{Step: CheckPacks, Severity: SeverityWarning, Kind: ProblemStray, Object: "data/zz/not-a-pack"}
		}, false},
		{"one full slot left", func(t *testing.T, d *dataRepo) Problem {
			d.remove(t, slotKey(d.other.ID))
			return Problem{Step: CheckSlots, Severity: SeverityWarning, Kind: ProblemSlots}
		}, false},
		{"slot corrupted", func(t *testing.T, d *dataRepo) Problem {
			d.mutate(t, slotKey(d.other.ID), func([]byte) []byte { return []byte("{") })
			return Problem{Step: CheckSlots, Severity: SeverityError, Kind: ProblemCorrupt, Object: slotKey(d.other.ID)}
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDataRepo(t)
			want := tt.damage(t, d)
			rep := check(t, d.open(t), CheckOptions{Full: true})
			if want != (Problem{}) && !hasProblem(rep, want) {
				t.Errorf("no problem like %+v in:\n%s", want, problemsText(rep))
			}
			if (rep.Errors() > 0) != tt.errors {
				t.Errorf("Errors() = %d, want errors: %v\n%s", rep.Errors(), tt.errors, problemsText(rep))
			}
		})
	}
}

// TestCheckUnindexedPack: a pack no index file lists is a warning, and a
// rebuilt index clears it.
func TestCheckUnindexedPack(t *testing.T) {
	d := newDataRepo(t)
	ctx := context.Background()
	packA, packB := d.packOf(t, d.snapA), d.packOf(t, d.snapB)
	r := d.open(t)
	// Leave one index file listing a's pack only, so b's pack is unindexed.
	var keep []packRecord
	if err := r.eachIndexFile(ctx, func(_ string, packs []packRecord, err error) error {
		for _, p := range packs {
			if packKey(p.id) == packA {
				keep = append(keep, p)
			}
		}
		return err
	}); err != nil || len(keep) != 1 {
		t.Fatalf("records for a's pack: %d, %v", len(keep), err)
	}
	for _, k := range d.keys(t, indexPrefix) {
		d.remove(t, k)
	}
	c, err := compress.New(compress.Default, indexMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := r.writeIndex(ctx, c, keep); err != nil {
		t.Fatal(err)
	}
	rep := check(t, d.open(t), CheckOptions{Full: true})
	if !hasProblem(rep, Problem{Step: CheckPacks, Severity: SeverityWarning, Kind: ProblemUnindexed, Object: packB}) ||
		!hasProblem(rep, Problem{Step: CheckSnapshots, Severity: SeverityError, Kind: ProblemMissing}) {
		t.Errorf("want an unindexed-pack warning and b's snapshot missing chunks:\n%s", problemsText(rep))
	}
	if _, err := d.open(t).RebuildIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if rep := check(t, d.open(t), CheckOptions{Full: true}); len(rep.Problems) != 0 {
		t.Errorf("problems after rebuilding the index:\n%s", problemsText(rep))
	}
}

func TestCheckTarget(t *testing.T) {
	d := newDataRepo(t)
	d.flip(t, d.packOf(t, d.snapB), 100)
	d.flip(t, d.opKey, -3)
	r := d.open(t)
	rep := check(t, r, CheckOptions{Target: d.tidA, Full: true})
	if len(rep.Problems) != 0 || rep.Snapshots != 1 || rep.PacksRead != 1 || rep.Segments != 1 {
		t.Errorf("target a: %+v\n%s", rep, problemsText(rep))
	}
	rep = check(t, r, CheckOptions{Target: d.tidB, Full: true})
	if rep.Snapshots != 1 || rep.Segments != 1 ||
		!hasProblem(rep, Problem{Step: CheckData, Kind: ProblemCorrupt}) || !hasProblem(rep, Problem{Step: CheckSegments, Kind: ProblemCorrupt}) {
		t.Errorf("target b: %+v\n%s", rep, problemsText(rep))
	}
}

func TestCheckSample(t *testing.T) {
	f := newFixture(t)
	idx := loadIndex(t, f.repo)
	tid, _ := f.repo.TargetID("a")
	for i := range 10 {
		writeTargetSnap(t, f.repo, idx, tid, uint64(100+i)) // #nosec G115 -- small
	}
	r := f.reopen(t, f.full, "admin")
	packs := loadIndex(t, r).Packs()
	if packs < 10 {
		t.Fatalf("want 10 packs, got %d", packs)
	}
	for _, tt := range []struct {
		sample float64
		want   int
	}{{0.2, (packs*2 + 9) / 10}, {0.0001, 1}, {1, packs}} {
		rep := check(t, r, CheckOptions{Sample: tt.sample, Rand: rand.New(rand.NewPCG(1, 2))}) // #nosec G404 -- test
		if rep.PacksRead != tt.want {
			t.Errorf("sample %g: read %d packs, want %d", tt.sample, rep.PacksRead, tt.want)
		}
	}
}

func TestCheckInsecure(t *testing.T) {
	be, _ := newLocal(t)
	r, err := Create(context.Background(), be, NewConfig(true, time.Now()), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	idx := loadIndex(t, r)
	writeSnap(t, r, idx, newStream(9, 2<<20, nil))
	if rep := check(t, r, CheckOptions{Full: true}); len(rep.Problems) != 0 || rep.ChunksVerified == 0 {
		t.Errorf("insecure repository: %+v\n%s", rep, problemsText(rep))
	}
}
