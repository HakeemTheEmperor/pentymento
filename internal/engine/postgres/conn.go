// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// Message IDs for Postgres connections and privileges (see docs/errors).
const (
	IDConnect          msg.ID = "PTM-2001"
	IDVersion          msg.ID = "PTM-2002"
	IDNoReplication    msg.ID = "PTM-2003"
	IDTooMuchPrivilege msg.ID = "PTM-2004"
	IDWALLevel         msg.ID = "PTM-2005"
	IDReplicationHBA   msg.ID = "PTM-2006"
)

// conninfoValue quotes a value for a libpq connection string.
func conninfoValue(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// conninfo is the target's connection string without a password, safe
// to show and to pass to pg_basebackup.
func conninfo(t config.Target, cred config.Credential, database string) string {
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+conninfoValue(v))
		}
	}
	add("host", t.Host)
	if t.Port != 0 {
		add("port", strconv.Itoa(t.Port))
	}
	add("dbname", database)
	add("user", cred.User)
	add("sslmode", t.SSLMode)
	add("sslrootcert", t.SSLRootCert)
	add("sslcert", t.SSLCert)
	add("sslkey", t.SSLKey)
	add("application_name", "pentym")
	return strings.Join(parts, " ")
}

// password resolves a credential's password, if it has one.
func (e *Engine) password(ctx context.Context, target string, cred config.Credential) (secrets.Secret, error) {
	if cred.Password.IsZero() {
		return secrets.Secret{}, nil
	}
	return e.Resolver.Resolve(ctx, target, cred.Password)
}

// backupCred returns the backup credential, or explains it is missing.
func backupCred(t engine.Target) (config.Credential, error) {
	c := t.Config.Credentials.Backup
	if c.User == "" {
		return c, &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
			Level: msg.LevelFailed, Target: t.Name,
			Fact: "no backup role configured (credentials.backup.user)",
			Next: "set targets." + t.Name + ".credentials.backup.user in the config; pentym init --print-sql prints a minimal role",
		}}
	}
	return c, nil
}

// connect opens a connection as the backup role: a normal one to the
// target's database, or a physical replication connection.
func (e *Engine) connect(ctx context.Context, t engine.Target, replication bool) (*pgconn.PgConn, error) {
	cred, err := backupCred(t)
	if err != nil {
		return nil, err
	}
	db := t.Config.Database
	if db == "" {
		db = "postgres"
	}
	cfg, err := pgconn.ParseConfig(conninfo(t.Config, cred, db))
	if err != nil {
		return nil, &msg.Error{Code: msg.ExitConfig, Err: err, Message: msg.Message{
			Level: msg.LevelFailed, Target: t.Name, Fact: "the connection settings are invalid", Cause: err.Error(),
			Next: "check host, port, sslmode and the certificate paths of target " + t.Name,
		}}
	}
	pw, err := e.password(ctx, t.Name, cred)
	if err != nil {
		return nil, err
	}
	defer pw.Clear()
	if !pw.IsEmpty() {
		cfg.Password = string(pw.Expose())
	}
	if replication {
		cfg.RuntimeParams["replication"] = "true"
	}
	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, connectError(t, replication, err, pw)
	}
	return conn, nil
}

// connectError explains a failed connection, scrubbed of the password.
func connectError(t engine.Target, replication bool, err error, pw secrets.Secret) error {
	cause := secrets.Scrub(err.Error(), pw)
	m := msg.Message{
		Level: msg.LevelFailed, ID: IDConnect, Target: t.Name,
		Fact:  "cannot connect to Postgres as " + t.Config.Credentials.Backup.User,
		Cause: cause,
		Next:  "check the host, port, role and password of target " + t.Name + ", then run pentym doctor " + t.Name,
	}
	code := msg.ExitConnect
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "28000" && replication:
			m.ID, m.Fact = IDReplicationHBA, "the server refuses replication connections from "+t.Config.Credentials.Backup.User
			m.Impact = "base backups use a replication connection"
			m.Next = "allow it in pg_hba.conf with a line for the replication database, then reload Postgres"
			code = msg.ExitAccess
		case strings.HasPrefix(pgErr.Code, "28"):
			code = msg.ExitAccess
		}
	}
	return &msg.Error{Code: code, Err: secrets.ScrubError(err, pw), Message: m}
}

// serverMajor parses the server_version parameter, such as "17.2" or
// "16.6 (Debian 16.6-1.pgdg120+1)".
func serverMajor(conn *pgconn.PgConn) (int, string, error) {
	v := conn.ParameterStatus("server_version")
	head, _, _ := strings.Cut(v, " ")
	major, _, _ := strings.Cut(head, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0, v, fmt.Errorf("server version %q is not understood", v)
	}
	return n, head, nil
}

// supported reports whether pentym supports a Postgres major version.
func supported(major int) bool {
	for _, v := range Versions {
		if v == strconv.Itoa(major) {
			return true
		}
	}
	return false
}

func versionError(t engine.Target, version string) error {
	return &msg.Error{Code: msg.ExitFailure, Message: msg.Message{
		Level: msg.LevelFailed, ID: IDVersion, Target: t.Name,
		Fact: "Postgres " + version + " is not supported; pentym supports " + strings.Join(Versions, ", "),
		Next: "upgrade the server to a supported major version",
	}}
}
