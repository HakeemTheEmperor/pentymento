// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package crypto

import (
	"bytes"
	"errors"
	"testing"
)

func newPriv(t *testing.T) PrivateKey {
	t.Helper()
	k, err := NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKeyring(t *testing.T) {
	old, cur, other := newPriv(t), newPriv(t), newPriv(t)
	ring := Keyring{cur, old}
	dk := NewKey()
	wrappedOld, err := WrapKey(old.Public(), dk)
	if err != nil {
		t.Fatal(err)
	}
	got, i, err := ring.Unwrap(wrappedOld)
	if err != nil || i != 1 || !bytes.Equal(got.Bytes(), dk.Bytes()) {
		t.Fatalf("Unwrap(old) = %d, %v", i, err)
	}
	// Re-wrapping moves it to the current key; once current it is kept.
	rewrapped, changed, err := ring.Rewrap(cur.Public(), wrappedOld)
	if err != nil || !changed {
		t.Fatalf("Rewrap = %v, %v", changed, err)
	}
	if _, i, err := ring.Unwrap(rewrapped); err != nil || i != 0 {
		t.Errorf("rewrapped opens with key %d, %v", i, err)
	}
	again, changed, err := ring.Rewrap(cur.Public(), rewrapped)
	if err != nil || changed || !bytes.Equal(again, rewrapped) {
		t.Errorf("Rewrap of a current key changed it: %v, %v", changed, err)
	}
	// A key wrapped to none of them does not open.
	wrappedOther, _ := WrapKey(other.Public(), dk)
	if _, _, err := ring.Unwrap(wrappedOther); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Unwrap(other) = %v", err)
	}
	if _, _, err := ring.Rewrap(cur.Public(), wrappedOther); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Rewrap(other) = %v", err)
	}
	if !(Keyring{}).Current().IsZero() || ring.Current().Public().String() != cur.Public().String() {
		t.Error("Current")
	}
}

func TestKeyringObjects(t *testing.T) {
	old, cur := newPriv(t), newPriv(t)
	ring := Keyring{cur, old}
	sealed, err := SealObject(old.Public(), "wal/x", []byte("segment"))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := ring.OpenObject("wal/x", sealed); err != nil || string(b) != "segment" {
		t.Fatalf("OpenObject = %q, %v", b, err)
	}
	if _, err := ring.OpenObject("wal/y", sealed); err == nil {
		t.Error("an object opened under another key")
	}
	out, changed, err := ring.RewrapObject(cur.Public(), sealed)
	if err != nil || !changed {
		t.Fatalf("RewrapObject = %v, %v", changed, err)
	}
	if b, err := (Keyring{cur}).OpenObject("wal/x", out); err != nil || string(b) != "segment" {
		t.Errorf("re-wrapped object with the current key alone = %q, %v", b, err)
	}
	if _, changed, _ := ring.RewrapObject(cur.Public(), out); changed {
		t.Error("RewrapObject changed a current object")
	}
	if _, _, err := ring.RewrapObject(cur.Public(), []byte("junk")); err == nil {
		t.Error("RewrapObject accepted junk")
	}
}
