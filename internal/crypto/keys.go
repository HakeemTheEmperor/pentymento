// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package crypto

import (
	"crypto/rand"
	"fmt"
	"log/slog"

	"filippo.io/age"
)

// KeySize is the size of every symmetric key: 256 bits.
const KeySize = 32

const redacted = "[redacted]"

// Key is a 256-bit symmetric key. Its bytes are captured by a closure, so
// no formatting path, including fmt's reflection over unexported fields,
// can print them.
type Key struct {
	v func() *[KeySize]byte
}

// NewKey returns a random key.
func NewKey() Key {
	var k [KeySize]byte
	if _, err := rand.Read(k[:]); err != nil {
		panic(err) // crypto/rand.Read does not fail
	}
	return keyOf(&k)
}

// KeyFromBytes returns a key holding a copy of b, which must be 32 bytes.
func KeyFromBytes(b []byte) (Key, error) {
	if len(b) != KeySize {
		return Key{}, fmt.Errorf("key is %d bytes, want %d", len(b), KeySize)
	}
	var k [KeySize]byte
	copy(k[:], b)
	return keyOf(&k), nil
}

func keyOf(k *[KeySize]byte) Key { return Key{v: func() *[KeySize]byte { return k }} }

// IsZero reports whether the key is unset.
func (k Key) IsZero() bool { return k.v == nil }

// Bytes returns the key's bytes. Never log, print or store them.
func (k Key) Bytes() []byte {
	if k.v == nil {
		return nil
	}
	return k.v()[:]
}

// Array returns the key as an array, for APIs that take one.
func (k Key) Array() [KeySize]byte {
	if k.v == nil {
		return [KeySize]byte{}
	}
	return *k.v()
}

// Clear zeroes the key's bytes.
func (k Key) Clear() { clear(k.Bytes()) }

// String returns "[redacted]".
func (Key) String() string { return redacted }

// GoString returns "[redacted]".
func (Key) GoString() string { return redacted }

// Format prints "[redacted]" for every verb.
func (Key) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// MarshalJSON encodes "[redacted]".
func (Key) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// LogValue logs "[redacted]".
func (Key) LogValue() slog.Value { return slog.StringValue(redacted) }

// PublicKey is the repository's public key: an age X25519 recipient. It is
// not secret.
type PublicKey struct {
	r *age.X25519Recipient
}

// ParsePublicKey parses an age X25519 recipient ("age1…").
func ParsePublicKey(s string) (PublicKey, error) {
	r, err := age.ParseX25519Recipient(s)
	if err != nil {
		return PublicKey{}, err
	}
	return PublicKey{r: r}, nil
}

// String returns the recipient ("age1…").
func (p PublicKey) String() string {
	if p.r == nil {
		return ""
	}
	return p.r.String()
}

// IsZero reports whether the key is unset.
func (p PublicKey) IsZero() bool { return p.r == nil }

// PrivateKey is the repository's private key: an age X25519 identity. Like
// Key, it never prints.
type PrivateKey struct {
	v func() *age.X25519Identity
}

// NewPrivateKey generates a key pair.
func NewPrivateKey() (PrivateKey, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return PrivateKey{}, err
	}
	return privateOf(id), nil
}

// parsePrivateKey parses an age identity ("AGE-SECRET-KEY-1…").
func parsePrivateKey(s string) (PrivateKey, error) {
	id, err := age.ParseX25519Identity(s)
	if err != nil {
		return PrivateKey{}, fmt.Errorf("not an age X25519 identity")
	}
	return privateOf(id), nil
}

func privateOf(id *age.X25519Identity) PrivateKey {
	return PrivateKey{v: func() *age.X25519Identity { return id }}
}

// IsZero reports whether the key is unset.
func (k PrivateKey) IsZero() bool { return k.v == nil }

// Public returns the matching public key.
func (k PrivateKey) Public() PublicKey { return PublicKey{r: k.v().Recipient()} }

func (k PrivateKey) identity() *age.X25519Identity { return k.v() }

// String returns "[redacted]".
func (PrivateKey) String() string { return redacted }

// GoString returns "[redacted]".
func (PrivateKey) GoString() string { return redacted }

// Format prints "[redacted]" for every verb.
func (PrivateKey) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// MarshalJSON encodes "[redacted]".
func (PrivateKey) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// LogValue logs "[redacted]".
func (PrivateKey) LogValue() slog.Value { return slog.StringValue(redacted) }
