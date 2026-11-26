// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package engine

import (
	"slices"
	"testing"
)

// fake is an engine with only a name; the registry needs no more.
type fake struct{ Engine }

func (fake) Name() string { return "fake" }

func TestRegistry(t *testing.T) {
	if _, err := Get("fake"); err == nil {
		t.Fatal("an unregistered engine was found")
	}
	Register(fake{})
	t.Cleanup(func() { mu.Lock(); delete(engines, "fake"); mu.Unlock() })
	if e, err := Get("fake"); err != nil || e.Name() != "fake" {
		t.Errorf("Get = %v, %v", e, err)
	}
	if !slices.Contains(Names(), "fake") {
		t.Errorf("Names = %v", Names())
	}
	defer func() {
		if recover() == nil {
			t.Error("registering a name twice did not panic")
		}
	}()
	Register(fake{})
}
