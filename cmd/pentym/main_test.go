// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"testing"
)

func TestRunReturnsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("run() wrote output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
