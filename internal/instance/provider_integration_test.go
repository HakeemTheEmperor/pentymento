// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package instance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/devtools/dbtest"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// Environment for the provider tests.
const (
	// envProviders lists providers that must work (docker,local); a
	// listed provider that is unavailable fails the test instead of
	// skipping it. CI sets it.
	envProviders = "PENTYM_TEST_PROVIDERS"
	// envPGBin and envMongoBin list directories of server binaries,
	// separated like PATH.
	envPGBin    = "PENTYM_TEST_POSTGRES_BIN"
	envMongoBin = "PENTYM_TEST_MONGODB_BIN"
	// envHold makes TestHoldInstance start an instance and wait to be
	// killed (see TestLeftoversAfterCrash).
	envHold = "PENTYM_TEST_HOLD"
)

func integrationEnv(t *testing.T) *Env {
	t.Helper()
	split := func(v string) []string {
		var out []string
		for _, d := range filepath.SplitList(os.Getenv(v)) {
			if d != "" {
				out = append(out, d)
			}
		}
		return out
	}
	tools := config.Tools{Postgres: split(envPGBin), MongoDB: split(envMongoBin)}
	if len(tools.Postgres) == 0 && runtime.GOOS == "linux" {
		tools.Postgres, _ = filepath.Glob("/usr/lib/postgresql/*/bin")
	}
	work := os.Getenv("PENTYM_TEST_WORK_DIR")
	if work == "" {
		work = t.TempDir()
	}
	return NewEnv(work, tools, &toolrun.Runner{})
}

func required(provider string) bool {
	return slices.Contains(strings.Split(os.Getenv(envProviders), ","), provider)
}

// available skips (or, when required, fails) when a provider cannot start
// an engine version here.
func available(t *testing.T, env *Env, provider, engine, version string) {
	t.Helper()
	var err error
	switch provider {
	case "docker":
		_, err = NewDocker(env).ServerVersion(context.Background())
	case "local":
		_, err = env.find(context.Background(), engine, version)
	}
	if err == nil {
		return
	}
	if required(provider) {
		t.Fatalf("%s is required (%s) but unavailable: %v", provider, envProviders, err)
	}
	t.Skipf("%s unavailable: %v", provider, err)
}

func runID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "it" + hex.EncodeToString(b)
}

func versions(engine string) []string {
	if engine == Postgres {
		return dbtest.PostgresVersions()
	}
	return dbtest.MongoDBVersions()
}

// use writes and reads a little data, proving the instance works.
func use(ctx context.Context, t *testing.T, engine string, c Conn) {
	t.Helper()
	switch engine {
	case Postgres:
		cfg, err := pgConfig(c, "postgres")
		if err != nil {
			t.Fatal(err)
		}
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close(context.Background()) }()
		if _, err := conn.Exec(ctx, "CREATE TABLE t AS SELECT generate_series(1, 100) AS i"); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 100 {
			t.Errorf("count = %d", n)
		}
	case MongoDB:
		cl, err := mongoClient(c)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cl.Disconnect(context.Background()) }()
		coll := cl.Database("pentym_test").Collection("t")
		if _, err := coll.InsertOne(ctx, bson.D{{Key: "i", Value: 1}}); err != nil {
			t.Fatal(err)
		}
		if n, err := coll.CountDocuments(ctx, bson.D{}); err != nil || n != 1 {
			t.Errorf("count = %d, %v", n, err)
		}
	}
}

// gone checks that nothing of an instance is left: its container, or its
// server process and instance directory.
func gone(t *testing.T, env *Env, inst Instance) {
	t.Helper()
	switch i := inst.(type) {
	case *dockerInstance:
		out, err := NewDocker(env).docker(context.Background(), "", nil, "ps", "--all", "--quiet", "--filter", "name=^"+i.name+"$")
		if err != nil || out != "" {
			t.Errorf("container %s is left: %q, %v", i.name, out, err)
		}
	case *localInstance:
		select {
		case <-i.proc.Done():
		case <-time.After(10 * time.Second):
			t.Errorf("server %d still running", i.proc.Pid())
		}
		if _, err := os.Stat(i.dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("instance directory %s is left", i.dir)
		}
	}
}

// TestProviders starts each supported version of each engine through each
// provider, uses it, stops it, and checks nothing is left (Build Order
// C12).
func TestProviders(t *testing.T) {
	env := integrationEnv(t)
	for _, provider := range []string{"docker", "local"} {
		for _, engine := range []string{Postgres, MongoDB} {
			for _, version := range versions(engine) {
				t.Run(provider+"/"+engine+"-"+version, func(t *testing.T) {
					available(t, env, provider, engine, version)
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
					defer cancel()
					p := NewSet(env, []string{provider}).Providers()[0]
					data := t.TempDir()
					inst, err := p.Start(ctx, Spec{Engine: engine, Version: version, DataDir: data, RunID: runID()})
					if err != nil {
						t.Fatal(err)
					}
					c := inst.Conn()
					if c.Host != "127.0.0.1" || c.Port == 0 || c.User != superuser || c.Password.IsEmpty() {
						t.Errorf("conn = %+v", c)
					}
					use(ctx, t, engine, c)
					if err := inst.Stop(ctx); err != nil {
						t.Errorf("Stop = %v", err)
					}
					gone(t, env, inst)
					if err := inst.Stop(ctx); err != nil {
						t.Errorf("second Stop = %v", err)
					}
					if entries, _ := os.ReadDir(data); len(entries) == 0 {
						t.Error("the data directory was emptied; it belongs to the caller")
					}
				})
			}
		}
	}
}

// holdInfo is what TestHoldInstance reports about its instance.
type holdInfo struct {
	Container string `json:"container"`
	ServerPID int    `json:"server_pid"`
}

// TestHoldInstance is a helper for TestLeftoversAfterCrash: with envHold
// set it starts an instance, reports it, and waits to be killed.
func TestHoldInstance(t *testing.T) {
	spec := os.Getenv(envHold)
	if spec == "" {
		t.Skip("helper for TestLeftoversAfterCrash")
	}
	var h struct{ Provider, Engine, Version, DataDir, Out string }
	if err := json.Unmarshal([]byte(spec), &h); err != nil {
		t.Fatal(err)
	}
	env := integrationEnv(t)
	inst, err := NewSet(env, []string{h.Provider}).Providers()[0].Start(context.Background(),
		Spec{Engine: h.Engine, Version: h.Version, DataDir: h.DataDir, RunID: runID()})
	if err != nil {
		t.Fatal(err)
	}
	var info holdInfo
	switch i := inst.(type) {
	case *dockerInstance:
		info.Container = i.name
	case *localInstance:
		info.ServerPID = i.proc.Pid()
	}
	b, _ := json.Marshal(info)
	if err := os.WriteFile(h.Out, b, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Hour)
}

// TestLeftoversAfterCrash kills a process that started an instance, then
// checks that the next run's cleanup removes what it left.
func TestLeftoversAfterCrash(t *testing.T) {
	work := t.TempDir()
	t.Setenv("PENTYM_TEST_WORK_DIR", work)
	env := integrationEnv(t)
	engine, version := Postgres, versions(Postgres)[0]
	for _, provider := range []string{"docker", "local"} {
		t.Run(provider, func(t *testing.T) {
			available(t, env, provider, engine, version)
			out := filepath.Join(t.TempDir(), "info.json")
			spec, _ := json.Marshal(map[string]string{"Provider": provider, "Engine": engine, "Version": version, "DataDir": t.TempDir(), "Out": out})
			cmd := exec.Command(os.Args[0], "-test.run=^TestHoldInstance$", "-test.timeout=2h") // #nosec G204 G702 -- the test binary
			cmd.Env = append(os.Environ(), envHold+"="+string(spec))
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var info holdInfo
			deadline := time.Now().Add(10 * time.Minute)
			for {
				b, err := os.ReadFile(out) // #nosec G304 -- test file
				if err == nil && json.Unmarshal(b, &info) == nil {
					break
				}
				if time.Now().After(deadline) {
					_ = cmd.Process.Kill()
					t.Fatal("the helper never started its instance")
				}
				time.Sleep(200 * time.Millisecond)
			}
			// The crash: no cleanup runs in the helper.
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()

			n, err := NewSet(env, []string{provider}).CleanLeftovers(context.Background())
			if err != nil || n == 0 {
				t.Fatalf("CleanLeftovers = %d, %v", n, err)
			}
			if info.Container != "" {
				outp, err := NewDocker(env).docker(context.Background(), "", nil, "ps", "--all", "--quiet", "--filter", "name=^"+info.Container+"$")
				if err != nil || outp != "" {
					t.Errorf("container %s is left: %q, %v", info.Container, outp, err)
				}
			}
			if info.ServerPID != 0 {
				deadline := time.Now().Add(15 * time.Second)
				for toolrun.PIDAlive(info.ServerPID) && time.Now().Before(deadline) {
					time.Sleep(100 * time.Millisecond)
				}
				if toolrun.PIDAlive(info.ServerPID) {
					t.Errorf("server %d is still running", info.ServerPID)
				}
				if entries, _ := os.ReadDir(env.instancesDir()); len(entries) != 0 {
					t.Errorf("instance directories left: %v", entries)
				}
			}
		})
	}
}
