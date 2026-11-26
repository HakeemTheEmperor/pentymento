// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// WithoutConditionalCreate returns be for storage that does not honour
// conditional create (PRD 7.8): a Put with IfNotExists checks that the key
// is absent, then writes, and Capabilities reports no conditional create,
// so locks and leases use their fallback. The check and the write are two
// steps, so two racing writers can both succeed.
func WithoutConditionalCreate(be Backend) Backend {
	if _, ok := be.(noConditionalCreate); ok {
		return be
	}
	return noConditionalCreate{be}
}

type noConditionalCreate struct{ Backend }

func (n noConditionalCreate) Put(ctx context.Context, key string, r io.Reader, opts PutOptions) error {
	if opts.IfNotExists {
		_, err := n.Stat(ctx, key)
		switch {
		case err == nil:
			return fmt.Errorf("%w: %s", ErrExists, key)
		case !errors.Is(err, ErrNotFound):
			return err
		}
		opts.IfNotExists = false
	}
	return n.Backend.Put(ctx, key, r, opts)
}

func (n noConditionalCreate) Capabilities() Capabilities {
	c := n.Backend.Capabilities()
	c.ConditionalCreate = false
	return c
}
