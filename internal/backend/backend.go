// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Backend stores objects by key.
type Backend interface {
	// Put stores the content of r under key, replacing any existing object
	// unless opts.IfNotExists is set.
	Put(ctx context.Context, key string, r io.Reader, opts PutOptions) error
	// Get returns the object's content, or the part of it in rng when rng
	// is not nil.
	Get(ctx context.Context, key string, rng *Range) (io.ReadCloser, error)
	// Stat returns the object's metadata.
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	// List calls fn for every object whose key starts with prefix, in
	// lexicographic key order; an error from fn stops the listing and is
	// returned.
	List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error
	// Delete removes the object; a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// Capabilities reports what the backend supports.
	Capabilities() Capabilities
}

// PutOptions modify a Put.
type PutOptions struct {
	// IfNotExists makes the Put fail with ErrExists when the key exists.
	// The check and the write are one atomic step (conditional create).
	IfNotExists bool
}

// Range is part of an object: Length bytes from Offset. Length -1 means to
// the end. A range that runs past the end is cut short there.
type Range struct {
	Offset int64
	Length int64
}

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key     string
	Size    int64
	ModTime time.Time
}

// Capabilities are the optional guarantees a backend provides.
type Capabilities struct {
	// ConditionalCreate: Put with IfNotExists is atomic. Locks and leases
	// rely on it; without it they fall back to a weaker protocol.
	ConditionalCreate bool
	// ObjectLock: objects can be protected from deletion for a period.
	ObjectLock bool
	// StrongConsistency: a successful write is visible to every
	// subsequent read and list.
	StrongConsistency bool
}

// Errors shared by every backend; match them with errors.Is.
var (
	ErrNotFound     = errors.New("object not found")
	ErrExists       = errors.New("object already exists")
	ErrInvalidRange = errors.New("invalid range")
	ErrInvalidKey   = errors.New("invalid key")
)

// ValidateKey checks that key is "/"-separated segments of A–Z a–z 0–9
// . _ -, none empty, none starting with ".".
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty", ErrInvalidKey)
	}
	for _, seg := range strings.Split(key, "/") {
		if err := validateSegment(seg); err != nil {
			return fmt.Errorf("%w %q: %v", ErrInvalidKey, key, err)
		}
	}
	return nil
}

// ValidatePrefix checks a List prefix: every complete segment must be
// valid; the last may be partial or empty.
func ValidatePrefix(prefix string) error {
	segs := strings.Split(prefix, "/")
	for i, seg := range segs {
		last := i == len(segs)-1
		if last && seg == "" {
			continue
		}
		if err := validateSegment(seg); err != nil {
			return fmt.Errorf("%w prefix %q: %v", ErrInvalidKey, prefix, err)
		}
	}
	return nil
}

func validateSegment(seg string) error {
	if seg == "" {
		return errors.New("empty segment")
	}
	if seg[0] == '.' {
		return errors.New("a segment cannot start with '.'")
	}
	for _, c := range seg {
		if !keyChar(c) {
			return fmt.Errorf("character %q is not allowed", c)
		}
	}
	return nil
}

func keyChar(c rune) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return c == '.' || c == '_' || c == '-'
}

// CheckRange validates rng against an object of size bytes and returns the
// offset and number of bytes to read.
func CheckRange(rng *Range, size int64) (offset, n int64, err error) {
	if rng == nil {
		return 0, size, nil
	}
	if rng.Offset < 0 || rng.Length < -1 || rng.Offset > size {
		return 0, 0, fmt.Errorf("%w: offset %d, length %d, object size %d", ErrInvalidRange, rng.Offset, rng.Length, size)
	}
	n = size - rng.Offset
	if rng.Length >= 0 && rng.Length < n {
		n = rng.Length
	}
	return rng.Offset, n, nil
}
