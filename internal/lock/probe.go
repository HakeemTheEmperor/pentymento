// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package lock

import (
	"bytes"
	"context"
	"errors"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// TestConditionalCreate checks that the storage really honours conditional
// create (PRD 7.8), as init and doctor do: it creates the same object twice
// with IfNotExists. The first create must succeed and the second must fail
// with ErrExists, and the backend must still report the capability (one
// that met a refusal falls back to check-then-put); otherwise locks and
// leases must use the weaker fallback. The probe object is deleted afterwards where the
// credentials allow; prune removes it otherwise.
func TestConditionalCreate(ctx context.Context, be backend.Backend) (bool, error) {
	key := locksPrefix + "probe-cc-" + randomID()
	opts := backend.PutOptions{IfNotExists: true}
	// The two attempts write different content, as two real writers
	// would: a backend may take an identical object for its own retried
	// write.
	if err := be.Put(ctx, key, bytes.NewReader([]byte(`{"attempt":1}`)), opts); err != nil {
		return false, err
	}
	defer func() { _ = be.Delete(context.WithoutCancel(ctx), key) }()
	err := be.Put(ctx, key, bytes.NewReader([]byte(`{"attempt":2}`)), opts)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, backend.ErrExists):
		// A backend that met a refusal of conditional create during the
		// probe falls back to check-then-put, which also returns ErrExists
		// here; it then no longer reports the capability.
		return be.Capabilities().ConditionalCreate, nil
	}
	return false, err
}
