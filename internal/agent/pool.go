// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package agent

import (
	"context"
	"sync/atomic"
)

// Pool bounds the work the agent's jobs do at once: compression,
// encryption and uploads share it (SDD 9.1), so a backup and a capture
// loop together use no more than the agent allows.
type Pool struct {
	slots   chan struct{}
	waiting atomic.Int64
}

// NewPool returns a pool of n slots (at least one).
func NewPool(n int) *Pool {
	if n < 1 {
		n = 1
	}
	return &Pool{slots: make(chan struct{}, n)}
}

// Size is the number of slots.
func (p *Pool) Size() int { return cap(p.slots) }

// Waiting is how many tasks wait for a slot: the upload queue depth.
func (p *Pool) Waiting() int { return int(p.waiting.Load()) }

// Do runs fn once a slot is free, or returns ctx's error.
func (p *Pool) Do(ctx context.Context, fn func() error) error {
	p.waiting.Add(1)
	select {
	case p.slots <- struct{}{}:
		p.waiting.Add(-1)
	case <-ctx.Done():
		p.waiting.Add(-1)
		return ctx.Err()
	}
	defer func() { <-p.slots }()
	return fn()
}
