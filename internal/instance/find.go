// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package instance

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// serverBinary is the binary whose version decides an engine's binary
// directory.
var serverBinary = map[string]string{Postgres: "postgres", MongoDB: "mongod"}

// Found is an engine's binary directory with its server version.
type Found struct {
	Dir     string
	Version toolrun.Version
}

// candidateDirs lists where to look for an engine's binaries: the
// configured directories, then the directory of the binary on PATH.
func (e *Env) candidateDirs(engine string) []string {
	dirs := e.Tools.Postgres
	if engine == MongoDB {
		dirs = e.Tools.MongoDB
	}
	out := append([]string(nil), dirs...)
	if p, err := e.lookPath(serverBinary[engine]); err == nil {
		out = append(out, filepath.Dir(p))
	}
	return out
}

// FindAll lists every directory holding the engine's server binary, with
// its version, in search order; duplicates are skipped.
func (e *Env) FindAll(ctx context.Context, engine string) ([]Found, error) {
	seen := map[string]bool{}
	var out []Found
	for _, dir := range e.candidateDirs(engine) {
		abs, err := filepath.Abs(dir)
		if err != nil || seen[abs] {
			continue
		}
		seen[abs] = true
		bin := filepath.Join(abs, exe(serverBinary[engine]))
		if _, err := os.Stat(bin); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		v, err := e.Runner.ToolVersion(ctx, serverBinary[engine], bin)
		if err != nil {
			return nil, err
		}
		out = append(out, Found{Dir: abs, Version: v})
	}
	return out, nil
}

// errNoBinaries explains that no directory holds the wanted version.
type errNoBinaries struct {
	engine, want string
	found        []Found
}

func (e *errNoBinaries) Error() string {
	key := "tools." + e.engine
	if len(e.found) == 0 {
		return fmt.Sprintf("no %s binary in %s or on PATH", serverBinary[e.engine], key)
	}
	var vs []string
	for _, f := range e.found {
		vs = append(vs, versionText(e.engine, f.Version))
	}
	sort.Strings(vs)
	return fmt.Sprintf("no %s %s binary in %s or on PATH (found %s)", e.engine, e.want, key, strings.Join(vs, ", "))
}

// find returns the directory holding the wanted version's binaries.
func (e *Env) find(ctx context.Context, engine, want string) (Found, error) {
	all, err := e.FindAll(ctx, engine)
	if err != nil {
		return Found{}, err
	}
	for _, f := range all {
		if versionMatches(engine, want, f.Version) {
			return f, nil
		}
	}
	return Found{}, &errNoBinaries{engine: engine, want: want, found: all}
}
