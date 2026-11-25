// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package cliout

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestReadSecret(t *testing.T) {
	var stderr bytes.Buffer
	p := &Printer{Stderr: &stderr, Interactive: true}
	p.SetStdin(strings.NewReader("correct horse\r\nsecond\n"))
	s, err := p.ReadSecret("passphrase")
	if err != nil || string(s.Expose()) != "correct horse" {
		t.Fatalf("ReadSecret = %q, %v", s.Expose(), err)
	}
	s, err = p.ReadSecret("again")
	if err != nil || string(s.Expose()) != "second" {
		t.Fatalf("second ReadSecret = %q, %v", s.Expose(), err)
	}
	// The prompt is written; the answer never is.
	if stderr.String() != "passphrase: \nagain: \n" {
		t.Errorf("stderr = %q", stderr.String())
	}
	// End of input is an empty answer.
	s, err = p.ReadSecret("more")
	if err != nil || !s.IsEmpty() {
		t.Errorf("ReadSecret at EOF = %q, %v", s.Expose(), err)
	}
}

func TestReadSecretNotInteractive(t *testing.T) {
	var stderr bytes.Buffer
	p := &Printer{Stderr: &stderr}
	p.SetStdin(strings.NewReader("secret\n"))
	if _, err := p.ReadSecret("passphrase"); !errors.Is(err, ErrNotInteractive) {
		t.Errorf("ReadSecret without a terminal = %v, want ErrNotInteractive", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}
