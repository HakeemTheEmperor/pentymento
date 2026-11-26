// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/backendtest"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// minioImage supports conditional writes (If-None-Match) on PUT and on
// multipart completion.
const minioImage = "quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z"

// startMinIO starts MinIO with one bucket and returns options for it.
func startMinIO(t *testing.T) Options {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pw := make([]byte, 16)
	_, _ = rand.Read(pw)
	c, err := tcminio.Run(ctx, minioImage, tcminio.WithUsername("pentym"), tcminio.WithPassword(hex.EncodeToString(pw)))
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("start MinIO: %v", err)
	}
	addr, err := c.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	o := Options{
		Bucket: "pentym", Region: "us-east-1", Endpoint: "http://" + addr, PathStyle: true,
		AccessKeyID: c.Username, SecretAccessKey: secrets.New([]byte(c.Password)),
		MaxAttempts: 10, MinBackoff: 10 * time.Millisecond, MaxBackoff: 200 * time.Millisecond,
	}
	b, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(o.Bucket)}); err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	return o
}

// under returns a backend for a fresh prefix of the bucket, so each
// subtest starts empty.
func under(t *testing.T, o Options) *Backend {
	t.Helper()
	id := make([]byte, 6)
	_, _ = rand.Read(id)
	o.Prefix = "t-" + hex.EncodeToString(id)
	b, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMinIOConformance(t *testing.T) {
	o := startMinIO(t)
	t.Run("direct", func(t *testing.T) {
		backendtest.Run(t, func(t *testing.T) backend.Backend { return under(t, o) })
	})
	// The same suite through a proxy that fails about one request in five:
	// throttling or a server error before the request reaches MinIO, or a
	// lost response after it did. Every operation must still succeed
	// through retries.
	t.Run("faulty-network", func(t *testing.T) {
		target, _ := url.Parse(o.Endpoint)
		proxy := httputil.NewSingleHostReverseProxy(target)
		var mu sync.Mutex
		rng := mrand.New(mrand.NewPCG(7, 8)) // #nosec G404 -- fault schedule
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			roll := rng.IntN(16)
			mu.Unlock()
			switch roll {
			case 0:
				s3Error(w, http.StatusServiceUnavailable, "SlowDown")
			case 1:
				s3Error(w, http.StatusInternalServerError, "InternalError")
			case 2:
				proxy.ServeHTTP(httptest.NewRecorder(), r)
				s3Error(w, http.StatusBadGateway, "BadGateway")
			default:
				proxy.ServeHTTP(w, r)
			}
		}))
		defer srv.Close()
		fo := o
		fo.Endpoint = srv.URL
		backendtest.Run(t, func(t *testing.T) backend.Backend { return under(t, fo) })
	})
}

func TestMinIOMultipartAndReadAhead(t *testing.T) {
	o := startMinIO(t)
	o.PartSize, o.ReadBlock, o.ReadAhead = minPartSize, 1<<20, 4
	b := under(t, o)
	ctx := context.Background()
	data := make([]byte, 2*minPartSize+777)
	_, _ = rand.Read(data)
	if err := b.Put(ctx, "data/ab/pack", bytes.NewReader(data), backend.PutOptions{IfNotExists: true}); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, "data/ab/pack", bytes.NewReader(data), backend.PutOptions{IfNotExists: true}); !errors.Is(err, backend.ErrExists) {
		t.Errorf("a second conditional multipart create = %v", err)
	}
	rc, err := b.Get(ctx, "data/ab/pack", nil)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if _, err := got.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if !bytes.Equal(got.Bytes(), data) {
		t.Error("the multipart object reads back different")
	}
	uploads, err := b.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(o.Bucket)})
	if err != nil || len(uploads.Uploads) != 0 {
		t.Errorf("multipart uploads left: %v, %v", uploads, err)
	}
	if !b.Capabilities().ConditionalCreate {
		t.Error("MinIO's conditional create was refused")
	}
}
