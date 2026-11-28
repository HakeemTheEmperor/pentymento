// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package postgres

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
)

type tarEntry struct {
	name, body string
	typ        byte
}

func makeTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body))}
		if e.typ == tar.TypeDir {
			h.Size = 0
		}
		if e.typ == tar.TypeSymlink {
			h.Linkname, h.Size = e.body, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// memArchives serves archives from memory, in order.
type memArchives struct {
	names []string
	data  map[string][]byte
}

func (m *memArchives) Names() []string { return m.names }

func (m *memArchives) Archive(_ context.Context, name string, w io.Writer) error {
	_, err := w.Write(m.data[name])
	return err
}

func TestExtractTar(t *testing.T) {
	dir := t.TempDir()
	var warned []string
	warn := func(m string, _ ...any) { warned = append(warned, m) }
	data := makeTar(t,
		tarEntry{"global/", "", tar.TypeDir},
		tarEntry{"global/pg_control", "control", tar.TypeReg},
		tarEntry{"base/1/1259", "heap", tar.TypeReg}, // its directory comes implicitly
		tarEntry{"pg_wal/000000010000000000000002", "wal", tar.TypeReg},
		tarEntry{"conf.d/link.conf", "/etc/postgresql/x.conf", tar.TypeSymlink},
	)
	if err := extractTar(bytes.NewReader(data), dir, warn); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"global/pg_control": "control", "base/1/1259": "heap", "pg_wal/000000010000000000000002": "wal"} {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path))) // #nosec G304 -- test file
		if err != nil || string(b) != want {
			t.Errorf("%s = %q, %v", path, b, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "conf.d", "link.conf")); !errors.Is(err, os.ErrNotExist) || len(warned) != 1 {
		t.Errorf("the symlink was created (%v) or not warned about (%v)", err, warned)
	}
	if fi, err := os.Stat(filepath.Join(dir, "global", "pg_control")); err == nil && os.PathSeparator == '/' && fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", fi.Mode().Perm())
	}

	for _, bad := range []string{"../escape", "base/../../escape", "/etc/passwd"} {
		err := extractTar(bytes.NewReader(makeTar(t, tarEntry{bad, "x", tar.TypeReg})), t.TempDir(), warn)
		if err == nil || !strings.Contains(err.Error(), "points outside the data directory") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if err := extractTar(strings.NewReader("not a tar at all, but long enough to be read as a header block"), t.TempDir(), warn); err == nil {
		t.Error("garbage extracted")
	}
}

func TestPrepareConfig(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"postgresql.conf": "shared_preload_libraries = 'pg_cron'", "pg_hba.conf": "host all all 10.0.0.0/8 scram-sha-256",
		"postgresql.auto.conf": "primary_conninfo = 'host=db'", "standby.signal": "", "tablespace_map": "16384 /mnt/ts",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareConfig(dir); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- test file
		return string(b)
	}
	if read("postgresql.conf") != sideConf || read("pg_hba.conf") != trustHBA || read("pg_ident.conf") != "" {
		t.Error("the side configuration was not written")
	}
	if read("postgresql.conf"+originalSuffix) != "shared_preload_libraries = 'pg_cron'" || read("postgresql.auto.conf"+originalSuffix) != "primary_conninfo = 'host=db'" {
		t.Error("the original configuration was not kept")
	}
	for _, gone := range []string{"standby.signal", "tablespace_map", "postgresql.auto.conf"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there", gone)
		}
	}
	for _, want := range []string{"archive_mode = off", "shared_preload_libraries = ''", "ssl = off", "allow_in_place_tablespaces = on"} {
		if !strings.Contains(sideConf, want) {
			t.Errorf("the side configuration lacks %q", want)
		}
	}
	if strings.Contains(lockedHBA, "trust") || !strings.Contains(lockedHBA, SideRole) {
		t.Error("the locked access rules still trust, or omit the side role")
	}
}

// TestRestoreMaterializes: the archives land in the data directory, each
// tablespace in place under pg_tblspc, and the instance starts over it at
// the snapshot's major version.
func TestRestoreMaterializes(t *testing.T) {
	dir := t.TempDir()
	src := &memArchives{
		names: []string{"16384.tar", BaseArchive, ManifestArchive},
		data: map[string][]byte{
			BaseArchive: makeTar(t, tarEntry{"PG_VERSION", "17\n", tar.TypeReg}, tarEntry{"pg_tblspc/", "", tar.TypeDir},
				tarEntry{"tablespace_map", "16384 /mnt/ts\n", tar.TypeReg}, tarEntry{"backup_label", "label", tar.TypeReg}),
			"16384.tar":     makeTar(t, tarEntry{"PG_17_202406281/16385/16386", "rows", tar.TypeReg}),
			ManifestArchive: []byte(`{"PostgreSQL-Backup-Manifest-Version": 2}`),
		},
	}
	stop := errors.New("no server in unit tests")
	var gotVersion, gotDir string
	start := func(_ context.Context, version, dataDir string) (instance.Instance, error) {
		gotVersion, gotDir = version, dataDir
		return nil, stop
	}
	e := &Engine{}
	_, _, err := e.Restore(context.Background(), engine.RestoreRequest{
		Archives: src, Backup: engine.BaseBackup{EngineVersion: "17.2", End: "0/3000100"}, DataDir: dir,
	}, start)
	if !errors.Is(err, stop) || gotVersion != "17" || gotDir != dir {
		t.Fatalf("Restore = %v; started %q over %q", err, gotVersion, gotDir)
	}
	b, err := os.ReadFile(filepath.Join(dir, "pg_tblspc", "16384", "PG_17_202406281", "16385", "16386")) // #nosec G304 -- test file
	if err != nil || string(b) != "rows" {
		t.Errorf("tablespace file = %q, %v", b, err)
	}
	for _, want := range []string{"PG_VERSION", "backup_label"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s: %v", want, err)
		}
	}
	for _, gone := range []string{"tablespace_map", ManifestArchive} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is in the data directory", gone)
		}
	}
	// A restore to a point replays stored WAL, which it must be given.
	if _, _, err := e.Restore(context.Background(), engine.RestoreRequest{Point: engine.Point{XID: "1"}}, start); err == nil || !strings.Contains(err.Error(), "needs the stored WAL") {
		t.Errorf("restore to a point without WAL = %v", err)
	}
}
