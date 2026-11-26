// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestSnapshotParts writes a snapshot of several streams, like a Postgres
// backup with a tablespace, and reads each back.
func TestSnapshotParts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	idx := loadIndex(t, f.repo)
	sw, err := f.repo.NewSnapshotWriter(ctx, idx, Snapshot{Target: "t1", Engine: "postgres", Method: "native", SystemID: "742"}, WriteOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer sw.Close()
	parts := []struct {
		name string
		src  func() io.Reader
	}{
		{"base.tar", func() io.Reader { return newStream(31, 3<<20, nil) }},
		{"16384.tar", func() io.Reader { return newStream(32, 2<<20, nil) }},
		{"empty.tar", func() io.Reader { return strings.NewReader("") }},
		{"backup_manifest", func() io.Reader { return strings.NewReader(`{"PostgreSQL-Backup-Manifest-Version": 1}`) }},
	}
	for _, p := range parts {
		if err := sw.WritePart(p.name, p.src()); err != nil {
			t.Fatal(err)
		}
	}
	if err := sw.WritePart("base.tar", strings.NewReader("x")); err == nil {
		t.Error("a part name was accepted twice")
	}
	s, st, err := sw.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "base.tar" || s.Size != 3<<20 || len(s.Parts) != 3 || s.Parts[0].Name != "16384.tar" || s.Parts[0].Size != 2<<20 ||
		s.Parts[1].Size != 0 || s.Parts[1].List == nil || s.Method != "native" || s.SystemID != "742" {
		t.Errorf("snapshot = %+v", s)
	}
	if s.TotalSize() != st.Bytes {
		t.Errorf("TotalSize = %d, stats %d", s.TotalSize(), st.Bytes)
	}
	if _, _, err := sw.Commit(); err == nil {
		t.Error("a second Commit succeeded")
	}

	r := f.reopen(t, f.full, "admin")
	loaded, err := r.LoadSnapshot(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	ridx := loadIndex(t, r)
	for _, p := range parts {
		var got bytes.Buffer
		if err := r.ReadPart(ctx, ridx, loaded, p.name, &got); err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		want, _ := io.ReadAll(p.src())
		if !bytes.Equal(got.Bytes(), want) {
			t.Errorf("%s reads back %d bytes, want %d", p.name, got.Len(), len(want))
		}
	}
	if err := r.ReadPart(ctx, ridx, loaded, "nope", io.Discard); err == nil {
		t.Error("reading a missing part succeeded")
	}
	if rep := check(t, r, CheckOptions{Full: true}); rep.Errors() != 0 || rep.SnapshotChunks != st.Chunks {
		t.Errorf("check: %d errors, %d chunks (wrote %d)\n%s", rep.Errors(), rep.SnapshotChunks, st.Chunks, problemsText(rep))
	}
}

// TestSnapshotPartFailure: a part that fails leaves nothing committed.
func TestSnapshotPartFailure(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sw, err := f.repo.NewSnapshotWriter(ctx, loadIndex(t, f.repo), Snapshot{Target: "t1"}, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.WritePart("base.tar", newStream(33, 1<<20, nil)); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("stream broke")
	if err := sw.WritePart("16384.tar", io.MultiReader(newStream(34, 1<<20, nil), &failReader{err: boom})); !errors.Is(err, boom) {
		t.Fatalf("WritePart = %v", err)
	}
	if err := sw.WritePart("backup_manifest", strings.NewReader("{}")); err == nil {
		t.Error("a part was accepted after a failure")
	}
	sw.Close()
	if _, _, err := sw.Commit(); err == nil {
		t.Error("Commit after Close succeeded")
	}
	if snaps, err := f.repo.Snapshots(ctx); err != nil || len(snaps) != 0 {
		t.Errorf("snapshots after a failure: %d, %v", len(snaps), err)
	}
	sw2, _ := f.repo.NewSnapshotWriter(ctx, loadIndex(t, f.repo), Snapshot{}, WriteOptions{})
	defer sw2.Close()
	if _, _, err := sw2.Commit(); err == nil {
		t.Error("a snapshot with no parts was committed")
	}
}
