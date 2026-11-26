// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package engine

import (
	"errors"
	"slices"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
)

// fake is an engine with only a name; the registry needs no more.
type fake struct {
	Engine
	env Env
}

func (fake) Name() string { return "fake" }

func TestRegistry(t *testing.T) {
	if _, err := New("fake", Env{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an unregistered engine: %v", err)
	}
	Register("fake", func(env Env) Engine { return fake{env: env} })
	t.Cleanup(func() { mu.Lock(); delete(factories, "fake"); mu.Unlock() })
	e, err := New("fake", Env{Tools: config.Tools{MongoDB: []string{"/opt/mongo"}}})
	if err != nil || e.Name() != "fake" {
		t.Fatalf("New = %v, %v", e, err)
	}
	if got := e.(fake).env.Tools.MongoDB; !slices.Equal(got, []string{"/opt/mongo"}) {
		t.Errorf("the factory got tools %v", got)
	}
	if !slices.Contains(Names(), "fake") {
		t.Errorf("Names = %v", Names())
	}
	defer func() {
		if recover() == nil {
			t.Error("registering a name twice did not panic")
		}
	}()
	Register("fake", func(Env) Engine { return fake{} })
}
