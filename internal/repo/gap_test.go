// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestGaps(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if gaps, err := f.repo.Gaps(ctx, "0123456789abcdef"); err != nil || len(gaps) != 0 {
		t.Fatalf("no gaps yet: %v, %v", gaps, err)
	}
	t0 := time.Date(2026, 11, 26, 14, 0, 0, 0, time.UTC)
	for i, reason := range []string{"the replication slot was invalidated", "WAL removed"} {
		if err := f.repo.PutGap(ctx, Gap{Target: "0123456789abcdef", From: "1/0/3000000", To: "1/0/9000000",
			Reason: reason, Time: t0.Add(time.Duration(i) * time.Hour), Generation: 2}); err != nil {
			t.Fatal(err)
		}
	}
	r := f.reopen(t, f.full, "admin")
	gaps, err := r.Gaps(ctx, "0123456789abcdef")
	if err != nil || len(gaps) != 2 || gaps[0].Reason != "the replication slot was invalidated" || !gaps[1].Time.Equal(t0.Add(time.Hour)) || gaps[0].Generation != 2 {
		t.Errorf("gaps = %+v, %v", gaps, err)
	}
	// Gaps are metadata, encrypted like the rest.
	keys, _ := f.repo.listKeys(ctx, gapPrefix("0123456789abcdef"))
	raw, err := readAll(ctx, f.be, keys[0], 1<<20)
	if err != nil || strings.Contains(string(raw), "slot") {
		t.Errorf("a gap is stored in plaintext: %v", err)
	}
}
