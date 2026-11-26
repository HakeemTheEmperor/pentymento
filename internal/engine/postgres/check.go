// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// query runs a query on a normal connection and returns its rows as text.
func query(ctx context.Context, conn *pgconn.PgConn, sql string) ([][]string, error) {
	res, err := conn.Exec(ctx, sql).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(res) != 1 {
		return nil, fmt.Errorf("expected one result, got %d", len(res))
	}
	if res[0].Err != nil {
		return nil, res[0].Err
	}
	out := make([][]string, 0, len(res[0].Rows))
	for _, row := range res[0].Rows {
		r := make([]string, len(row))
		for i, v := range row {
			r[i] = string(v)
		}
		out = append(out, r)
	}
	return out, nil
}

// Check connects as the backup role and checks what base backups need: a
// supported version, the REPLICATION attribute, wal_level, a free WAL
// sender and a replication connection pg_hba.conf allows. A superuser
// role works but has more privilege than needed (SDD 8.5).
func (e *Engine) Check(ctx context.Context, t engine.Target) (*engine.CheckResult, error) {
	conn, err := e.connect(ctx, t, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	major, version, err := serverMajor(conn)
	if err != nil {
		return nil, err
	}
	res := &engine.CheckResult{ServerVersion: version, Role: t.Config.Credentials.Backup.User}
	add := func(level msg.Level, id msg.ID, fact, next string) {
		res.Findings = append(res.Findings, engine.Finding{Level: level, ID: id, Fact: fact, Next: next})
	}
	if !supported(major) {
		add(msg.LevelFailed, IDVersion, "Postgres "+version+" is not supported; pentym supports "+strings.Join(Versions, ", "),
			"upgrade the server to a supported major version")
	}
	rows, err := query(ctx, conn, `SELECT r.rolsuper, r.rolreplication, current_setting('wal_level'),
		current_setting('max_wal_senders')::int, (SELECT count(*) FROM pg_stat_replication)
		FROM pg_roles r WHERE r.rolname = current_user`)
	if err != nil {
		return nil, fmt.Errorf("read the role and server settings: %w", err)
	}
	if len(rows) != 1 {
		return nil, errors.New("the current role is not in pg_roles")
	}
	super, repl, walLevel := rows[0][0] == "t", rows[0][1] == "t", rows[0][2]
	senders, _ := strconv.Atoi(rows[0][3])
	used, _ := strconv.Atoi(rows[0][4])
	role := t.Config.Credentials.Backup.User
	switch {
	case super:
		add(msg.LevelWarning, IDTooMuchPrivilege, role+" is a superuser, which is more privilege than backups need",
			"use a role with only REPLICATION: pentym init --print-sql prints one")
	case !repl:
		add(msg.LevelFailed, IDNoReplication, role+" lacks the REPLICATION attribute, which base backups need",
			"ALTER ROLE "+role+" REPLICATION;")
	}
	if walLevel == "minimal" {
		add(msg.LevelFailed, IDWALLevel, "wal_level is minimal, which allows no base backups over replication",
			"set wal_level = replica in postgresql.conf and restart Postgres")
	}
	if senders-used < 1 {
		add(msg.LevelFailed, IDNoReplication, fmt.Sprintf("all %d WAL senders are in use (max_wal_senders)", senders),
			"raise max_wal_senders and restart Postgres")
	}
	// pg_hba.conf has separate rules for replication connections.
	rconn, err := e.connect(ctx, t, true)
	if err != nil {
		var me *msg.Error
		if errors.As(err, &me) {
			add(msg.LevelFailed, me.Message.ID, me.Message.Fact, me.Message.Next)
			return res, nil
		}
		return nil, err
	}
	defer func() { _ = rconn.Close(context.WithoutCancel(ctx)) }()
	if _, err := pglogrepl.IdentifySystem(ctx, rconn); err != nil {
		add(msg.LevelFailed, IDReplicationHBA, "the replication connection does not work: "+err.Error(),
			"check pg_hba.conf has a line for the replication database")
	}
	return res, nil
}

// Inventory is a Postgres snapshot's catalog summary: what the cluster
// held, for verification to compare with later.
type Inventory struct {
	ServerVersion string            `json:"server_version"`
	SystemID      string            `json:"system_id,omitempty"`
	Databases     []DatabaseSummary `json:"databases"`
}

// DatabaseSummary describes one database.
type DatabaseSummary struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	// Tables counts user tables; EstimatedRows sums the planner's row
	// estimates, so it is approximate.
	Tables        int    `json:"tables"`
	EstimatedRows int64  `json:"estimated_rows"`
	Error         string `json:"error,omitempty"`
}

// Inventory summarizes the cluster: every database that accepts
// connections, with its size and user tables. A database the backup role
// cannot read is listed with the reason.
func (e *Engine) Inventory(ctx context.Context, t engine.Target) (json.RawMessage, error) {
	conn, err := e.connect(ctx, t, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	_, version, err := serverMajor(conn)
	if err != nil {
		return nil, err
	}
	inv := Inventory{ServerVersion: version, Databases: []DatabaseSummary{}}
	if rows, err := query(ctx, conn, "SELECT system_identifier::text FROM pg_control_system()"); err == nil && len(rows) == 1 {
		inv.SystemID = rows[0][0]
	}
	dbs, err := query(ctx, conn, `SELECT datname, pg_database_size(oid) FROM pg_database
		WHERE datallowconn AND NOT datistemplate ORDER BY datname`)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	for _, d := range dbs {
		s := DatabaseSummary{Name: d[0]}
		s.SizeBytes, _ = strconv.ParseInt(d[1], 10, 64)
		if err := e.summarize(ctx, t, &s); err != nil {
			s.Error = err.Error()
		}
		inv.Databases = append(inv.Databases, s)
	}
	return json.Marshal(inv)
}

// summarize counts a database's user tables and estimated rows.
func (e *Engine) summarize(ctx context.Context, t engine.Target, s *DatabaseSummary) error {
	tt := t
	tt.Config.Database = s.Name
	conn, err := e.connect(ctx, tt, false)
	if err != nil {
		var me *msg.Error
		if errors.As(err, &me) {
			return errors.New(me.Message.Cause)
		}
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	rows, err := query(ctx, conn, `SELECT count(*), coalesce(sum(greatest(c.reltuples, 0)), 0)::bigint
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		AND n.nspname NOT LIKE 'pg\_toast%'`)
	if err != nil {
		return err
	}
	s.Tables, _ = strconv.Atoi(rows[0][0])
	s.EstimatedRows, _ = strconv.ParseInt(rows[0][1], 10, 64)
	return nil
}
