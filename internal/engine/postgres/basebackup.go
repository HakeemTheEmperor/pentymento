// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// Archive names in a Postgres snapshot.
const (
	BaseArchive     = "base.tar"
	ManifestArchive = "backup_manifest"
)

// tablespaceArchiveRE is a tablespace archive's name: its OID.
var tablespaceArchiveRE = regexp.MustCompile(`^[0-9]+\.tar$`)

// BaseBackup takes a base backup (PRD 7.2): natively with BASE_BACKUP over
// a replication connection, falling back to pg_basebackup when the native
// path fails before any data arrives. The WAL the backup needs is included,
// so a snapshot restores on its own, and a temporary replication slot
// keeps the server from removing it meanwhile. Nothing is compressed at
// the source.
func (e *Engine) BaseBackup(ctx context.Context, t engine.Target, sink engine.ArchiveSink) (*engine.BaseBackup, error) {
	bb, started, err := e.nativeBaseBackup(ctx, t, sink)
	if err == nil || started || ctx.Err() != nil || !fallbackWorthy(err) {
		return bb, err
	}
	e.logger().Warn("native base backup failed; trying pg_basebackup",
		"target", t.Name, "error", err.Error())
	bb, ferr := e.pgBasebackup(ctx, t, sink)
	if ferr != nil {
		return nil, fmt.Errorf("%w; pg_basebackup also failed: %w", err, ferr)
	}
	return bb, nil
}

// fallbackWorthy reports whether pg_basebackup might succeed where the
// native protocol failed: not for configuration, connection or access
// problems, which it would hit too.
func fallbackWorthy(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "28") {
		return false
	}
	return !errors.Is(err, errConnect) && !errors.Is(err, errUnsupported)
}

// Sentinels that rule out the fallback.
var (
	errConnect     = errors.New("connection failed")
	errUnsupported = errors.New("unsupported server")
)

// nativeBaseBackup runs BASE_BACKUP. started reports whether any archive
// reached the sink, after which a fallback could not start over cleanly.
func (e *Engine) nativeBaseBackup(ctx context.Context, t engine.Target, sink engine.ArchiveSink) (_ *engine.BaseBackup, started bool, err error) {
	conn, err := e.connect(ctx, t, true)
	if err != nil {
		return nil, false, errors.Join(errConnect, err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	major, version, err := serverMajor(conn)
	if err != nil {
		return nil, false, err
	}
	if !supported(major) {
		return nil, false, errors.Join(errUnsupported, versionError(t, version))
	}
	sys, err := pglogrepl.IdentifySystem(ctx, conn)
	if err != nil {
		return nil, false, fmt.Errorf("IDENTIFY_SYSTEM: %w", err)
	}
	segSize, err := walSegmentSize(ctx, conn)
	if err != nil {
		return nil, false, err
	}
	// The temporary slot reserves WAL from now until the connection
	// closes, so the WAL the backup includes cannot be removed meanwhile.
	slot := "pentym_bb_" + randomHex(6)
	if err := conn.Exec(ctx, "CREATE_REPLICATION_SLOT "+slot+" TEMPORARY PHYSICAL (RESERVE_WAL)").Close(); err != nil {
		return nil, false, fmt.Errorf("create a temporary replication slot: %w", err)
	}
	start, err := pglogrepl.StartBaseBackup(ctx, conn, pglogrepl.BaseBackupOptions{
		Label:         "pentym " + e.RunID,
		WAL:           true, // the WAL the backup needs, in base.tar's pg_wal
		NoWait:        true, // do not wait for WAL archiving: the WAL is included
		TablespaceMap: true,
		Manifest:      true,
	})
	if err != nil {
		return nil, false, fmt.Errorf("BASE_BACKUP: %w", err)
	}
	started, err = receiveArchives(ctx, conn, sink)
	if err != nil {
		return nil, started, err
	}
	end, err := pglogrepl.FinishBaseBackup(ctx, conn)
	if err != nil {
		return nil, true, fmt.Errorf("finish BASE_BACKUP: %w", err)
	}
	return &engine.BaseBackup{
		EngineVersion: version, Method: "native", SystemID: sys.SystemID,
		Start: engine.Position(start.LSN.String()), End: engine.Position(end.LSN.String()),
		Timeline: strconv.Itoa(int(end.TimelineID)), WALSegmentSize: int64(segSize), // #nosec G115 -- at most 1 GiB
	}, true, nil
}

// receiveArchives copies the backup's COPY stream into the sink. Since
// Postgres 15 the stream carries typed messages: 'n' starts an archive,
// 'm' starts the manifest, 'd' is data, 'p' is progress.
func receiveArchives(ctx context.Context, conn *pgconn.PgConn, sink engine.ArchiveSink) (bool, error) {
	var w io.Writer
	started := false
	for {
		m, err := conn.ReceiveMessage(ctx)
		if err != nil {
			return started, fmt.Errorf("receive the base backup: %w", err)
		}
		switch m := m.(type) {
		case *pgproto3.CopyOutResponse, *pgproto3.NoticeResponse:
		case *pgproto3.CopyData:
			if len(m.Data) == 0 {
				continue
			}
			switch m.Data[0] {
			case 'n':
				name, _, ok := bytes.Cut(m.Data[1:], []byte{0})
				if !ok {
					return started, errors.New("malformed new-archive message")
				}
				n := filepath.Base(strings.TrimPrefix(string(name), "./"))
				if n != BaseArchive && !tablespaceArchiveRE.MatchString(n) {
					return started, fmt.Errorf("unexpected archive %q in the base backup", name)
				}
				if w, err = sink.Archive(ctx, n); err != nil {
					return started, err
				}
				started = true
			case 'm':
				if w, err = sink.Archive(ctx, ManifestArchive); err != nil {
					return started, err
				}
				started = true
			case 'd':
				if w == nil {
					return started, errors.New("base backup data before any archive")
				}
				if _, err := w.Write(m.Data[1:]); err != nil {
					return started, err
				}
			case 'p':
			default:
				return started, fmt.Errorf("unexpected base backup message type %q", m.Data[0])
			}
		case *pgproto3.CopyDone:
			return started, nil
		case *pgproto3.ErrorResponse:
			return started, pgconn.ErrorResponseToPgError(m)
		default:
			return started, fmt.Errorf("unexpected message %T during the base backup", m)
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pg_basebackup -v reports the backup's start and end on stderr.
var (
	walStartRE = regexp.MustCompile(`write-ahead log start point: ([0-9A-F]+/[0-9A-F]+) on timeline ([0-9]+)`)
	walEndRE   = regexp.MustCompile(`write-ahead log end point: ([0-9A-F]+/[0-9A-F]+)`)
)

// pgBasebackup is the fallback: pg_basebackup writing one tar to stdout.
// To stdout it can write only one archive, so it refuses a cluster with
// tablespaces, and it cannot send a manifest; the WAL is included by
// fetching it at the end (-X fetch).
func (e *Engine) pgBasebackup(ctx context.Context, t engine.Target, sink engine.ArchiveSink) (*engine.BaseBackup, error) {
	cred, err := backupCred(t)
	if err != nil {
		return nil, err
	}
	bin, err := e.findTool("pg_basebackup")
	if err != nil {
		return nil, err
	}
	cmd := &toolrun.Cmd{
		Tool: "pg_basebackup", Path: bin, RunID: e.RunID,
		Args: []string{"--dbname=" + conninfo(t.Config, cred, ""), "--pgdata=-", "--format=tar",
			"--wal-method=fetch", "--no-manifest", "--label=pentym " + e.RunID, "--verbose"},
	}
	pw, err := e.password(ctx, t.Name, cred)
	if err != nil {
		return nil, err
	}
	defer pw.Clear()
	if !pw.IsEmpty() {
		port := ""
		if t.Config.Port != 0 {
			port = strconv.Itoa(t.Config.Port)
		}
		f, err := toolrun.WriteSecretFile(e.Runner.SecretDir, ".pgpass",
			toolrun.PGPass(toolrun.PGPassEntry{Host: t.Config.Host, Port: port, Database: "replication", User: cred.User, Password: pw}))
		if err != nil {
			return nil, err
		}
		cmd.Files, cmd.Secrets = []*toolrun.SecretFile{f}, []secrets.Secret{pw}
		cmd.Env = []string{"PGPASSFILE=" + f.Path}
	}
	var mu sync.Mutex
	bb := &engine.BaseBackup{Method: "pg_basebackup"}
	cmd.OnStderr = func(line string) {
		mu.Lock()
		defer mu.Unlock()
		if m := walStartRE.FindStringSubmatch(line); m != nil {
			bb.Start, bb.Timeline = engine.Position(m[1]), m[2]
		}
		if m := walEndRE.FindStringSubmatch(line); m != nil {
			bb.End = engine.Position(m[1])
		}
	}
	w, err := sink.Archive(ctx, BaseArchive)
	if err != nil {
		return nil, err
	}
	cmd.Stdout = w
	if err := e.Runner.Run(ctx, cmd); err != nil {
		return nil, err
	}
	if bb.Start == "" || bb.End == "" {
		return nil, errors.New("pg_basebackup did not report the backup's WAL start and end points")
	}
	// The server's version and system ID come from a normal connection.
	if conn, err := e.connect(ctx, t, false); err == nil {
		_, bb.EngineVersion, _ = serverMajor(conn)
		var out [][][]byte
		if res, err := conn.Exec(ctx, "SELECT system_identifier::text FROM pg_control_system()").ReadAll(); err == nil && len(res) == 1 {
			out = res[0].Rows
		}
		if len(out) == 1 && len(out[0]) == 1 {
			bb.SystemID = string(out[0][0])
		}
		if size, err := walSegmentSize(ctx, conn); err == nil {
			bb.WALSegmentSize = int64(size) // #nosec G115 -- at most 1 GiB
		}
		_ = conn.Close(context.WithoutCancel(ctx))
	}
	return bb, nil
}

// findTool finds a client tool in tools.postgres, then on PATH.
func (e *Engine) findTool(name string) (string, error) {
	return toolrun.Find(name, "postgres", e.ToolDirs, e.LookPath)
}
