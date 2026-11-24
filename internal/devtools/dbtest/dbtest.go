// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcmongodb "github.com/testcontainers/testcontainers-go/modules/mongodb"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Environment variables that select the engine versions under test.
const (
	EnvPostgres = "PENTYM_TEST_POSTGRES"
	EnvMongoDB  = "PENTYM_TEST_MONGODB"
)

// Supported versions (PRD 6.1). Each is the image tag used for the container.
var (
	AllPostgresVersions = []string{"16", "17", "18"}
	AllMongoDBVersions  = []string{"7.0", "8.0", "9.0"}
)

// replicaSetName is the name of the single-node MongoDB replica set.
const replicaSetName = "rs0"

// startTimeout bounds how long a container may take to start and become ready.
const startTimeout = 5 * time.Minute

// PostgresVersions returns the Postgres major versions to test, from
// PENTYM_TEST_POSTGRES or, when unset, every supported version.
func PostgresVersions() []string {
	return versionsFromEnv(EnvPostgres, AllPostgresVersions)
}

// MongoDBVersions returns the MongoDB versions to test, from
// PENTYM_TEST_MONGODB or, when unset, every supported version.
func MongoDBVersions() []string {
	return versionsFromEnv(EnvMongoDB, AllMongoDBVersions)
}

func versionsFromEnv(name string, all []string) []string {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return append([]string(nil), all...)
	}
	var out []string
	for _, f := range strings.Split(v, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// Postgres is a running Postgres container.
type Postgres struct {
	// Version is the image tag the container was started from.
	Version string
	// ConnString is a libpq connection URL for the superuser. It contains
	// a random test password and must not be logged.
	ConnString string
}

// StartPostgres starts a Postgres container of the given major version and
// stops it when the test finishes.
func StartPostgres(t testing.TB, version string) *Postgres {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	c, err := tcpostgres.Run(ctx, "postgres:"+version,
		tcpostgres.WithDatabase("pentym_test"),
		tcpostgres.WithUsername("pentym_test"),
		tcpostgres.WithPassword(randomPassword(t)),
		tcpostgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("start postgres:%s: %v", version, err)
	}
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres:%s connection string: %v", version, err)
	}
	return &Postgres{Version: version, ConnString: dsn}
}

// MongoDB is a running single-node MongoDB replica set.
type MongoDB struct {
	// Version is the image tag the container was started from.
	Version string
	// URI is the connection string for the replica set.
	URI string
	// ReplicaSet is the replica set name.
	ReplicaSet string
}

// StartMongoDB starts a MongoDB container of the given version as a
// single-node replica set and stops it when the test finishes.
func StartMongoDB(t testing.TB, version string) *MongoDB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	c, err := tcmongodb.Run(ctx, "mongo:"+version, tcmongodb.WithReplicaSet(replicaSetName))
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("start mongo:%s: %v", version, err)
	}
	uri, err := c.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("mongo:%s connection string: %v", version, err)
	}
	uri, err = withDirectConnection(uri)
	if err != nil {
		t.Fatalf("mongo:%s connection string: %v", version, err)
	}
	return &MongoDB{Version: version, URI: uri, ReplicaSet: replicaSetName}
}

// withDirectConnection sets directConnection=true on a MongoDB URI. The
// replica set member advertises its container address, which the host cannot
// reach under Docker Desktop (Windows, macOS); a direct connection uses the
// mapped port instead. The server is still a replica set member with an
// oplog.
func withDirectConnection(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("directConnection", "true")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func randomPassword(t testing.TB) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random password: %v", err)
	}
	return hex.EncodeToString(b)
}
