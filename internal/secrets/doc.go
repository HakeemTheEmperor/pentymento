// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Package secrets holds secret references and the Secret type (PRD 8.3,
// SDD 4.4, 8.5).
//
// Configuration holds secrets only as references: env:NAME, file:/path,
// keychain:service/item or cmd:<helper>. A reference is resolved by a
// Resolver at the moment of use, so a rotated secret takes effect on the
// next connection without a restart.
//
// A resolved Secret never prints its value: String, Format, MarshalJSON,
// MarshalText, MarshalYAML and LogValue all return "[redacted]". Scrub and
// ScrubError strip secret values and password patterns from error text
// before it is shown or logged.
//
// Go's garbage collector cannot guarantee that secret bytes are wiped from
// memory; Clear zeroes the buffers this package controls.
package secrets
