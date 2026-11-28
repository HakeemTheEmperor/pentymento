// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// Message IDs for extraction (see docs/errors).
const (
	IDObjectExists  msg.ID = "PTM-6003"
	IDObjectMissing msg.ID = "PTM-6004"
)

// maxIdentifier is Postgres' longest name, in bytes.
const maxIdentifier = 63

// maxSuffixTries bounds the search for a free safe name.
const maxSuffixTries = 100

// qualified is a schema-qualified name.
type qualified struct{ schema, name string }

func (q qualified) String() string { return q.schema + "." + q.name }

// sql quotes it as an identifier.
func (q qualified) sql() string { return quoteIdent(q.schema) + "." + quoteIdent(q.name) }

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// parseTable reads a table as given: name or schema.name, public by
// default.
func parseTable(s string) (qualified, error) {
	schema, name, ok := strings.Cut(s, ".")
	if !ok {
		schema, name = "public", s
	}
	if schema == "" || name == "" || strings.Contains(name, ".") {
		return qualified{}, fmt.Errorf("%q is not a table name; write it as orders or billing.invoices", s)
	}
	return qualified{schema, name}, nil
}

// withSuffix appends a suffix to a name, shortening the name so the
// result fits Postgres' 63 bytes.
func withSuffix(name, suffix string) string {
	keep := maxIdentifier - len(suffix)
	for len(name) > keep {
		_, size := lastRune(name)
		name = name[:len(name)-size]
	}
	return name + suffix
}

func lastRune(s string) (rune, int) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i]&0xC0 != 0x80 {
			return rune(s[i]), len(s) - i
		}
	}
	return 0, len(s)
}

// sideTable is a table in the side instance and what belongs to it.
type sideTable struct {
	src     qualified
	oid     string
	indexes []string
	seqs    []string
	rows    int64
	dest    qualified
	renamed map[string]string // index or sequence → its name in production
	foreign []engine.SkippedConstraint
}

// Extract copies tables from a side instance into production (PRD 7.5):
// pg_dump -t from the side instance, piped into pg_restore against the
// target as its restore role. Unless req.InPlace, each table is renamed
// in the side instance first, with its indexes and owned sequences, to a
// safe name that is free in production. Foreign keys to tables that are
// not extracted are left out and reported. Constraints, indexes, owned
// sequences with their values, defaults and triggers come along; grants
// only in place, so a recovered copy is not exposed to the application.
func (e *Engine) Extract(ctx context.Context, from instance.Instance, t engine.Target, req engine.ExtractRequest) (*engine.ExtractResult, error) {
	cred := t.Config.Credentials.Restore
	if cred.User == "" {
		return nil, &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
			Level: msg.LevelFailed, Target: t.Name,
			Fact: "no restore role configured (credentials.restore.user)",
			Next: "create a role that may create tables in the target schema, such as pentym_restore, and set targets." + t.Name + ".credentials.restore.user",
		}}
	}
	if len(req.Objects) == 0 {
		return nil, errors.New("no tables to extract")
	}
	if !req.InPlace && req.Suffix == "" {
		return nil, errors.New("a safe name needs a suffix")
	}
	db := t.Config.Database
	if db == "" {
		db = "postgres"
	}
	var tables []*sideTable
	seen := map[qualified]bool{}
	for _, o := range req.Objects {
		q, err := parseTable(o)
		if err != nil {
			return nil, &msg.Error{Code: msg.ExitUsage, Message: msg.Message{Level: msg.LevelFailed, Target: t.Name, Fact: err.Error(), Next: "pentym restore --help"}}
		}
		if !seen[q] {
			seen[q] = true
			tables = append(tables, &sideTable{src: q, renamed: map[string]string{}})
		}
	}
	sc := from.Conn()
	side, err := sideConnectDB(ctx, sc, sc.User, sc.Password, db)
	if err != nil {
		return nil, fmt.Errorf("connect to the side instance: %w", err)
	}
	defer func() { _ = side.Close(context.WithoutCancel(ctx)) }()
	prod, err := e.connectAs(ctx, t, cred, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = prod.Close(context.WithoutCancel(ctx)) }()

	for _, st := range tables {
		if err := describeSideTable(ctx, side, st); err != nil {
			if errors.Is(err, errNoTable) {
				return nil, &msg.Error{Code: msg.ExitNotFound, Message: msg.Message{
					Level: msg.LevelFailed, ID: IDObjectMissing, Target: t.Name,
					Fact:   "table " + st.src.String() + " did not exist at the restore point",
					Impact: "nothing was loaded into production",
					Next:   "check the table's name and schema, or restore to an earlier point",
				}}
			}
			return nil, err
		}
	}
	if err := e.chooseNames(ctx, prod, t, tables, req); err != nil {
		return nil, err
	}
	oids := make([]string, len(tables))
	for i, st := range tables {
		oids[i] = st.oid
	}
	for _, st := range tables {
		if err := prepareSideTable(ctx, side, st, oids, req.InPlace); err != nil {
			return nil, err
		}
	}
	if err := e.dumpRestore(ctx, side, sc, db, t, cred, tables, req.InPlace); err != nil {
		return nil, err
	}
	res := &engine.ExtractResult{}
	for _, st := range tables {
		res.Objects = append(res.Objects, engine.Extracted{Source: st.src.String(), Name: st.dest.String(), Rows: st.rows})
		res.SkippedForeignKeys = append(res.SkippedForeignKeys, st.foreign...)
	}
	return res, nil
}

var errNoTable = errors.New("no such table")

// describeSideTable finds a table in the side instance, its indexes,
// owned sequences and rows.
func describeSideTable(ctx context.Context, side *pgconn.PgConn, st *sideTable) error {
	rows, err := query(ctx, side, `SELECT c.oid::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = `+quoteLiteral(st.src.schema)+` AND c.relname = `+quoteLiteral(st.src.name)+` AND c.relkind IN ('r', 'p')`)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return errNoTable
	}
	st.oid = rows[0][0]
	if rows, err = query(ctx, side, `SELECT ic.relname FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid
		WHERE i.indrelid = `+st.oid+` ORDER BY 1`); err != nil {
		return err
	}
	for _, r := range rows {
		st.indexes = append(st.indexes, r[0])
	}
	if rows, err = query(ctx, side, `SELECT s.relname FROM pg_depend d JOIN pg_class s ON s.oid = d.objid
		WHERE d.classid = 'pg_class'::regclass AND d.refobjid = `+st.oid+` AND s.relkind = 'S' AND d.deptype IN ('a', 'i') ORDER BY 1`); err != nil {
		return err
	}
	for _, r := range rows {
		st.seqs = append(st.seqs, r[0])
	}
	if rows, err = query(ctx, side, "SELECT count(*) FROM "+st.src.sql()); err != nil {
		return err
	}
	st.rows, _ = strconv.ParseInt(rows[0][0], 10, 64)
	return nil
}

// chooseNames picks each table's name in production: its own name in
// place, which must be free, or the first free safe name.
func (e *Engine) chooseNames(ctx context.Context, prod *pgconn.PgConn, t engine.Target, tables []*sideTable, req engine.ExtractRequest) error {
	taken := func(schema, name string) (bool, error) {
		rows, err := query(ctx, prod, `SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = `+quoteLiteral(schema)+` AND c.relname = `+quoteLiteral(name))
		return len(rows) > 0, err
	}
	for _, st := range tables {
		rows, err := query(ctx, prod, "SELECT 1 FROM pg_namespace WHERE nspname = "+quoteLiteral(st.src.schema))
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return &msg.Error{Code: msg.ExitNotFound, Message: msg.Message{
				Level: msg.LevelFailed, ID: IDObjectMissing, Target: t.Name,
				Fact: "schema " + st.src.schema + " does not exist in production", Impact: "nothing was loaded into production",
				Next: "create the schema, then restore again",
			}}
		}
		if req.InPlace {
			for _, n := range append([]string{st.src.name}, append(st.indexes, st.seqs...)...) {
				used, err := taken(st.src.schema, n)
				if err != nil {
					return err
				}
				if used {
					return &msg.Error{Code: msg.ExitFailure, Message: msg.Message{
						Level: msg.LevelFailed, ID: IDObjectExists, Target: t.Name,
						Fact:   st.src.schema + "." + n + " still exists in production, so " + st.src.String() + " cannot be restored in place",
						Impact: "nothing was loaded into production; --in-place restores only tables that no longer exist",
						Next:   "restore without --in-place: it loads the table under a safe name to compare and copy rows from",
					}}
				}
			}
			st.dest = st.src
			continue
		}
	try:
		for i := 1; i <= maxSuffixTries; i++ {
			sfx := req.Suffix
			if i > 1 {
				sfx += "_" + strconv.Itoa(i)
			}
			dest := withSuffix(st.src.name, sfx)
			names := map[string]string{}
			for _, n := range append(append([]string(nil), st.indexes...), st.seqs...) {
				names[n] = withSuffix(n, sfx)
			}
			for _, n := range append([]string{dest}, mapValues(names)...) {
				used, err := taken(st.src.schema, n)
				if err != nil {
					return err
				}
				if used || otherTableUses(tables, st, n) {
					continue try
				}
			}
			st.dest, st.renamed = qualified{st.src.schema, dest}, names
			break
		}
		if st.dest == (qualified{}) {
			return fmt.Errorf("no free name for %s after %d tries", st.src, maxSuffixTries)
		}
	}
	return nil
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// otherTableUses reports whether another table of this extraction has
// already chosen a name.
func otherTableUses(tables []*sideTable, self *sideTable, name string) bool {
	for _, st := range tables {
		if st == self || st.dest.schema != self.src.schema {
			continue
		}
		if st.dest.name == name {
			return true
		}
		for _, v := range st.renamed {
			if v == name {
				return true
			}
		}
	}
	return false
}

// prepareSideTable leaves out the table's foreign keys to tables outside
// the extraction, recording how to add them back, and renames the table,
// its indexes and owned sequences to their names in production.
func prepareSideTable(ctx context.Context, side *pgconn.PgConn, st *sideTable, oids []string, inPlace bool) error {
	rows, err := query(ctx, side, `SELECT conname, pg_get_constraintdef(oid), confrelid::regclass::text FROM pg_constraint
		WHERE conrelid = `+st.oid+` AND contype = 'f' AND confrelid <> ALL (ARRAY[`+strings.Join(oids, ", ")+`]::oid[]) ORDER BY 1`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		st.foreign = append(st.foreign, engine.SkippedConstraint{
			Table: st.dest.String(), Name: r[0], References: r[2],
			Statement: "ALTER TABLE " + st.dest.sql() + " ADD CONSTRAINT " + quoteIdent(r[0]) + " " + r[1] + ";",
		})
		if _, err := side.Exec(ctx, "ALTER TABLE "+st.src.sql()+" DROP CONSTRAINT "+quoteIdent(r[0])).ReadAll(); err != nil {
			return fmt.Errorf("leave out the foreign key %s: %w", r[0], err)
		}
	}
	if inPlace {
		return nil
	}
	var stmts []string
	for _, n := range st.seqs {
		stmts = append(stmts, "ALTER SEQUENCE "+qualified{st.src.schema, n}.sql()+" RENAME TO "+quoteIdent(st.renamed[n]))
	}
	for _, n := range st.indexes {
		stmts = append(stmts, "ALTER INDEX "+qualified{st.src.schema, n}.sql()+" RENAME TO "+quoteIdent(st.renamed[n]))
	}
	stmts = append(stmts, "ALTER TABLE "+st.src.sql()+" RENAME TO "+quoteIdent(st.dest.name))
	for _, s := range stmts {
		if _, err := side.Exec(ctx, s).ReadAll(); err != nil {
			return fmt.Errorf("rename in the side instance (%s): %w", s, err)
		}
	}
	return nil
}

// pgDumpVersionRE reads pg_dump's version line.
var pgDumpVersionRE = regexp.MustCompile(`\(PostgreSQL\) (\d+)`)

// dumpRestore pipes pg_dump of the tables from the side instance into
// pg_restore against production, in one transaction: a failure loads
// nothing.
func (e *Engine) dumpRestore(ctx context.Context, side *pgconn.PgConn, sc instance.Conn, db string, t engine.Target, cred config.Credential, tables []*sideTable, inPlace bool) error {
	dump, err := e.findTool("pg_dump")
	if err != nil {
		return err
	}
	restore, err := e.findTool("pg_restore")
	if err != nil {
		return err
	}
	if err := e.checkToolVersion(ctx, dump, side); err != nil {
		return err
	}
	sidePass, err := toolrun.WriteSecretFile(e.Runner.SecretDir, ".pgpass", toolrun.PGPass(toolrun.PGPassEntry{
		Host: sc.Host, Port: strconv.Itoa(sc.Port), Database: db, User: sc.User, Password: sc.Password}))
	if err != nil {
		return err
	}
	dumpArgs := []string{"--format=custom", "--no-owner", "--dbname=" + fmt.Sprintf("host=%s port=%d dbname=%s user=%s sslmode=disable application_name=pentym",
		conninfoValue(sc.Host), sc.Port, conninfoValue(db), conninfoValue(sc.User))}
	if !inPlace {
		dumpArgs = append(dumpArgs, "--no-privileges")
	}
	for _, st := range tables {
		dumpArgs = append(dumpArgs, "--table="+st.dest.sql())
		// Owned sequences by name too, so their values come along.
		for _, n := range st.seqs {
			if r, ok := st.renamed[n]; ok {
				n = r
			}
			dumpArgs = append(dumpArgs, "--table="+qualified{st.dest.schema, n}.sql())
		}
	}
	dumpCmd := &toolrun.Cmd{Tool: "pg_dump", Path: dump, RunID: e.RunID, Args: dumpArgs,
		Env: []string{"PGPASSFILE=" + sidePass.Path}, Files: []*toolrun.SecretFile{sidePass}, Secrets: []secrets.Secret{sc.Password}}

	restoreArgs := []string{"--no-owner", "--exit-on-error", "--single-transaction", "--dbname=" + conninfo(t.Config, cred, db)}
	if !inPlace {
		restoreArgs = append(restoreArgs, "--no-privileges")
	}
	restoreCmd := &toolrun.Cmd{Tool: "pg_restore", Path: restore, RunID: e.RunID, Args: restoreArgs}
	pw, err := e.password(ctx, t.Name, cred)
	if err != nil {
		return err
	}
	defer pw.Clear()
	if !pw.IsEmpty() {
		port := ""
		if t.Config.Port != 0 {
			port = strconv.Itoa(t.Config.Port)
		}
		f, err := toolrun.WriteSecretFile(e.Runner.SecretDir, ".pgpass",
			toolrun.PGPass(toolrun.PGPassEntry{Host: t.Config.Host, Port: port, Database: db, User: cred.User, Password: pw}))
		if err != nil {
			return err
		}
		restoreCmd.Files, restoreCmd.Secrets = []*toolrun.SecretFile{f}, []secrets.Secret{pw}
		restoreCmd.Env = []string{"PGPASSFILE=" + f.Path}
	}
	pr, pwr := io.Pipe()
	dumpCmd.Stdout, restoreCmd.Stdin = pwr, pr
	dumped := make(chan error, 1)
	go func() {
		err := e.Runner.Run(ctx, dumpCmd)
		_ = pwr.CloseWithError(err)
		dumped <- err
	}()
	rerr := e.Runner.Run(ctx, restoreCmd)
	_ = pr.CloseWithError(rerr)
	derr := <-dumped
	if derr != nil {
		return fmt.Errorf("dump from the side instance: %w", derr)
	}
	if rerr != nil {
		return fmt.Errorf("load into production (nothing was loaded): %w", rerr)
	}
	return nil
}

// checkToolVersion makes sure pg_dump can read the side instance: it must
// be at least the server's major version.
func (e *Engine) checkToolVersion(ctx context.Context, dump string, side *pgconn.PgConn) error {
	var out bytes.Buffer
	if err := e.Runner.Run(ctx, &toolrun.Cmd{Tool: "pg_dump", Path: dump, RunID: e.RunID, Args: []string{"--version"}, Stdout: &out}); err != nil {
		return err
	}
	m := pgDumpVersionRE.FindStringSubmatch(out.String())
	server, _, _ := serverMajor(side)
	if m == nil {
		return fmt.Errorf("pg_dump --version said %q", strings.TrimSpace(out.String()))
	}
	if v, _ := strconv.Atoi(m[1]); v < server {
		return &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
			Level: msg.LevelFailed, Fact: fmt.Sprintf("%s is version %d, older than the restored server's %d", dump, v, server),
			Next: fmt.Sprintf("install the PostgreSQL %d client tools and add their directory to tools.postgres", server),
		}}
	}
	return nil
}

// sideConnectDB connects to a database of the side instance.
func sideConnectDB(ctx context.Context, c instance.Conn, user string, password secrets.Secret, db string) (*pgconn.PgConn, error) {
	cfg, err := pgconn.ParseConfig(fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=disable connect_timeout=10 application_name=pentym",
		conninfoValue(c.Host), c.Port, conninfoValue(user), conninfoValue(db)))
	if err != nil {
		return nil, err
	}
	cfg.Password = string(password.Expose())
	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, secrets.ScrubError(err, password)
	}
	return conn, nil
}
