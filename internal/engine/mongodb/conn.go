// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package mongodb

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// Message IDs for MongoDB connections and privileges (see docs/errors).
const (
	IDConnect          msg.ID = "PTM-2007"
	IDVersion          msg.ID = "PTM-2008"
	IDPrivilege        msg.ID = "PTM-2009"
	IDTooMuchPrivilege msg.ID = "PTM-2010"
	IDStandalone       msg.ID = "PTM-2011"
	IDSharded          msg.ID = "PTM-2012"
)

// defaultURI is where a target without a uri connects.
const defaultURI = "mongodb://localhost:27017"

// targetURI returns the target's connection string.
func targetURI(t config.Target) string {
	if t.URI == "" {
		return defaultURI
	}
	return t.URI
}

// x509 reports whether a connection string authenticates with a client
// certificate, which needs no configured user.
func x509(uri string) bool {
	_, query, _ := strings.Cut(uri, "?")
	for _, kv := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "authMechanism") && strings.EqualFold(v, "MONGODB-X509") {
			return true
		}
	}
	return false
}

// backupCred returns the backup credential, or explains it is missing.
func backupCred(t engine.Target) (config.Credential, error) {
	c := t.Config.Credentials.Backup
	if c.User == "" && !x509(targetURI(t.Config)) {
		return c, &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
			Level: msg.LevelFailed, Target: t.Name,
			Fact: "no backup user configured (credentials.backup.user)",
			Next: "set targets." + t.Name + ".credentials.backup.user in the config, or authenticate with a client certificate (authMechanism=MONGODB-X509 in uri)",
		}}
	}
	return c, nil
}

// password resolves a credential's password, if it has one.
func (e *Engine) password(ctx context.Context, target string, cred config.Credential) (secrets.Secret, error) {
	if cred.Password.IsZero() {
		return secrets.Secret{}, nil
	}
	return e.Resolver.Resolve(ctx, target, cred.Password)
}

// hello is the part of the hello reply that tells the topology.
type hello struct {
	// SetName is the replica set's name; empty for a standalone.
	SetName string `bson:"setName"`
	// Msg is "isdbgrid" from a mongos router.
	Msg string `bson:"msg"`
}

func (h hello) sharded() bool    { return h.Msg == "isdbgrid" }
func (h hello) replicaSet() bool { return h.SetName != "" && !h.sharded() }

// session is a connection to the target as the backup user.
type session struct {
	client *mongo.Client
	hello  hello
	// user is the configured user; empty with a client certificate.
	user string
}

func (s *session) close(ctx context.Context) {
	_ = s.client.Disconnect(context.WithoutCancel(ctx))
}

// connect connects as the backup user and asks the server for its
// topology, which also proves the login works.
func (e *Engine) connect(ctx context.Context, t engine.Target) (*session, error) {
	cred, err := backupCred(t)
	if err != nil {
		return nil, err
	}
	pw, err := e.password(ctx, t.Name, cred)
	if err != nil {
		return nil, err
	}
	defer pw.Clear()
	opts := options.Client().ApplyURI(targetURI(t.Config)).SetAppName("pentym")
	if cred.User != "" {
		// Keep what the URI says about authentication (authSource,
		// authMechanism); add the user and password.
		var a options.Credential
		if opts.Auth != nil {
			a = *opts.Auth
		}
		a.Username = cred.User
		if !pw.IsEmpty() {
			a.Password, a.PasswordSet = string(pw.Expose()), true
		}
		opts.SetAuth(a)
	}
	cl, err := mongo.Connect(opts)
	if err != nil {
		return nil, connectError(t, err, pw)
	}
	var h hello
	if err := cl.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&h); err != nil {
		_ = cl.Disconnect(context.WithoutCancel(ctx))
		return nil, connectError(t, err, pw)
	}
	return &session{client: cl, hello: h, user: cred.User}, nil
}

// connectError explains a failed connection, scrubbed of the password.
func connectError(t engine.Target, err error, pw secrets.Secret) error {
	fact := "cannot connect to MongoDB"
	if u := t.Config.Credentials.Backup.User; u != "" {
		fact += " as " + u
	}
	code := msg.ExitConnect
	var ce mongo.CommandError
	if (errors.As(err, &ce) && (ce.Code == 18 || ce.Code == 13)) || strings.Contains(err.Error(), "auth error") {
		code = msg.ExitAccess // AuthenticationFailed, Unauthorized
	}
	return &msg.Error{Code: code, Err: secrets.ScrubError(err, pw), Message: msg.Message{
		Level: msg.LevelFailed, ID: IDConnect, Target: t.Name, Fact: fact,
		Cause: secrets.Scrub(err.Error(), pw),
		Next:  "check the uri, user and password of target " + t.Name + ", then run pentym doctor " + t.Name,
	}}
}

// version returns the server's version, such as "8.0.4".
func (s *session) version(ctx context.Context) (string, error) {
	var info struct {
		Version string `bson:"version"`
	}
	if err := s.client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err != nil {
		return "", fmt.Errorf("read the server version: %w", err)
	}
	return info.Version, nil
}

// supported reports whether pentym supports a server version, by its
// major.minor release.
func supported(version string) bool {
	v, err := toolrun.ParseVersion([]byte(version))
	return err == nil && slices.Contains(Versions, fmt.Sprintf("%d.%d", v.Major, v.Minor))
}

// position formats an oplog timestamp as a log position, in the
// <seconds>:<ordinal> form mongorestore --oplogLimit takes.
func position(ts bson.Timestamp) engine.Position {
	return engine.Position(fmt.Sprintf("%d:%d", ts.T, ts.I))
}
