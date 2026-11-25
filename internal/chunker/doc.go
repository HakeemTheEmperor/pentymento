// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Package chunker splits a stream into content-defined chunks with FastCDC
// and names each chunk by its keyed BLAKE3 hash (PRD 7.1, SDD 5.3, 6.1).
//
// Chunks are 256 KiB to 4 MiB, 1 MiB on average. Boundaries depend only on
// the content and on the repository's chunk-ID key, which seeds the gear
// table, so an insertion moves only the chunks around it, and the sequence
// of chunk sizes of known content cannot be predicted without the key.
//
// The algorithm is specified in docs/repository-format.md.
package chunker
