// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Package backend defines the storage backend interface (SDD 4.1). It is
// deliberately small: packs, the index, locks, leases and log segments are
// all built on these calls, identically for every backend.
//
// Every backend shares these semantics, which the conformance suite in
// internal/backend/backendtest enforces:
//
//   - Get and Stat of a missing key return ErrNotFound.
//   - Put with IfNotExists returns ErrExists when the key exists; it is
//     atomic, so concurrent creators of one key see exactly one success.
//   - A Put that fails or is interrupted never leaves a partial object.
//   - Delete of a missing key succeeds.
//   - List matches a plain string prefix and calls fn in lexicographic key
//     order.
//   - Keys are "/"-separated segments of A–Z a–z 0–9 . _ -, never starting
//     with "." and never "..".
package backend
