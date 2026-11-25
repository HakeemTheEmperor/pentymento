// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Package crypto implements Pentymento's write-only encryption and key
// slots (PRD 8.1, 8.2; SDD 8.1–8.4). The design is specified for review in
// docs/crypto.md.
//
// Primitives come only from golang.org/x/crypto (XChaCha20-Poly1305,
// Argon2id) and filippo.io/age (X25519 key wrapping); nothing here
// implements a cryptographic primitive.
//
//   - Chunks are encrypted one by one with XChaCha20-Poly1305 under a random
//     per-pack data key, with the chunk ID as associated data.
//   - Data keys are wrapped to the repository's age X25519 public key; only
//     a full key slot reaches the matching private key.
//   - Metadata is encrypted with the metadata key, with its object key as
//     associated data.
//   - Full slots protect a master key, which encrypts the private key; the
//     agent slot holds only the metadata and chunk-ID keys, so an agent can
//     write and deduplicate but never read.
//
// No key ever prints: every key type formats as "[redacted]".
package crypto
