// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package mongodb

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/devtools/leaktest"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// oneArchive serves a snapshot's mongodump archive.
type oneArchive struct{ data []byte }

func (oneArchive) Names() []string { return []string{ArchiveName} }

func (a oneArchive) Archive(_ context.Context, name string, w io.Writer) error {
	if name != ArchiveName {
		return errors.New("no archive " + name)
	}
	_, err := w.Write(a.data)
	return err
}

type sideInst struct {
	conn    instance.Conn
	stopped atomic.Bool
}

func (s *sideInst) Conn() instance.Conn        { return s.conn }
func (s *sideInst) Stop(context.Context) error { s.stopped.Store(true); return nil }

func TestRestore(t *testing.T) {
	canary := leaktest.New(t, 1)[0]
	archive := sampleArchive()
	for _, tt := range []struct {
		name   string
		start  engine.Position
		replay bool
	}{{"replica set", "1732600000:1", true}, {"standalone", "", false}} {
		t.Run(tt.name, func(t *testing.T) {
			e, log := fakeEngine(t, "restore", nil)
			side := &sideInst{conn: instance.Conn{Host: "127.0.0.1", Port: 27999, User: "pentym", Password: secrets.New([]byte(canary))}}
			var version, dataDir string
			start := func(_ context.Context, v, dir string) (instance.Instance, error) {
				version, dataDir = v, dir
				return side, nil
			}
			inst, reached, err := e.Restore(context.Background(), engine.RestoreRequest{
				Archives: oneArchive{archive},
				Backup:   engine.BaseBackup{EngineVersion: "8.0.4", Start: tt.start, End: "1732600005:1"},
				DataDir:  "/work/restore-1",
			}, start)
			if err != nil {
				t.Fatal(err)
			}
			if inst != side || reached.Position != "1732600005:1" || version != "8.0" || dataDir != "/work/restore-1" {
				t.Errorf("instance %v, reached %+v, started %s over %s", inst, reached, version, dataDir)
			}
			rec := readRecord(t, log)
			want := []string{"--uri=mongodb://127.0.0.1:27999/?directConnection=true&authSource=admin", "--username=pentym"}
			if !slices.Equal(rec.Args[:2], want) || !strings.HasPrefix(rec.Args[2], "--config=") ||
				!slices.Equal(rec.Args[3:5], []string{"--archive", "--stopOnError"}) || slices.Contains(rec.Args, "--oplogReplay") != tt.replay {
				t.Errorf("args = %q", rec.Args)
			}
			if !bytes.Equal(rec.Stdin, archive) || rec.Config != `password: "`+string(canary)+`"`+"\n" {
				t.Errorf("stdin %d bytes, config %q", len(rec.Stdin), rec.Config)
			}
			leaktest.Scan(t, "mongorestore arguments and environment", []leaktest.Canary{canary}, strings.Join(rec.Args, " "), strings.Join(rec.Env, "\n"))
			if entries, _ := os.ReadDir(e.Runner.SecretDir); len(entries) != 0 {
				t.Errorf("secret files left: %v", entries)
			}
		})
	}
}

func TestRestoreFailures(t *testing.T) {
	e, _ := fakeEngine(t, "restore-fail", nil)
	side := &sideInst{conn: instance.Conn{Host: "127.0.0.1", Port: 1, User: "pentym", Password: secrets.New([]byte("pw"))}}
	start := func(context.Context, string, string) (instance.Instance, error) { return side, nil }
	req := engine.RestoreRequest{Archives: oneArchive{sampleArchive()}, Backup: engine.BaseBackup{EngineVersion: "7.0.14"}}
	_, _, err := e.Restore(context.Background(), req, start)
	var ee *toolrun.ExitError
	if !errors.As(err, &ee) || !strings.Contains(ee.Error(), "duplicate key") || !side.stopped.Load() {
		t.Errorf("a failing mongorestore = %v; instance stopped %v", err, side.stopped.Load())
	}
	req.Backup.EngineVersion = "eight"
	if _, _, err := e.Restore(context.Background(), req, start); err == nil {
		t.Error("an unreadable version was accepted")
	}
	req.Point = engine.Point{Before: "1:1"}
	if _, _, err := e.Restore(context.Background(), req, start); !errors.Is(err, errNotYet) {
		t.Errorf("restore to a point = %v", err)
	}
}
