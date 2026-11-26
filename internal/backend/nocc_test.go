// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package backend_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/backendtest"
)

func TestWithoutConditionalCreate(t *testing.T) {
	backendtest.Run(t, func(*testing.T) backend.Backend {
		return backend.WithoutConditionalCreate(backendtest.NewMemory())
	})
	be := backend.WithoutConditionalCreate(backendtest.NewMemory())
	if be.Capabilities().ConditionalCreate || !be.Capabilities().StrongConsistency {
		t.Errorf("capabilities = %+v", be.Capabilities())
	}
	if backend.WithoutConditionalCreate(be) != be {
		t.Error("wrapping twice wraps again")
	}
	ctx := context.Background()
	opts := backend.PutOptions{IfNotExists: true}
	if err := be.Put(ctx, "locks/a", bytes.NewReader([]byte("1")), opts); err != nil {
		t.Fatal(err)
	}
	if err := be.Put(ctx, "locks/a", bytes.NewReader([]byte("2")), opts); !errors.Is(err, backend.ErrExists) {
		t.Errorf("second create = %v", err)
	}
}
