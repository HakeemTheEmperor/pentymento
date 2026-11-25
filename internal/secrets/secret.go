// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package secrets

import (
	"fmt"
	"log/slog"
)

// Redacted is what every formatting path prints instead of a secret.
const Redacted = "[redacted]"

// Secret is a resolved secret value. Its value is reachable only through
// Expose; every formatting, encoding and logging path prints Redacted.
//
// The bytes are captured by a closure rather than held in a field: fmt and
// reflection print a function only as an address, so even a struct holding
// a Secret in an unexported field, printed with any verb, cannot reveal
// them.
type Secret struct {
	v func() []byte
}

// New returns a Secret holding a copy of b.
func New(b []byte) Secret {
	c := append([]byte(nil), b...)
	return Secret{v: func() []byte { return c }}
}

// Expose returns the secret's bytes. Pass them only to the code that uses
// the secret (a driver, a credential file); never log, print or store them.
func (s Secret) Expose() []byte {
	if s.v == nil {
		return nil
	}
	return s.v()
}

// IsEmpty reports whether the secret holds no bytes.
func (s Secret) IsEmpty() bool { return len(s.Expose()) == 0 }

// Clear zeroes the secret's bytes.
func (s Secret) Clear() { clear(s.Expose()) }

// String returns Redacted.
func (Secret) String() string { return Redacted }

// GoString returns Redacted, for %#v.
func (Secret) GoString() string { return Redacted }

// Format prints Redacted for every verb, including %x and %q.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// MarshalJSON encodes the secret as the string "[redacted]".
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// MarshalText encodes the secret as "[redacted]".
func (Secret) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// MarshalYAML encodes the secret as "[redacted]".
func (Secret) MarshalYAML() (any, error) { return Redacted, nil }

// LogValue logs the secret as "[redacted]".
func (Secret) LogValue() slog.Value { return slog.StringValue(Redacted) }
