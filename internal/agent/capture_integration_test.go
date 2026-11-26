// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/devtools/dbtest"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/engine/postgres"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// Environment for a capture child: the server to capture and the role.
const (
	envPGHost = "PENTYM_IT_PG_HOST"
	envPGPort = "PENTYM_IT_PG_PORT"
	envReplPW = "PENTYM_IT_REPL_PW" // #nosec G101 -- the name of a variable, not a secret
)

// pgCaptureTarget is the configured target for the test server.
func pgCaptureTarget(host string, port int) config.Target {
	ref, _ := secrets.ParseRef("env:" + envReplPW)
	return config.Target{Engine: "postgres", Host: host, Port: port, Database: "postgres", SSLMode: "disable",
		Credentials: config.Credentials{Replication: config.Credential{User: "pentym_repl", Password: ref}}}
}

func pgEngine(dir string) *postgres.Engine {
	return &postgres.Engine{
		Resolver: secrets.Resolver{Getenv: os.Getenv},
		Runner:   &toolrun.Runner{SecretDir: filepath.Join(dir, "secrets")},
	}
}

// captureChildMain runs an agent that captures the test server, printing
// its checkpoints, until its stdin closes.
func captureChildMain(dir string) int {
	port, _ := strconv.Atoi(os.Getenv(envPGPort))
	cfg := &config.Config{Targets: map[string]config.Target{"prod-pg": pgCaptureTarget(os.Getenv(envPGHost), port)}}
	r := openAs(&childTB{}, dir, nil, "agent")
	eng := pgEngine(os.TempDir())
	out := &syncWriter{w: os.Stdout}
	a, err := New(Options{
		Config: cfg, Repo: r, Host: "child", Timings: fastTimings, Heartbeat: 100 * time.Millisecond,
		Engine: func(Target) (engine.Engine, error) { return eng, nil },
		OnLease: func(_ string, held bool, gen uint64) {
			if held {
				out.line(fmt.Sprintf("held %d", gen))
			}
		},
	})
	if err != nil {
		out.line("error " + err.Error())
		return 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = os.Stdin.Read(make([]byte, 1)); cancel() }()
	out.line("started")
	if err := a.Run(ctx); err != nil {
		out.line("error " + err.Error())
		return 1
	}
	out.line("stopped")
	return 0
}

func init() {
	if dir := os.Getenv("PENTYM_AGENT_CAPTURE_CHILD"); dir != "" {
		os.Exit(captureChildMain(dir))
	}
}

// pgServer starts Postgres with a replication role and returns its
// superuser connection string.
func pgServer(t *testing.T, version string) (string, string, int) {
	t.Helper()
	pg := dbtest.StartPostgres(t, version)
	ctx := context.Background()
	exec := func(sql string) string {
		conn, err := pgconn.Connect(ctx, pg.ConnString)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close(ctx) }()
		res, err := conn.Exec(ctx, sql).ReadAll()
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if len(res) > 0 && len(res[len(res)-1].Rows) > 0 {
			return string(res[len(res)-1].Rows[0][0])
		}
		return ""
	}
	hba := exec("SELECT current_setting('hba_file')")
	pg.Exec(t, "sh", "-c", "echo 'host replication all all scram-sha-256' >> "+hba)
	exec("SELECT pg_reload_conf()")
	pw := make([]byte, 8)
	_, _ = rand.Read(pw)
	t.Setenv(envReplPW, "repl-"+hex.EncodeToString(pw))
	exec("CREATE ROLE pentym_repl LOGIN REPLICATION PASSWORD '" + os.Getenv(envReplPW) + "'")
	exec("CREATE TABLE events (id bigserial PRIMARY KEY, note text, at timestamptz DEFAULT now())")
	cfg, err := pgconn.ParseConfig(pg.ConnString)
	if err != nil {
		t.Fatal(err)
	}
	return pg.ConnString, cfg.Host, int(cfg.Port)
}

// writer writes to the server until stopped, switching WAL files now and
// then, and returns the LSN after its last commit.
func writer(ctx context.Context, dsn string, done chan<- string) {
	conn, err := pgconn.Connect(context.Background(), dsn)
	if err != nil {
		done <- ""
		return
	}
	defer func() { _ = conn.Close(context.Background()) }()
	for i := 0; ctx.Err() == nil; i++ {
		_, _ = conn.Exec(context.Background(), "INSERT INTO events (note) SELECT md5(g::text) FROM generate_series(1, 2000) AS g").ReadAll()
		if i%15 == 14 {
			_, _ = conn.Exec(context.Background(), "SELECT pg_switch_wal()").ReadAll()
		}
		time.Sleep(20 * time.Millisecond)
	}
	res, _ := conn.Exec(context.Background(), "SELECT pg_current_wal_flush_lsn()::text").ReadAll()
	if len(res) == 1 && len(res[0].Rows) == 1 {
		done <- string(res[0].Rows[0][0])
		return
	}
	done <- ""
}

// coverage checks the stored WAL covers every byte from the first stored
// segment's start to end, on timeline 1.
func coverage(t *testing.T, r *repo.Repository, tid string, end uint64) {
	t.Helper()
	segs, err := r.WALSegments(context.Background(), tid)
	if err != nil {
		t.Fatal(err)
	}
	const segSize = 16 << 20
	type span struct{ from, to uint64 }
	var spans []span
	for _, s := range segs {
		logID, _ := strconv.ParseUint(s.Name[8:16], 16, 64)
		segID, _ := strconv.ParseUint(s.Name[16:24], 16, 64)
		start := (logID*0x100 + segID) * segSize
		to := start + segSize
		if s.Partial {
			to = uint64(s.EndLSN)
		}
		spans = append(spans, span{start, to})
	}
	if len(spans) == 0 {
		t.Fatal("no WAL stored")
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].from < spans[j].from })
	reach := spans[0].from
	for _, s := range spans {
		if s.from > reach {
			t.Fatalf("a hole in the stored WAL from %X to %X", reach, s.from)
		}
		reach = max(reach, s.to)
	}
	if reach < end {
		t.Errorf("the stored WAL reaches %X, short of %X", reach, end)
	}
	t.Logf("%d stored pieces cover %X to %X", len(spans), spans[0].from, reach)
}

func parseLSNText(t *testing.T, s string) uint64 {
	t.Helper()
	l, err := parseLSN(s)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(l)
}

// TestCaptureSurvivesKills (Build Order C18): an agent capturing a busy
// server is killed at random points, then stopped gracefully; the stored
// WAL has no hole and no gap was needed.
func TestCaptureSurvivesKills(t *testing.T) {
	for _, version := range dbtest.PostgresVersions() {
		t.Run(version, func(t *testing.T) {
			dsn, host, port := pgServer(t, version)
			dir := newRepoDir(t)
			t.Setenv(envPGHost, host)
			t.Setenv(envPGPort, strconv.Itoa(port))
			seed := uint64(time.Now().UnixNano())
			t.Logf("kill schedule seed %d", seed)
			rng := mrand.New(mrand.NewPCG(seed, seed+1)) // #nosec G404 -- a test schedule

			wctx, stopWriting := context.WithCancel(context.Background())
			lastLSN := make(chan string, 1)
			go writer(wctx, dsn, lastLSN)
			t.Setenv("PENTYM_AGENT_CAPTURE_CHILD", dir)
			for round := range 5 {
				c := startChild(t, dir, fmt.Sprintf("k%d", round))
				waitFor(t, "the lease", 15*time.Second, func() bool { return c.saw("held") })
				time.Sleep(time.Duration(1000+rng.IntN(8000)) * time.Millisecond)
				_ = c.cmd.Process.Kill()
				<-c.exit //nolint:errcheck // killed
			}
			stopWriting()
			end := <-lastLSN
			if end == "" {
				t.Fatal("the writer could not report its last position")
			}
			// A last agent catches up, then stops gracefully, storing the
			// segment in progress.
			c := startChild(t, dir, "final")
			waitFor(t, "the lease", 15*time.Second, func() bool { return c.saw("held") })
			time.Sleep(postgres.PartialInterval + 3*time.Second)
			_ = c.stdin.Close()
			if err := <-c.exit; err != nil {
				t.Fatalf("the final agent: %v: %s", err, c.output())
			}
			r := openAs(t, dir, nil, "admin")
			tid, _ := r.TargetID("prod-pg")
			gaps, err := r.Gaps(context.Background(), tid)
			if err != nil || len(gaps) != 0 {
				t.Errorf("gaps after kills = %+v, %v", gaps, err)
			}
			cp, err := r.LatestCheckpoint(context.Background(), tid)
			if err != nil || cp == nil {
				t.Fatalf("checkpoint: %+v, %v", cp, err)
			}
			_, cpLSN, _ := strings.Cut(cp.Position, "/")
			if parseLSNText(t, cpLSN) < parseLSNText(t, end) {
				t.Errorf("the checkpoint %s is short of the last commit %s", cp.Position, end)
			}
			coverage(t, r, tid, parseLSNText(t, end))
		})
	}
}

// TestSlotInvalidationThroughAgent: a slot invalidated while the agent was
// stopped is recorded as a gap; the agent forces a base backup and
// captures again.
func TestSlotInvalidationThroughAgent(t *testing.T) {
	version := dbtest.PostgresVersions()[0]
	dsn, host, port := pgServer(t, version)
	dir := newRepoDir(t)
	cfg := &config.Config{Targets: map[string]config.Target{"prod-pg": pgCaptureTarget(host, port)}}
	eng := pgEngine(t.TempDir())
	opts := func(b *backups) Options {
		return Options{Config: cfg, Repo: openAs(t, dir, nil, "agent"), Host: "a", Timings: fastTimings, Heartbeat: 100 * time.Millisecond,
			Engine: func(Target) (engine.Engine, error) { return eng, nil }, Backup: b.hook}
	}
	exec := func(sql string) string {
		conn, err := pgconn.Connect(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close(context.Background()) }()
		res, err := conn.Exec(context.Background(), sql).ReadAll()
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if len(res) > 0 && len(res[len(res)-1].Rows) > 0 {
			return string(res[len(res)-1].Rows[0][0])
		}
		return ""
	}
	first := start(t, opts(&backups{}))
	exec("INSERT INTO events (note) VALUES ('before')")
	waitFor(t, "a checkpoint", postgres.PartialInterval+15*time.Second, func() bool { return checkpoint(t, dir) != nil })
	first.stop(t)
	before := checkpoint(t, dir)

	exec("ALTER SYSTEM SET max_slot_wal_keep_size = '1MB'")
	exec("SELECT pg_reload_conf()")
	deadline := time.Now().Add(60 * time.Second)
	for exec("SELECT coalesce(wal_status, '') FROM pg_replication_slots WHERE slot_name = 'pentym_prod_pg'") != "lost" {
		if time.Now().After(deadline) {
			t.Fatal("the slot was not invalidated")
		}
		exec("INSERT INTO events (note) SELECT md5(g::text) FROM generate_series(1, 5000) AS g")
		exec("SELECT pg_switch_wal()")
		exec("CHECKPOINT")
	}
	exec("ALTER SYSTEM RESET max_slot_wal_keep_size")
	exec("SELECT pg_reload_conf()")

	b := &backups{}
	start(t, opts(b))
	waitFor(t, "the forced backup", 30*time.Second, func() bool { return b.n.Load() == 1 })
	r := openAs(t, dir, nil, "admin")
	tid, _ := r.TargetID("prod-pg")
	gaps, err := r.Gaps(context.Background(), tid)
	if err != nil || len(gaps) != 1 || gaps[0].From != before.Position || !strings.Contains(gaps[0].Reason, "invalidated") {
		t.Fatalf("gaps = %+v, %v", gaps, err)
	}
	exec("INSERT INTO events (note) VALUES ('after')")
	var seen atomic.Bool
	waitFor(t, "capture after the gap", postgres.PartialInterval+15*time.Second, func() bool {
		cp := checkpoint(t, dir)
		seen.Store(cp != nil && cp.Generation == 2)
		return seen.Load()
	})
}
