// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/agent"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
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
		stub("reload", "Reload the running agent's configuration without dropping connections", args(),
			"Reload the configuration", "pentym agent reload"),
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
	ag, err := agent.New(agent.Options{Config: cfg, Repo: r, Telemetry: tel, Cleanup: set.CleanLeftovers})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopReload := onReloadSignal(func() {
		next, err := a.repoConfig()
		if err != nil {
			logger.Error("the configuration did not reload", "error", err)
			return
		}
		if _, err := ag.Reload(ctx, next); err != nil {
			logger.Error("the configuration did not reload", "error", err)
		}
	})
	defer stopReload()
	return ag.Run(ctx)
}
