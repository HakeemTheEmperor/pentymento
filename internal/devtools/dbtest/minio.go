// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package dbtest

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
)

// MinIOImage supports conditional writes (If-None-Match) on PUT and on
// multipart completion; the module's default image predates them.
const MinIOImage = "quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z"

// MinIO is a running MinIO server with one empty bucket.
type MinIO struct {
	// Endpoint is the server's URL, such as http://127.0.0.1:32768.
	Endpoint string
	Bucket   string
	// AccessKey and SecretKey are the root user's keys.
	AccessKey, SecretKey string
}

// StartMinIO starts MinIO with a bucket named pentym and stops it when the
// test finishes.
func StartMinIO(t testing.TB) *MinIO {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	c, err := tcminio.Run(ctx, MinIOImage, tcminio.WithUsername("pentym"), tcminio.WithPassword(randomPassword(t)))
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("start MinIO: %v", err)
	}
	addr, err := c.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("MinIO address: %v", err)
	}
	m := &MinIO{Endpoint: "http://" + addr, Bucket: "pentym", AccessKey: c.Username, SecretKey: c.Password}
	client := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(m.Endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(m.AccessKey, m.SecretKey, ""),
	})
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(m.Bucket)}); err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	return m
}
