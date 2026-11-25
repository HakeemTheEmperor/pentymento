// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package secrets

import (
	"fmt"
	"log/slog"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Scheme is a secret reference scheme.
type Scheme string

// Reference schemes. Plaintext is a value written directly in the config,
// allowed only with allow_plaintext_secrets.
const (
	SchemeEnv       Scheme = "env"
	SchemeFile      Scheme = "file"
	SchemeKeychain  Scheme = "keychain"
	SchemeCmd       Scheme = "cmd"
	SchemePlaintext Scheme = "plaintext"
)

// plaintextLabel is what a plaintext reference prints instead of its value.
const plaintextLabel = "[plaintext secret]"

// Ref is a secret reference from the configuration. It holds no secret
// unless it is a plaintext value, which it never prints.
type Ref struct {
	scheme Scheme
	arg    string // reference argument; empty for plaintext
	plain  Secret // the value, for plaintext only
	// Line is the line in the config file the reference was read from, or
	// 0 when it did not come from a file.
	Line int
}

// ParseRef parses a configured value: env:NAME, file:/path,
// keychain:service/item or cmd:<helper>. Any other non-empty value is
// plaintext.
func ParseRef(s string) (Ref, error) {
	if s == "" {
		return Ref{}, fmt.Errorf("empty secret reference")
	}
	scheme, arg, ok := strings.Cut(s, ":")
	if ok {
		switch Scheme(scheme) {
		case SchemeEnv, SchemeFile, SchemeKeychain, SchemeCmd:
			if strings.TrimSpace(arg) == "" {
				return Ref{}, fmt.Errorf("secret reference %s: is missing its argument", scheme)
			}
			if Scheme(scheme) == SchemeKeychain {
				service, item, _ := strings.Cut(arg, "/")
				if service == "" || item == "" {
					return Ref{}, fmt.Errorf("secret reference keychain:%s must have the form keychain:service/item", arg)
				}
			}
			return Ref{scheme: Scheme(scheme), arg: arg}, nil
		}
	}
	return Ref{scheme: SchemePlaintext, plain: New([]byte(s))}, nil
}

// Scheme returns the reference scheme.
func (r Ref) Scheme() Scheme { return r.scheme }

// IsZero reports whether the reference is unset.
func (r Ref) IsZero() bool { return r.scheme == "" }

// IsPlaintext reports whether the value was written directly in the config.
func (r Ref) IsPlaintext() bool { return r.scheme == SchemePlaintext }

// String returns the reference as written, such as "env:PGPASSWORD", which
// is safe to show and to record in the journal. A plaintext value prints
// as "[plaintext secret]".
func (r Ref) String() string {
	switch r.scheme {
	case "":
		return ""
	case SchemePlaintext:
		return plaintextLabel
	}
	return string(r.scheme) + ":" + r.arg
}

// GoString is String, for %#v.
func (r Ref) GoString() string { return r.String() }

// Format prints String for every verb.
func (r Ref) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(r.String())) }

// MarshalJSON encodes the reference as a string.
func (r Ref) MarshalJSON() ([]byte, error) { return []byte(fmt.Sprintf("%q", r.String())), nil }

// MarshalText encodes the reference as text.
func (r Ref) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

// MarshalYAML encodes the reference as a string.
func (r Ref) MarshalYAML() (any, error) { return r.String(), nil }

// LogValue logs the reference as String.
func (r Ref) LogValue() slog.Value { return slog.StringValue(r.String()) }

// UnmarshalYAML reads a reference from a YAML scalar and records its line.
func (r *Ref) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a secret must be a reference such as env:NAME", n.Line)
	}
	p, err := ParseRef(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	p.Line = n.Line
	*r = p
	return nil
}
