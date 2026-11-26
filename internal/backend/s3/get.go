// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
)

// bodyAttempts bounds how often one block is fetched again after its body
// broke off mid-read; the SDK retries only the request itself.
const bodyAttempts = 3

// Get returns the object, or the part in rng. It reads in ranged blocks:
// the first is fetched before Get returns (so a missing object is
// reported at once), the rest in parallel, up to the read-ahead ahead of
// the reader, and delivered in order. Every block after the first must
// come from the same version of the object (If-Match).
func (b *Backend) Get(ctx context.Context, key string, rng *backend.Range) (io.ReadCloser, error) {
	if err := backend.ValidateKey(key); err != nil {
		return nil, err
	}
	var offset, length int64 = 0, -1
	if rng != nil {
		if rng.Offset < 0 || rng.Length < -1 {
			return nil, fmt.Errorf("%w: offset %d, length %d", backend.ErrInvalidRange, rng.Offset, rng.Length)
		}
		offset, length = rng.Offset, rng.Length
	}
	if length == 0 {
		// HTTP cannot ask for zero bytes; check the offset and return none.
		return b.empty(ctx, key, offset)
	}
	n := b.opts.ReadBlock
	if length > 0 && length < n {
		n = length
	}
	data, size, etag, err := b.fetch(ctx, key, offset, n, "")
	if errors.Is(err, errInvalidRange) {
		return b.empty(ctx, key, offset)
	}
	if err != nil {
		return nil, err
	}
	end := size
	if length >= 0 && offset+length < end {
		end = offset + length
	}
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	r := &blockReader{cur: data, cancel: func() { stop(); cancel() }, queue: make(chan *block, b.opts.ReadAhead)}
	next := offset + int64(len(data))
	if next >= end {
		close(r.queue)
		return r, nil
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(r.queue)
		for off := next; off < end; off += b.opts.ReadBlock {
			bl := &block{done: make(chan struct{})}
			select {
			case r.queue <- bl:
			case <-rctx.Done():
				return
			}
			n := min(b.opts.ReadBlock, end-off)
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				defer close(bl.done)
				bl.data, _, _, bl.err = b.fetch(rctx, key, off, n, etag)
			}()
		}
	}()
	return r, nil
}

// empty returns an empty reader when offset is exactly the object's size,
// and ErrInvalidRange when it is past it.
func (b *Backend) empty(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	info, err := b.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	if offset > info.Size {
		return nil, fmt.Errorf("%w: offset %d, object size %d", backend.ErrInvalidRange, offset, info.Size)
	}
	return io.NopCloser(strings.NewReader("")), nil
}

// fetch reads n bytes at offset (fewer at the object's end) and returns
// them with the object's size and ETag. A body that breaks off is fetched
// again.
func (b *Backend) fetch(ctx context.Context, key string, offset, n int64, etag string) ([]byte, int64, string, error) {
	var lastErr error
	for attempt := 0; attempt < bodyAttempts; attempt++ {
		if attempt > 0 {
			d, _ := jitterBackoff{min: b.opts.MinBackoff, max: b.opts.MaxBackoff}.BackoffDelay(attempt, lastErr)
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return nil, 0, "", ctx.Err()
			}
		}
		data, size, tag, retry, err := b.fetchOnce(ctx, key, offset, n, etag)
		if err == nil || !retry {
			return data, size, tag, err
		}
		lastErr = err
	}
	return nil, 0, "", lastErr
}

func (b *Backend) fetchOnce(ctx context.Context, key string, offset, n int64, etag string) (data []byte, size int64, tag string, retry bool, err error) {
	if err := b.acquire(ctx); err != nil {
		return nil, 0, "", false, err
	}
	defer b.release()
	in := &s3.GetObjectInput{
		Bucket: aws.String(b.opts.Bucket), Key: aws.String(b.root + key),
		Range: aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+n-1)),
	}
	if etag != "" {
		in.IfMatch = aws.String(etag)
	}
	out, err := b.client.GetObject(ctx, in)
	if err != nil {
		if statusCode(err) == 412 {
			return nil, 0, "", false, fmt.Errorf("get %s: the object changed while it was read", key)
		}
		return nil, 0, "", false, mapError("get", key, err, false)
	}
	defer func() { _ = out.Body.Close() }()
	size, err = totalSize(aws.ToString(out.ContentRange))
	if err != nil {
		return nil, 0, "", false, fmt.Errorf("get %s: %w", key, err)
	}
	want := min(n, size-offset)
	data = make([]byte, want)
	if _, err := io.ReadFull(out.Body, data); err != nil {
		if ctx.Err() != nil {
			return nil, 0, "", false, ctx.Err()
		}
		return nil, 0, "", true, fmt.Errorf("get %s: reading the body: %w", key, err)
	}
	return data, size, aws.ToString(out.ETag), false, nil
}

// totalSize parses the size from a Content-Range such as
// "bytes 0-8388607/123456789".
func totalSize(cr string) (int64, error) {
	_, total, ok := strings.Cut(cr, "/")
	if !ok || total == "*" {
		return 0, fmt.Errorf("unexpected Content-Range %q", cr)
	}
	return strconv.ParseInt(total, 10, 64)
}

// block is one ranged read, in flight or done.
type block struct {
	done chan struct{}
	data []byte
	err  error
}

// blockReader delivers blocks in order.
type blockReader struct {
	cur    []byte
	queue  chan *block
	err    error
	cancel func()
	wg     sync.WaitGroup
	closed bool
}

func (r *blockReader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		bl, ok := <-r.queue
		if !ok {
			r.err = io.EOF
			continue
		}
		<-bl.done
		if bl.err != nil {
			r.err = bl.err
			continue
		}
		r.cur = bl.data
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

// Close stops the reads ahead and waits for them.
func (r *blockReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.cancel()
	// The scheduler closes the queue once it sees the cancellation.
	for range r.queue { //nolint:revive // drained only to let the scheduler exit
	}
	r.wg.Wait()
	return nil
}
