// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package instance

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// pgConfig builds a connection config without putting the password in a
// URL that could be printed.
func pgConfig(c Conn, database string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cfg.Host, cfg.Port, cfg.User, cfg.Database = c.Host, uint16(c.Port), c.User, database // #nosec G115 -- a TCP port
	cfg.Password = string(c.Password.Expose())
	cfg.RuntimeParams = map[string]string{"application_name": "pentym"}
	return cfg, nil
}

// pgVersion logs in and returns the server's version: the check that the
// instance is ready and the one asked for.
func pgVersion(ctx context.Context, c Conn) (toolrun.Version, error) {
	cfg, err := pgConfig(c, "postgres")
	if err != nil {
		return toolrun.Version{}, err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return toolrun.Version{}, err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var num int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&num); err != nil {
		return toolrun.Version{}, err
	}
	return toolrun.Version{Major: num / 10000, Minor: num % 10000}, nil
}

// mongoClient connects to a side instance as its superuser.
func mongoClient(c Conn) (*mongo.Client, error) {
	o := options.Client().
		SetHosts([]string{net.JoinHostPort(c.Host, strconv.Itoa(c.Port))}).
		SetDirect(true).
		SetAppName("pentym").
		SetAuth(options.Credential{Username: c.User, Password: string(c.Password.Expose()), AuthSource: "admin"})
	return mongo.Connect(o)
}

// mongoVersion logs in and returns the server's version.
func mongoVersion(ctx context.Context, c Conn) (toolrun.Version, error) {
	cl, err := mongoClient(c)
	if err != nil {
		return toolrun.Version{}, err
	}
	defer func() { _ = cl.Disconnect(context.Background()) }()
	var info struct {
		Version string `bson:"version"`
	}
	if err := cl.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err != nil {
		return toolrun.Version{}, err
	}
	return toolrun.ParseVersion([]byte(info.Version))
}

// serverVersion logs in to an instance and returns its version.
func serverVersion(ctx context.Context, engine string, c Conn) (toolrun.Version, error) {
	if engine == Postgres {
		return pgVersion(ctx, c)
	}
	return mongoVersion(ctx, c)
}

// awaitReady waits until the instance accepts a login, then checks it
// runs the version asked for.
func awaitReady(ctx context.Context, spec Spec, c Conn) error {
	var v toolrun.Version
	err := waitFor(ctx, readyTimeout, func(ctx context.Context) error {
		var err error
		v, err = serverVersion(ctx, spec.Engine, c)
		return err
	})
	if err != nil {
		return err
	}
	if !versionMatches(spec.Engine, spec.Version, v) {
		return fmt.Errorf("the side instance runs %s %s, not %s", spec.Engine, v, spec.Version)
	}
	return nil
}

// mongoClientNoAuth connects without credentials, for the localhost
// exception.
func mongoClientNoAuth(c Conn) (*mongo.Client, error) {
	return mongo.Connect(options.Client().
		SetHosts([]string{net.JoinHostPort(c.Host, strconv.Itoa(c.Port))}).
		SetDirect(true).SetAppName("pentym"))
}
