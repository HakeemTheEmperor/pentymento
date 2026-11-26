// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/agent"
	"github.com/HakeemTheEmperor/pentymento/internal/control"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/ops"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/telemetry"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

func (a *app) agentCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "agent",
		Short: "Run continuous capture and the scheduler",
		Long: "Run the agent in the foreground until it is stopped (Ctrl-C or SIGTERM). For every configured target it holds " +
			"the capture lease, so exactly one agent captures each target, and it writes a heartbeat to the repository every 30 s. " +
			"On a graceful stop it finishes within 30 s: it stops capture, writes a final heartbeat and releases its leases, " +
			"so a standby agent takes over at once.\n\n" +
			"It unlocks the repository with repository.key, which should be an agent slot: with agent.write_only on (the " +
			"default) it refuses a slot that can read backups. On Linux and macOS, SIGHUP reloads the configuration without " +
			"disturbing targets whose settings did not change.",
		Example: example(
			"Run the agent in the foreground", "pentym agent",
			"Install and start the agent as a service", "pentym agent install"),
		Args: args(),
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runAgent(cmd.Context()) },
	}
	install := stub("install", "Install the agent as an operating-system service", args(),
		"Install the agent service", "pentym agent install",
		"Print the service files instead of installing", "pentym agent install --print")
	install.Flags().Bool("print", false, "print the service files instead of installing them")
	c.AddCommand(
		install,
		stub("uninstall", "Remove the agent service", args(), "Remove the agent service", "pentym agent uninstall"),
		stub("start", "Start the agent service", args(), "Start the agent service", "pentym agent start"),
		stub("stop", "Stop the agent service", args(), "Stop the agent service", "pentym agent stop"),
		a.agentReloadCommand(),
	)
	return c
}

// runAgent runs the agent until a signal stops it (SDD 9.4, 9.5).
func (a *app) runAgent(ctx context.Context) error {
	cfg, r, err := a.openRepo(ctx)
	if err != nil {
		return err
	}
	if _, err := a.unlock(ctx, cfg, r, repo.UnlockOptions{}); err != nil {
		return err
	}
	level := slog.LevelInfo
	if a.opts.debug {
		level = slog.LevelDebug
	}
	// Services log JSON to stdout; a terminal gets text.
	logger, err := telemetry.NewLogger(a.stdout, a.opts.logFormat, a.printer.OutTerminal, level)
	if err != nil {
		return err
	}
	tel := telemetry.New(logger, telemetry.NewMetrics())
	work, err := cfg.WorkDirPath()
	if err != nil {
		return err
	}
	set := instance.NewSet(instance.NewEnv(work, cfg.Tools, &toolrun.Runner{Logger: tel.Component("toolrun")}), cfg.Providers())
	var ag *agent.Agent
	backupNow := func(ctx context.Context, t agent.Target) (any, error) {
		eng, err := a.newEngine(ag.Config(), t.Config)
		if err != nil {
			return nil, err
		}
		b := &ops.Backup{
			Repo: r, Engine: eng, Target: engine.Target{Name: t.Name, Config: t.Config},
			Identity: repo.CurrentIdentity(), Credential: t.Config.Credentials.Backup.Password, Telemetry: tel,
		}
		res, err := b.Run(ctx)
		if err != nil {
			return nil, err
		}
		return a.backupReport(t.Name, res), nil
	}
	ag, err = agent.New(agent.Options{Config: cfg, Repo: r, Telemetry: tel, Cleanup: set.CleanLeftovers, Backup: backupNow})
	if err != nil {
		return err
	}
	reload := func(ctx context.Context) (*agent.ReloadResult, error) {
		next, err := a.repoConfig()
		if err != nil {
			return nil, err
		}
		return ag.Reload(ctx, next)
	}

	// The control socket, and the TCP health port when configured.
	sock, err := cfg.AgentSocket()
	if err != nil {
		return err
	}
	l, err := control.Listen(sock)
	if err != nil {
		return err
	}
	cs := &control.Server{Agent: ag, Reload: reload, Backup: ag.BackupNow, Metrics: tel.Metrics.Handler(),
		PProf: cfg.Agent.Debug.PProf, Logger: tel.Component("control")}
	servers := []*http.Server{{Handler: cs.Handler(), ReadHeaderTimeout: 10 * time.Second}}
	listeners := []net.Listener{l}
	if addr := cfg.Agent.HealthListen; addr != "" {
		hl, err := net.Listen("tcp", addr)
		if err != nil {
			_ = l.Close()
			return fmt.Errorf("open the health port %s: %w", addr, err)
		}
		servers = append(servers, &http.Server{Handler: control.HealthHandler(ag, tel.Metrics.Handler()), ReadHeaderTimeout: 10 * time.Second})
		listeners = append(listeners, hl)
	}
	for i, s := range servers {
		go func() { _ = s.Serve(listeners[i]) }()
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for _, s := range servers {
			_ = s.Shutdown(sctx)
		}
	}()
	logger.Info("control socket open", "socket", sock, "health_listen", cfg.Agent.HealthListen)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopReload := onReloadSignal(func() {
		if _, err := reload(ctx); err != nil {
			logger.Error("the configuration did not reload", "error", err)
		}
	})
	defer stopReload()
	return ag.Run(ctx)
}

func (a *app) agentReloadCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Reload the running agent's configuration without dropping connections",
		Long: "Ask the running agent, through its control socket, to reload its configuration: new targets start, removed " +
			"ones stop and release their leases, changed ones restart, and targets whose settings did not change keep " +
			"their connections. A reload cannot switch repositories.",
		Example: example("Reload the configuration", "pentym agent reload"),
		Args:    args(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, err := a.repoConfig()
			if err != nil {
				return err
			}
			sock, err := cfg.AgentSocket()
			if err != nil {
				return err
			}
			res, err := control.NewClient(sock).Reload(ctx)
			if errors.Is(err, control.ErrUnreachable) {
				return a.agentUnreachable(ctx, cfg, sock, err, "start the agent: pentym agent")
			}
			if err != nil {
				return err
			}
			if a.opts.json {
				return a.printer.WriteJSON(res)
			}
			a.printer.Message(msg.Message{Level: msg.LevelOK, Fact: fmt.Sprintf(
				"agent reloaded: %d added, %d removed, %d restarted, %d unchanged",
				len(res.Added), len(res.Removed), len(res.Restarted), len(res.Unchanged))})
			return nil
		},
	}
}
