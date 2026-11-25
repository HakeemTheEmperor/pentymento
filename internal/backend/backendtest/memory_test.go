// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package backendtest

import (
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// TestMemory holds the in-memory backend to the same contract as the real
// ones.
func TestMemory(t *testing.T) {
	Run(t, func(*testing.T) backend.Backend { return NewMemory() })
}
