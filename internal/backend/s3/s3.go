// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// Defaults (PRD 7.8, Build Order C15).
const (
	// DefaultPartSize is the largest object sent in one PUT and the size
	// of each part of a multipart upload; packs are about this size.
	DefaultPartSize = 16 << 20
	// DefaultConcurrency bounds the requests in flight to the bucket.
	DefaultConcurrency = 16
	// DefaultReadBlock is the size of each ranged GET, and DefaultReadAhead
	// how many blocks a reader fetches ahead of the one being read.
	DefaultReadBlock = 8 << 20
	DefaultReadAhead = 4
	// DefaultMaxAttempts bounds the attempts of one request, retried on
	// throttling, 5xx and network errors with backoff from MinBackoff to
	// MaxBackoff.
	DefaultMaxAttempts = 10
	DefaultMinBackoff  = time.Second
	DefaultMaxBackoff  = 5 * time.Minute
)

// minPartSize is S3's smallest part, except the last.
const minPartSize = 5 << 20

// Options configure a Backend.
type Options struct {
	Bucket string
	// Prefix puts the repository under a "directory" of the bucket.
	Prefix string
	Region string
	// Endpoint is an S3-compatible service's URL, such as
	// https://minio.internal:9000; empty means AWS.
	Endpoint string
	// PathStyle addresses the bucket in the path (endpoint/bucket/key),
	// which many S3-compatible services need.
	PathStyle bool
	// AccessKeyID and SecretAccessKey are static credentials; when unset,
	// the AWS default credential chain is used (environment, shared
	// files, instance roles, IRSA, OIDC).
	AccessKeyID     string
	SecretAccessKey secrets.Secret

	// NoConditionalCreate makes Put with IfNotExists check, then put:
	// for storage that ignores If-None-Match (PRD 7.8 fallback).
	NoConditionalCreate bool

	// Tuning; zero means the default.
	PartSize    int64
	Concurrency int
	ReadBlock   int64
	ReadAhead   int
	MaxAttempts int
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
}

func (o *Options) defaults() {
	if o.PartSize == 0 {
		o.PartSize = DefaultPartSize
	}
	if o.Concurrency == 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.ReadBlock == 0 {
		o.ReadBlock = DefaultReadBlock
	}
	if o.ReadAhead == 0 {
		o.ReadAhead = DefaultReadAhead
	}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = DefaultMaxAttempts
	}
	if o.MinBackoff == 0 {
		o.MinBackoff = DefaultMinBackoff
	}
	if o.MaxBackoff == 0 {
		o.MaxBackoff = DefaultMaxBackoff
	}
}

// Backend stores objects in an S3-compatible bucket.
type Backend struct {
	client *s3.Client
	opts   Options
	// root is the key prefix every object is stored under.
	root string
	// gate bounds the requests in flight.
	gate chan struct{}
	// noCC is set when the storage refused If-None-Match, or was
	// configured without conditional create.
	noCC atomic.Bool
}

var _ backend.Backend = (*Backend)(nil)

// New returns a backend for a bucket. It makes no request: a wrong bucket
// or credentials show on the first operation.
func New(ctx context.Context, o Options) (*Backend, error) {
	o.defaults()
	if o.Bucket == "" {
		return nil, errors.New("no bucket")
	}
	if o.PartSize < minPartSize {
		return nil, fmt.Errorf("part size %d is below S3's minimum of %d", o.PartSize, minPartSize)
	}
	prefix := strings.Trim(o.Prefix, "/")
	if prefix != "" {
		if err := backend.ValidateKey(prefix); err != nil {
			return nil, fmt.Errorf("prefix: %w", err)
		}
	}
	load := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(so *retry.StandardOptions) {
				so.MaxAttempts = o.MaxAttempts
				so.Backoff = jitterBackoff{min: o.MinBackoff, max: o.MaxBackoff}
				so.RateLimiter = ratelimit.None
			})
		}),
		// Compute and check checksums only where S3 requires them: many
		// S3-compatible services reject the newer trailing checksums.
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	}
	if o.Region != "" {
		load = append(load, awsconfig.WithRegion(o.Region))
	}
	if o.AccessKeyID != "" {
		load = append(load, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(o.AccessKeyID, string(o.SecretAccessKey.Expose()), "")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, load...)
	if err != nil {
		return nil, fmt.Errorf("load the AWS configuration: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(so *s3.Options) {
		so.UsePathStyle = o.PathStyle
		if o.Endpoint != "" {
			so.BaseEndpoint = aws.String(o.Endpoint)
		}
	})
	b := &Backend{client: client, opts: o, gate: make(chan struct{}, o.Concurrency)}
	if prefix != "" {
		b.root = prefix + "/"
	}
	b.noCC.Store(o.NoConditionalCreate)
	return b, nil
}

// Capabilities reports conditional create unless the storage refused it
// or was configured without it, and strong consistency, which AWS S3 and
// the S3-compatible services pentym supports provide.
func (b *Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{ConditionalCreate: !b.noCC.Load(), StrongConsistency: true}
}

// acquire waits for a request slot.
func (b *Backend) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case b.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Backend) release() { <-b.gate }

// readUpTo reads up to n bytes; fewer means r ended.
func readUpTo(r io.Reader, n int64) ([]byte, error) {
	var buf bytes.Buffer
	_, err := io.CopyN(&buf, r, n)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Put stores r under key: in one PUT up to the part size, otherwise as a
// multipart upload. With IfNotExists the PUT (or the multipart completion)
// carries If-None-Match: *, so exactly one of racing creators wins.
func (b *Backend) Put(ctx context.Context, key string, r io.Reader, opts backend.PutOptions) error {
	if err := backend.ValidateKey(key); err != nil {
		return err
	}
	first, err := readUpTo(r, b.opts.PartSize+1)
	if err != nil {
		return err
	}
	if opts.IfNotExists && b.noCC.Load() {
		if err := b.checkAbsent(ctx, key); err != nil {
			return err
		}
		opts.IfNotExists = false
	}
	if int64(len(first)) <= b.opts.PartSize {
		err = b.putSingle(ctx, key, first, opts.IfNotExists)
	} else {
		err = b.putMultipart(ctx, key, first, r, opts.IfNotExists)
	}
	if errors.Is(err, errCCUnsupported) {
		// The storage refuses If-None-Match: from now on, check then put.
		b.noCC.Store(true)
		if err := b.checkAbsent(ctx, key); err != nil {
			return err
		}
		return b.putSingle(ctx, key, first, false)
	}
	return err
}

// checkAbsent is the fallback's check: ErrExists if key exists.
func (b *Backend) checkAbsent(ctx context.Context, key string) error {
	_, err := b.Stat(ctx, key)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s", backend.ErrExists, key)
	case errors.Is(err, backend.ErrNotFound):
		return nil
	}
	return err
}

func ifNoneMatch(cond bool) *string {
	if cond {
		return aws.String("*")
	}
	return nil
}

func (b *Backend) putSingle(ctx context.Context, key string, data []byte, cond bool) error {
	err := b.putOnce(ctx, key, data, cond)
	if cond && errors.Is(err, backend.ErrExists) && b.ownWrite(ctx, key, data) {
		// An earlier attempt of this PUT succeeded but its response was
		// lost, and the SDK's retry met the object it had written.
		return nil
	}
	return err
}

// putOnce is one PUT request (with the SDK's retries), holding one
// request slot.
func (b *Backend) putOnce(ctx context.Context, key string, data []byte, cond bool) error {
	if err := b.acquire(ctx); err != nil {
		return err
	}
	defer b.release()
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(b.opts.Bucket), Key: aws.String(b.root + key),
		Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))),
		IfNoneMatch: ifNoneMatch(cond),
	})
	return mapError("put", key, err, cond)
}

// ownWrite reports whether the object at key holds exactly data. Objects
// pentym creates conditionally hold unique content (lock records name
// their holder; packs and index files are content-addressed), so a match
// is this writer's own earlier attempt. data is at most one part.
func (b *Backend) ownWrite(ctx context.Context, key string, data []byte) bool {
	rc, err := b.Get(ctx, key, nil)
	if err != nil {
		return false
	}
	defer func() { _ = rc.Close() }()
	got, err := readUpTo(rc, int64(len(data))+1)
	return err == nil && bytes.Equal(got, data)
}

// putMultipart uploads first and the rest of r in parts of the part size,
// each retried on its own; any failure aborts the upload, so no partial
// object is ever visible.
func (b *Backend) putMultipart(ctx context.Context, key string, first []byte, r io.Reader, cond bool) (err error) {
	k := aws.String(b.root + key)
	bucket := aws.String(b.opts.Bucket)
	if err := b.acquire(ctx); err != nil {
		return err
	}
	created, err := b.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: k})
	b.release()
	if err != nil {
		return mapError("put", key, err, false)
	}
	defer func() {
		if err != nil {
			actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			_, _ = b.client.AbortMultipartUpload(actx, &s3.AbortMultipartUploadInput{Bucket: bucket, Key: k, UploadId: created.UploadId})
		}
	}()
	var parts []types.CompletedPart
	part := first[:b.opts.PartSize]
	carry := first[b.opts.PartSize:]
	for n := int32(1); ; n++ {
		if err := b.acquire(ctx); err != nil {
			return err
		}
		out, perr := b.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: bucket, Key: k, UploadId: created.UploadId, PartNumber: aws.Int32(n),
			Body: bytes.NewReader(part), ContentLength: aws.Int64(int64(len(part))),
		})
		b.release()
		if perr != nil {
			return mapError("put", key, perr, false)
		}
		parts = append(parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(n)})
		if len(carry) == 0 {
			break
		}
		rest, rerr := readUpTo(r, b.opts.PartSize-int64(len(carry)))
		if rerr != nil {
			return rerr
		}
		part = append(append(make([]byte, 0, len(carry)+len(rest)), carry...), rest...)
		carry = nil
		if int64(len(part)) == b.opts.PartSize {
			// There may be more: peek one byte.
			more, rerr := readUpTo(r, 1)
			if rerr != nil {
				return rerr
			}
			carry = more
		}
	}
	complete := func(cond bool) error {
		if err := b.acquire(ctx); err != nil {
			return err
		}
		defer b.release()
		_, err := b.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket: bucket, Key: k, UploadId: created.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
			IfNoneMatch:     ifNoneMatch(cond),
		})
		return mapError("put", key, err, cond)
	}
	err = complete(cond)
	if errors.Is(err, errCCUnsupported) {
		// The parts are uploaded: check, then complete unconditionally.
		b.noCC.Store(true)
		if err = b.checkAbsent(ctx, key); err != nil {
			return err
		}
		err = complete(false)
	}
	return err
}

// Stat returns the object's size and modification time.
func (b *Backend) Stat(ctx context.Context, key string) (backend.ObjectInfo, error) {
	if err := backend.ValidateKey(key); err != nil {
		return backend.ObjectInfo{}, err
	}
	if err := b.acquire(ctx); err != nil {
		return backend.ObjectInfo{}, err
	}
	defer b.release()
	out, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.opts.Bucket), Key: aws.String(b.root + key)})
	if err != nil {
		return backend.ObjectInfo{}, mapError("stat", key, err, false)
	}
	return backend.ObjectInfo{Key: key, Size: aws.ToInt64(out.ContentLength), ModTime: aws.ToTime(out.LastModified)}, nil
}

// List calls fn for every object under prefix, in key order (S3 lists
// keys in UTF-8 byte order, which for pentym's key alphabet is the same).
func (b *Backend) List(ctx context.Context, prefix string, fn func(backend.ObjectInfo) error) error {
	if err := backend.ValidatePrefix(prefix); err != nil {
		return err
	}
	p := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.opts.Bucket), Prefix: aws.String(b.root + prefix),
	})
	for p.HasMorePages() {
		if err := b.acquire(ctx); err != nil {
			return err
		}
		page, err := p.NextPage(ctx)
		b.release()
		if err != nil {
			return mapError("list", prefix, err, false)
		}
		for _, o := range page.Contents {
			key := strings.TrimPrefix(aws.ToString(o.Key), b.root)
			if err := fn(backend.ObjectInfo{Key: key, Size: aws.ToInt64(o.Size), ModTime: aws.ToTime(o.LastModified)}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Delete removes the object; a missing one is not an error.
func (b *Backend) Delete(ctx context.Context, key string) error {
	if err := backend.ValidateKey(key); err != nil {
		return err
	}
	if err := b.acquire(ctx); err != nil {
		return err
	}
	defer b.release()
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b.opts.Bucket), Key: aws.String(b.root + key)})
	if err = mapError("delete", key, err, false); errors.Is(err, backend.ErrNotFound) {
		return nil
	}
	return err
}
