// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package backendtest

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// Memory is a backend that keeps objects in memory, for tests that need
// many fast repository operations. It passes the same suite as the real
// backends: conditional create, strong consistency, whole-object writes.
type Memory struct {
	mu   sync.Mutex
	objs map[string]memObject
}

type memObject struct {
	data []byte
	mod  time.Time
}

var _ backend.Backend = (*Memory)(nil)

// NewMemory returns an empty in-memory backend.
func NewMemory() *Memory { return &Memory{objs: map[string]memObject{}} }

// Put reads r whole, then stores it; a reader that fails stores nothing.
func (m *Memory) Put(ctx context.Context, key string, r io.Reader, opts backend.PutOptions) error {
	if err := backend.ValidateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[key]; ok && opts.IfNotExists {
		return backend.ErrExists
	}
	m.objs[key] = memObject{data: b, mod: time.Now()}
	return nil
}

// Get returns the object or a range of it.
func (m *Memory) Get(ctx context.Context, key string, rng *backend.Range) (io.ReadCloser, error) {
	if err := backend.ValidateKey(key); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	o, ok := m.objs[key]
	m.mu.Unlock()
	if !ok {
		return nil, backend.ErrNotFound
	}
	off, n, err := backend.CheckRange(rng, int64(len(o.data)))
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(o.data[off : off+n])), nil
}

// Stat returns the object's size and modification time.
func (m *Memory) Stat(ctx context.Context, key string) (backend.ObjectInfo, error) {
	if err := backend.ValidateKey(key); err != nil {
		return backend.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return backend.ObjectInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return backend.ObjectInfo{}, backend.ErrNotFound
	}
	return backend.ObjectInfo{Key: key, Size: int64(len(o.data)), ModTime: o.mod}, nil
}

// List calls fn for every object under prefix, in key order.
func (m *Memory) List(ctx context.Context, prefix string, fn func(backend.ObjectInfo) error) error {
	if err := backend.ValidatePrefix(prefix); err != nil {
		return err
	}
	m.mu.Lock()
	var infos []backend.ObjectInfo
	for k, o := range m.objs {
		if strings.HasPrefix(k, prefix) {
			infos = append(infos, backend.ObjectInfo{Key: k, Size: int64(len(o.data)), ModTime: o.mod})
		}
	}
	m.mu.Unlock()
	sort.Slice(infos, func(i, j int) bool { return infos[i].Key < infos[j].Key })
	for _, oi := range infos {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(oi); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes the object; a missing key is not an error.
func (m *Memory) Delete(ctx context.Context, key string) error {
	if err := backend.ValidateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

// Capabilities reports conditional create and strong consistency.
func (m *Memory) Capabilities() backend.Capabilities {
	return backend.Capabilities{ConditionalCreate: true, StrongConsistency: true}
}
