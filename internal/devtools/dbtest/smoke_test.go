// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package dbtest

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const queryTimeout = 30 * time.Second

// TestPostgresSmoke starts each selected Postgres version, connects and
// checks that the server reports the expected major version.
func TestPostgresSmoke(t *testing.T) {
	for _, version := range PostgresVersions() {
		t.Run("postgres-"+version, func(t *testing.T) {
			t.Parallel()
			pg := StartPostgres(t, version)

			ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
			defer cancel()
			conn, err := pgx.Connect(ctx, pg.ConnString)
			if err != nil {
				t.Fatalf("connect to postgres:%s: %v", version, err)
			}
			defer func() { _ = conn.Close(context.Background()) }()

			var num string
			if err := conn.QueryRow(ctx, "SHOW server_version_num").Scan(&num); err != nil {
				t.Fatalf("postgres:%s server_version_num: %v", version, err)
			}
			n, err := strconv.Atoi(num)
			if err != nil {
				t.Fatalf("postgres:%s server_version_num %q: %v", version, num, err)
			}
			if got := strconv.Itoa(n / 10000); got != version {
				t.Fatalf("postgres:%s reports major version %s", version, got)
			}
		})
	}
}

// TestMongoDBSmoke starts each selected MongoDB version as a single-node
// replica set, connects and checks the version and replica set state.
func TestMongoDBSmoke(t *testing.T) {
	for _, version := range MongoDBVersions() {
		t.Run("mongodb-"+version, func(t *testing.T) {
			t.Parallel()
			mg := StartMongoDB(t, version)

			ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
			defer cancel()
			client, err := mongo.Connect(options.Client().ApplyURI(mg.URI))
			if err != nil {
				t.Fatalf("connect to mongo:%s: %v", version, err)
			}
			defer func() { _ = client.Disconnect(context.Background()) }()

			admin := client.Database("admin")
			var build struct {
				Version string `bson:"version"`
			}
			if err := admin.RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&build); err != nil {
				t.Fatalf("mongo:%s buildInfo: %v", version, err)
			}
			if !strings.HasPrefix(build.Version, version+".") {
				t.Fatalf("mongo:%s reports version %s", version, build.Version)
			}

			var hello struct {
				SetName           string `bson:"setName"`
				IsWritablePrimary bool   `bson:"isWritablePrimary"`
			}
			if err := admin.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
				t.Fatalf("mongo:%s hello: %v", version, err)
			}
			if hello.SetName != mg.ReplicaSet || !hello.IsWritablePrimary {
				t.Fatalf("mongo:%s replica set = %q, writable primary = %v; want %q, true",
					version, hello.SetName, hello.IsWritablePrimary, mg.ReplicaSet)
			}
		})
	}
}
