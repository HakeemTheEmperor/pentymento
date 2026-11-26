// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package lock

import (
	"context"
	"io"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// overwriting ignores IfNotExists, like storage without conditional
// create.
type overwriting struct{ backend.Backend }

func (o overwriting) Put(ctx context.Context, key string, r io.Reader, _ backend.PutOptions) error {
	return o.Backend.Put(ctx, key, r, backend.PutOptions{})
}

func TestConditionalCreateProbe(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(backend.Backend) backend.Backend
		want bool
	}{
		{"honoured", func(b backend.Backend) backend.Backend { return b }, true},
		{"ignored", func(b backend.Backend) backend.Backend { return overwriting{b} }, false},
		// Refused, and emulated by check-then-put: the second create fails
		// as on real conditional create, but the capability is gone.
		{"emulated", backend.WithoutConditionalCreate, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be, _ := newBackend(t)
			got, err := TestConditionalCreate(ctx, tc.wrap(be))
			if err != nil || got != tc.want {
				t.Fatalf("TestConditionalCreate = %v, %v; want %v", got, err, tc.want)
			}
			// The probe never counts as a lock, and it is cleaned up.
			left := 0
			_ = be.List(ctx, locksPrefix, func(backend.ObjectInfo) error { left++; return nil })
			if left != 0 {
				t.Errorf("%d objects left under %s", left, locksPrefix)
			}
		})
	}
}
