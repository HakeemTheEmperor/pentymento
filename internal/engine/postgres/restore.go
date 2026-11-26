// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package postgres

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// SideRole is the superuser pentym creates in a restored side instance:
// the restored cluster's own roles have passwords pentym does not know.
const SideRole = "pentym_side"

// originalSuffix marks the restored cluster's own configuration files,
// set aside so the side instance runs with pentym's.
const originalSuffix = ".pentym-original"

// configFiles are the restored cluster's configuration, replaced for the
// side instance.
var configFiles = []string{"postgresql.conf", "postgresql.auto.conf", "pg_hba.conf", "pg_ident.conf"}

// sideConf is the side instance's postgresql.conf. The instance provider
// appends the address, port and sockets to postgresql.auto.conf; nothing
// from production (archiving, preloaded libraries, TLS files, paths)
// applies.
const sideConf = `# Written by pentym for a side instance restored from a snapshot.
# The cluster's own configuration files are kept beside this one, ending
# in ` + originalSuffix + `.
listen_addresses = '*'
password_encryption = 'scram-sha-256'
archive_mode = off
shared_preload_libraries = ''
ssl = off
logging_collector = off
log_destination = 'stderr'
`

// trustHBA lets pentym log in while it creates its role: a window of a few
// seconds on an instance that listens only on the loopback interface (or,
// under Docker, is published only there).
const trustHBA = `# Written by pentym: trust only while pentym sets up its own role.
host all all 0.0.0.0/0 trust
host all all ::/0 trust
`

// lockedHBA lets only pentym's side role in, with its password.
const lockedHBA = `# Written by pentym: only pentym's side role, with its password.
host all ` + SideRole + ` 0.0.0.0/0 scram-sha-256
host all ` + SideRole + ` ::/0 scram-sha-256
`

// Restore materializes a snapshot into req.DataDir and starts a side
// instance over it (SDD 6.4 step 4): base.tar into the data directory,
// each tablespace archive as an in-place tablespace under pg_tblspc, so
// no link points outside the directory. The WAL in base.tar brings the
// cluster to a consistent state when it starts. pentym's own
// configuration replaces production's, and access is locked to a new
// superuser with a random password once pentym has created it.
func (e *Engine) Restore(ctx context.Context, req engine.RestoreRequest, start engine.StartFunc) (instance.Instance, *engine.Reached, error) {
	if req.Point != (engine.Point{}) {
		return nil, nil, fmt.Errorf("restoring to a point: %w", errNotYet)
	}
	for _, name := range req.Archives.Names() {
		var dir string
		switch {
		case name == BaseArchive:
			dir = req.DataDir
		case tablespaceArchiveRE.MatchString(name):
			dir = filepath.Join(req.DataDir, "pg_tblspc", strings.TrimSuffix(name, ".tar"))
		default:
			continue // backup_manifest: not part of the cluster
		}
		if err := e.extractArchive(ctx, req.Archives, name, dir); err != nil {
			return nil, nil, fmt.Errorf("restore %s: %w", name, err)
		}
	}
	if err := prepareConfig(req.DataDir); err != nil {
		return nil, nil, err
	}
	major, _, _ := strings.Cut(req.Backup.EngineVersion, ".")
	inst, err := start(ctx, major, req.DataDir)
	if err != nil {
		return nil, nil, err
	}
	conn, err := e.lockSideInstance(ctx, inst.Conn(), req)
	if err != nil {
		_ = inst.Stop(context.WithoutCancel(ctx))
		return nil, nil, err
	}
	return &sideInstance{Instance: inst, conn: conn}, &engine.Reached{Position: req.Backup.End}, nil
}

// sideInstance is a started instance with pentym's side role as its login.
type sideInstance struct {
	instance.Instance
	conn instance.Conn
}

func (s *sideInstance) Conn() instance.Conn { return s.conn }

// extractArchive streams one archive into dir.
func (e *Engine) extractArchive(ctx context.Context, src engine.ArchiveSource, name, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := src.Archive(ctx, name, pw)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	if err := extractTar(pr, dir, e.logger().Warn); err != nil {
		_ = pr.CloseWithError(err)
		if serr := <-done; serr != nil && !errors.Is(serr, err) {
			e.logger().Debug("the snapshot reader stopped", "error", serr)
		}
		return err
	}
	// The tar's end-of-archive padding may follow; let the reader finish.
	_, _ = io.Copy(io.Discard, pr)
	return <-done
}

// extractTar unpacks a Postgres archive into dir: directories and regular
// files only, every path kept inside dir, files 0600 and directories 0700
// as Postgres requires. Links and special files are skipped with a
// warning: base backups carry none that a restore needs.
func extractTar(r io.Reader, dir string, warn func(string, ...any)) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("the archive does not parse: %w", err)
		}
		path, err := insidePath(root, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(path, tr); err != nil {
				return err
			}
		default:
			warn("skipped an archive entry that is not a file or directory", "entry", h.Name, "type", string(h.Typeflag))
		}
	}
}

// insidePath maps an archive entry name to a path under root, refusing
// absolute names and names that climb out.
func insidePath(root, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) || filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("the archive entry %q points outside the data directory", name)
	}
	return filepath.Join(root, clean), nil
}

func writeFile(path string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- a path checked to be inside the data directory
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	return errors.Join(err, f.Close())
}

// prepareConfig sets the restored cluster's configuration aside, writes
// the side instance's, and removes what would make Postgres behave as a
// standby or look for tablespaces elsewhere: the tablespaces are already
// in place under pg_tblspc.
func prepareConfig(dataDir string) error {
	for _, name := range configFiles {
		p := filepath.Join(dataDir, name)
		if err := os.Rename(p, p+originalSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, name := range []string{"standby.signal", "recovery.signal", "tablespace_map", "postmaster.pid", "postmaster.opts"} {
		if err := os.Remove(filepath.Join(dataDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for name, content := range map[string]string{"postgresql.conf": sideConf, "pg_hba.conf": trustHBA, "pg_ident.conf": ""} {
		if err := os.WriteFile(filepath.Join(dataDir, name), []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// Waiting for a restored instance: it accepts no connection until its
// recovery is done, which takes as long as the WAL takes to replay; a
// server that never listens has failed to start.
const (
	listenTimeout = 2 * time.Minute
	pollInterval  = 500 * time.Millisecond
)

// sideConnect connects to the side instance as user, with a password or
// none (trust), waiting while the server starts and recovers.
func sideConnect(ctx context.Context, c instance.Conn, user string, password secrets.Secret) (*pgconn.PgConn, error) {
	conninfo := fmt.Sprintf("host=%s port=%d user=%s dbname=template1 sslmode=disable connect_timeout=10 application_name=pentym",
		conninfoValue(c.Host), c.Port, conninfoValue(user))
	cfg, err := pgconn.ParseConfig(conninfo)
	if err != nil {
		return nil, err
	}
	cfg.Password = string(password.Expose())
	listening := false
	deadline := time.Now().Add(listenTimeout)
	for {
		conn, err := pgconn.ConnectConfig(ctx, cfg)
		if err == nil {
			return conn, nil
		}
		var pgErr *pgconn.PgError
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.As(err, &pgErr) && pgErr.Code == "57P03": // starting up or recovering
			listening = true
		case errors.As(err, &pgErr):
			return nil, secrets.ScrubError(err, password)
		case !listening && time.Now().After(deadline):
			return nil, fmt.Errorf("the side instance did not start listening within %s: %w", listenTimeout, secrets.ScrubError(err, password))
		}
		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// lockSideInstance logs in through the trust window, creates pentym's
// side role with a random password, allows only it, and returns its
// login. It logs in first as the target's backup role, which exists in the
// backup, to find the cluster's bootstrap superuser.
func (e *Engine) lockSideInstance(ctx context.Context, c instance.Conn, req engine.RestoreRequest) (instance.Conn, error) {
	var conn *pgconn.PgConn
	var err error
	for _, user := range uniqueNonEmpty(req.Target.Config.Credentials.Backup.User, "postgres") {
		if conn, err = sideConnect(ctx, c, user, secrets.Secret{}); err == nil {
			break
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "28000" { // role does not exist
			return instance.Conn{}, fmt.Errorf("log in to the side instance: %w", err)
		}
	}
	if err != nil {
		return instance.Conn{}, fmt.Errorf("log in to the side instance: %w", err)
	}
	rows, err := query(ctx, conn, "SELECT rolname FROM pg_roles WHERE oid = 10")
	_ = conn.Close(context.WithoutCancel(ctx))
	if err != nil || len(rows) != 1 {
		return instance.Conn{}, fmt.Errorf("find the bootstrap superuser: %v", err)
	}
	conn, err = sideConnect(ctx, c, rows[0][0], secrets.Secret{})
	if err != nil {
		return instance.Conn{}, fmt.Errorf("log in to the side instance as its bootstrap superuser: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return instance.Conn{}, err
	}
	pw := secrets.New([]byte(hex.EncodeToString(b)))
	sql := fmt.Sprintf(`DO $$ BEGIN
IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%[1]s') THEN ALTER ROLE %[1]s SUPERUSER LOGIN PASSWORD '%[2]s';
ELSE CREATE ROLE %[1]s SUPERUSER LOGIN PASSWORD '%[2]s'; END IF; END $$`, SideRole, pw.Expose())
	if _, err := conn.Exec(ctx, sql).ReadAll(); err != nil {
		return instance.Conn{}, fmt.Errorf("create the side role: %w", secrets.ScrubError(err, pw))
	}
	if err := os.WriteFile(filepath.Join(req.DataDir, "pg_hba.conf"), []byte(lockedHBA), 0o600); err != nil {
		return instance.Conn{}, err
	}
	if _, err := conn.Exec(ctx, "SELECT pg_reload_conf()").ReadAll(); err != nil {
		return instance.Conn{}, fmt.Errorf("reload the side instance's access rules: %w", err)
	}
	// The reload is asynchronous: wait until a login without a password
	// is refused, which shows the trust window has closed, then check the
	// side role logs in with its password.
	boot := rows[0][0]
	for attempt := 0; ; attempt++ {
		open, err := sideConnect(ctx, c, boot, secrets.Secret{})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && (pgErr.Code == "28000" || pgErr.Code == "28P01") {
				break
			}
			return instance.Conn{}, fmt.Errorf("check the side instance's access rules: %w", err)
		}
		_ = open.Close(context.WithoutCancel(ctx))
		if attempt == 50 {
			return instance.Conn{}, errors.New("the side instance still trusts logins after its access rules were locked")
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return instance.Conn{}, ctx.Err()
		}
	}
	check, err := sideConnect(ctx, c, SideRole, pw)
	if err != nil {
		return instance.Conn{}, fmt.Errorf("log in as %s: %w", SideRole, err)
	}
	_ = check.Close(context.WithoutCancel(ctx))
	return instance.Conn{Host: c.Host, Port: c.Port, User: SideRole, Password: pw}, nil
}

func uniqueNonEmpty(names ...string) []string {
	var out []string
	for _, n := range names {
		if n != "" && !contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
