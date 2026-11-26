// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/cliout"
	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/lock"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// doctorCheck is one line of doctor's report.
type doctorCheck struct {
	Check  string `json:"check"`
	Status string `json:"status"` // ok, warning or failed
	Result string `json:"result"`
	Next   string `json:"next,omitempty"`
}

var statusLevel = map[string]msg.Level{"ok": msg.LevelOK, "warning": msg.LevelWarning, "failed": msg.LevelFailed}

// clientTools are the tools each engine's backups and restores use,
// besides the server binary.
var clientTools = map[string][]string{
	instance.Postgres: {"pg_dump", "pg_restore", "pg_basebackup", "initdb", "pg_ctl"},
	instance.MongoDB:  {"mongodump", "mongorestore"},
}

func (a *app) doctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor [target]",
		Short: "Check tools, versions, capabilities, privileges and bucket settings",
		Long: "Check what pentym needs on this machine: database tools and their versions, the instance providers that start temporary " +
			"servers for restores and verification, and the repository storage's capabilities, including a conditional-create test. " +
			"With a target, only that target's engine is checked.\n\n" +
			"It exits with code 1 when a check fails; warnings alone exit 0.",
		Example: example(
			"Check everything", "pentym doctor",
			"Check what prod-pg needs", "pentym doctor prod-pg"),
		Args: optionalArg("[target]"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			target := ""
			if len(argv) == 1 {
				target = argv[0]
			}
			return a.runDoctor(cmd.Context(), target)
		},
	}
}

func (a *app) runDoctor(ctx context.Context, target string) error {
	cfg, err := a.loadConfig(os.Environ())
	if err != nil {
		return err
	}
	engines, err := doctorEngines(cfg, target)
	if err != nil {
		return err
	}
	workDir, err := cfg.WorkDirPath()
	if err != nil {
		return err
	}
	runner := &toolrun.Runner{SecretDir: instance.SecretDir(workDir), Logger: a.tel.Component("toolrun")}
	env := instance.NewEnv(workDir, cfg.Tools, runner)
	required := len(cfg.Targets) > 0

	var checks []doctorCheck
	versions := map[string][]instance.Found{}
	for _, e := range engines {
		c, found := doctorTools(ctx, env, e, required)
		versions[e] = found
		checks = append(checks, c...)
	}
	dockerCheck, dockerOK := doctorDocker(ctx, env)
	checks = append(checks, dockerCheck)
	checks = append(checks, doctorProviders(cfg, engines, versions, dockerOK))
	checks = append(checks, a.doctorRepository(ctx, cfg))

	failed := 0
	for _, c := range checks {
		if c.Status == "failed" {
			failed++
		}
	}
	if a.opts.json {
		if err := a.printer.WriteJSON(struct {
			Checks []doctorCheck `json:"checks"`
		}{checks}); err != nil {
			return err
		}
		if failed > 0 {
			return silentExit(msg.ExitFailure)
		}
		return nil
	}
	t := cliout.Table{Columns: []cliout.Column{{Header: "CHECK", Priority: 2}, {Header: "RESULT", Priority: 2}}}
	for _, c := range checks {
		t.Rows = append(t.Rows, []cliout.Cell{{Text: c.Check}, a.printer.Status(statusLevel[c.Status], c.Result)})
	}
	if err := a.printer.Table(t); err != nil {
		return err
	}
	for _, c := range checks {
		if c.Status != "ok" && c.Next != "" {
			a.printer.Message(msg.Message{Level: statusLevel[c.Status], Fact: c.Check + ": " + c.Result, Next: c.Next})
		}
	}
	if failed > 0 {
		return &msg.Error{Code: msg.ExitFailure, Message: msg.Message{
			Level: msg.LevelFailed, Fact: fmt.Sprintf("%d %s failed", failed, plural(failed, "check", "checks")),
			Next: "fix the failed checks above, then run pentym doctor again",
		}}
	}
	return nil
}

// doctorEngines returns the engines to check: the target's, the configured
// targets', or both when no target is configured.
func doctorEngines(cfg *config.Config, target string) ([]string, error) {
	if target != "" {
		t, ok := cfg.Targets[target]
		if !ok {
			return nil, &msg.Error{Code: msg.ExitNotFound, Message: msg.Message{
				Level: msg.LevelFailed, Fact: "no target " + target + " in the config", Next: "add it under targets: in the config",
			}}
		}
		return []string{t.Engine}, nil
	}
	set := map[string]bool{}
	for _, t := range cfg.Targets {
		set[t.Engine] = true
	}
	if len(set) == 0 {
		return []string{instance.Postgres, instance.MongoDB}, nil
	}
	var out []string
	for e := range set {
		out = append(out, e)
	}
	sort.Strings(out)
	return out, nil
}

// doctorTools reports an engine's server binaries and client tools.
func doctorTools(ctx context.Context, env *instance.Env, engine string, required bool) ([]doctorCheck, []instance.Found) {
	missing := "warning"
	if required {
		missing = "failed"
	}
	found, err := env.FindAll(ctx, engine)
	server := doctorCheck{Check: engine + " server"}
	switch {
	case err != nil:
		server.Status, server.Result = "failed", err.Error()
	case len(found) == 0:
		server.Status, server.Result = "warning", "no binaries in tools."+engine+" or on PATH; the local provider cannot start "+engine
		server.Next = "install the server binaries and list their directory under tools." + engine + ", or use the Docker provider"
	default:
		var parts []string
		for _, f := range found {
			parts = append(parts, fmt.Sprintf("%s in %s", f.Version, f.Dir))
		}
		server.Status, server.Result = "ok", strings.Join(parts, "; ")
	}
	tools := doctorCheck{Check: engine + " tools", Status: "ok"}
	var have, lack []string
	for _, name := range clientTools[engine] {
		path := ""
		for _, f := range found {
			if p := filepath.Join(f.Dir, exeName(name)); fileExists(p) {
				path = p
				break
			}
		}
		if path == "" {
			path = lookTool(name)
		}
		if path == "" {
			lack = append(lack, name)
			continue
		}
		v, err := env.Runner.ToolVersion(ctx, name, path)
		if err != nil {
			have = append(have, name+" (version unknown)")
			continue
		}
		have = append(have, name+" "+v.String())
	}
	tools.Result = strings.Join(have, ", ")
	if len(lack) > 0 {
		tools.Status = missing
		if tools.Result != "" {
			tools.Result += "; "
		}
		tools.Result += "missing: " + strings.Join(lack, ", ")
		tools.Next = "install the " + engine + " client tools of your server's version"
	}
	return []doctorCheck{server, tools}, found
}

// doctorDocker reports whether the Docker daemon is reachable.
func doctorDocker(ctx context.Context, env *instance.Env) (doctorCheck, bool) {
	v, err := instance.NewDocker(env).ServerVersion(ctx)
	if err != nil {
		return doctorCheck{Check: "docker", Status: "warning", Result: err.Error(),
			Next: "start Docker, or rely on the local provider"}, false
	}
	return doctorCheck{Check: "docker", Status: "ok", Result: "daemon " + v}, true
}

// doctorProviders reports the configured provider order and what each can
// start.
func doctorProviders(cfg *config.Config, engines []string, versions map[string][]instance.Found, dockerOK bool) doctorCheck {
	var parts []string
	usable := 0
	for _, p := range cfg.Providers() {
		switch p {
		case config.ProviderDocker:
			if dockerOK {
				parts = append(parts, "docker: any version")
				usable++
			} else {
				parts = append(parts, "docker: unavailable")
			}
		case config.ProviderLocal:
			var vs []string
			for _, e := range engines {
				for _, f := range versions[e] {
					v := fmt.Sprint(f.Version.Major)
					if e == instance.MongoDB {
						v = fmt.Sprintf("%d.%d", f.Version.Major, f.Version.Minor)
					}
					vs = append(vs, e+" "+v)
				}
			}
			if len(vs) == 0 {
				parts = append(parts, "local: no server binaries")
			} else {
				parts = append(parts, "local: "+strings.Join(vs, ", "))
				usable++
			}
		}
	}
	c := doctorCheck{Check: "instance providers", Status: "ok", Result: strings.Join(parts, "; ")}
	if usable == 0 {
		c.Status = "warning"
		c.Next = "start Docker or install server binaries: restores and verification need a temporary server"
	}
	return c
}

// doctorRepository reports the repository storage's capabilities and
// tests conditional create, comparing the result with what init recorded.
func (a *app) doctorRepository(ctx context.Context, cfg *config.Config) doctorCheck {
	c := doctorCheck{Check: "repository"}
	if !cfg.Repository.Configured() {
		c.Status, c.Result, c.Next = "warning", "no repository in the config", "set repository.path or repository.s3, then run pentym init"
		return c
	}
	where := "local storage at " + cfg.Repository.Path
	if s := cfg.Repository.S3; s.IsSet() {
		where = "S3 bucket " + cfg.Repository.Location()
		if s.Endpoint != "" {
			where += " at " + s.Endpoint
		}
	} else if _, err := os.Stat(cfg.Repository.Path); err != nil {
		c.Status, c.Result, c.Next = "warning", cfg.Repository.Path+" does not exist", "pentym init"
		return c
	}
	be, err := a.openBackend(ctx, cfg)
	if err != nil {
		c.Status, c.Result = "failed", err.Error()
		return c
	}
	cc, err := lock.TestConditionalCreate(ctx, be)
	// What init recorded, if there is a repository yet.
	recorded, known := true, false
	if r, err := repo.Open(ctx, be); err == nil {
		recorded, known = r.Config().HasConditionalCreate(), true
	}
	caps := be.Capabilities()
	switch {
	case err != nil:
		c.Status, c.Result = "failed", where+"; conditional-create test: "+err.Error()
		c.Next = "check that pentym can reach and write to " + cfg.Repository.Location()
	case !cc && known && recorded:
		c.Status = "warning"
		c.Result = where + "; conditional create not honoured, but the repository was created on storage that honoured it, so locks and leases do not use their fallback"
		c.Next = "see docs/errors/PTM-3005"
	case !cc:
		c.Status, c.Result, c.Next = "warning", where+"; conditional create not honoured", "see docs/errors/PTM-3005"
	case known && !recorded:
		c.Status = "warning"
		c.Result = where + "; conditional create: yes (tested), but the repository recorded at init that it was not honoured, so locks and leases use their weaker fallback"
		c.Next = "see docs/errors/PTM-3005"
	default:
		c.Status = "ok"
		c.Result = fmt.Sprintf("%s; conditional create: yes (tested); strong consistency: %s; object lock: %s",
			where, yesNo(caps.StrongConsistency), yesNo(caps.ObjectLock))
	}
	return c
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func exeName(name string) string {
	if filepath.Separator == '\\' {
		return name + ".exe"
	}
	return name
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func lookTool(name string) string {
	p, err := lookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// lookPath finds a tool on PATH; tests replace it.
var lookPath = exec.LookPath
