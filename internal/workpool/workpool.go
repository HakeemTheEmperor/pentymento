// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Package workpool runs CPU-heavy pipeline stages (hashing, compression,
// encryption) on a bounded number of goroutines while keeping results in
// input order (SDD 6.1 step 5, SDD 9.1), since a snapshot is an ordered list
// of chunks.
package workpool

import (
	"context"
	"sync"
)

// Run reads items from src, processes up to workers of them at once with
// work, and passes the results to sink in the order src produced them.
//
// src and sink run on the calling goroutine and its helper respectively,
// never concurrently with themselves. At most 2×workers items are in
// flight, so memory stays bounded. src returns ok=false when it has no more
// items. The first error from src, work or sink, or the context's
// cancellation, stops the run; Run returns it once every goroutine it
// started has finished.
func Run[T, R any](
	ctx context.Context,
	workers int,
	src func() (item T, ok bool, err error),
	work func(ctx context.Context, item T) (R, error),
	sink func(R) error,
) error {
	if workers < 1 {
		workers = 1
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	type result struct {
		r   R
		err error
	}
	order := make(chan chan result, 2*workers)
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	sinkDone := make(chan error, 1)
	go func() {
		var first error
		for ch := range order {
			res := <-ch
			if first != nil {
				continue
			}
			if res.err == nil {
				res.err = sink(res.r)
			}
			if res.err != nil {
				first = res.err
				cancel(first)
			}
		}
		sinkDone <- first
	}()

	var srcErr error
dispatch:
	for ctx.Err() == nil {
		item, ok, err := src()
		if err != nil {
			srcErr = err
			cancel(err)
			break
		}
		if !ok {
			break
		}
		ch := make(chan result, 1)
		select {
		case order <- ch:
		case <-ctx.Done():
			break dispatch
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			ch <- result{err: context.Cause(ctx)}
			break dispatch
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r, err := work(ctx, item)
			ch <- result{r: r, err: err}
		}()
	}
	close(order)
	wg.Wait()
	sinkErr := <-sinkDone

	switch {
	case srcErr != nil:
		return srcErr
	case sinkErr != nil:
		return sinkErr
	}
	return context.Cause(ctx)
}
