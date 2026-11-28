// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
)

// rootCommand builds the command tree (PRD 7.15). Commands that are not
// built yet return "not implemented" (exit code 10).
func (a *app) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "pentym",
		Short: "Point-in-time recovery and undo-DROP for self-hosted Postgres and MongoDB",
		Long: "Pentymento: point-in-time recovery and undo-DROP for self-hosted Postgres and MongoDB, in a single binary.\n\n" +
			"Results and --json output go to stdout; progress, logs and messages go to stderr.",
		Example: example(
			"Show the recovery window of every target", "pentym status",
			"List destructive events on prod-pg in the last 2 hours", "pentym events prod-pg --since 2h",
			"Restore prod-pg to just before event 3", "pentym restore prod-pg --before-event 3",
		),
		Args:              cobra.NoArgs,
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error { return a.setup(cmd) },
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.opts.version {
				return a.printVersion()
			}
			return cmd.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetIn(a.stdin)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error { return newUsageError(c, err.Error()) })

	pf := root.PersistentFlags()
	pf.BoolVar(&a.opts.json, "json", false, "print results as JSON on stdout")
	pf.StringVar(&a.opts.color, "color", "auto", "colour output: auto, always or never")
	pf.BoolVar(&a.opts.ascii, "ascii", false, "use ASCII symbols instead of Unicode")
	pf.BoolVarP(&a.opts.quiet, "quiet", "q", false, "print only results and errors")
	pf.BoolVarP(&a.opts.verbose, "verbose", "v", false, "print more detail, such as message IDs")
	pf.BoolVar(&a.opts.debug, "debug", false, "print the structured debug log to stderr")
	pf.StringVar(&a.opts.logFormat, "log-format", "", "log format: text or json (default: text in a terminal, json otherwise)")
	pf.StringVar(&a.opts.config, "config", "", "config file (default: $PENTYM_CONFIG, else "+config.DefaultPath(runtime.GOOS, a.getenv)+")")
	root.Flags().BoolVar(&a.opts.version, "version", false, "print the version")

	root.AddCommand(
		a.initCommand(),
		a.backupCommand(),
		a.agentCommand(),
		a.catchUpCommand(),
		restoreCommand(),
		eventsCommand(),
		a.listCommand(),
		a.statusCommand(),
		verifyCommand(),
		a.checkCommand(),
		pruneCommand(),
		pinCommand(),
		a.dumpCommand(),
		historyCommand(),
		a.keyCommand(),
		a.repoCommand(),
		a.doctorCommand(),
		notifyCommand(),
		debugCommand(),
	)
	return root
}

// example formats pairs of (description, command) as a help example block.
// Commands are copyable as is: no prompt character.
func example(pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "  # %s\n  %s", pairs[i], pairs[i+1])
	}
	return b.String()
}

// stub returns a command that is not built yet.
func stub(use, short string, posArgs cobra.PositionalArgs, ex ...string) *cobra.Command {
	return &cobra.Command{
		Use:     use,
		Short:   short,
		Example: example(ex...),
		Args:    posArgs,
		RunE:    notImplemented,
	}
}

// group returns a parent command that only holds subcommands.
func group(use, short string, ex ...string) *cobra.Command {
	return &cobra.Command{
		Use:     use,
		Short:   short,
		Example: example(ex...),
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, a []string) error {
			if len(a) > 0 {
				return newUsageError(cmd, fmt.Sprintf("unknown command %q for %q", a[0], cmd.CommandPath()))
			}
			var names []string
			for _, c := range cmd.Commands() {
				if c.IsAvailableCommand() {
					names = append(names, c.Name())
				}
			}
			return newUsageError(cmd, fmt.Sprintf("%s needs a subcommand: %s", cmd.CommandPath(), strings.Join(names, ", ")))
		},
	}
}

func restoreCommand() *cobra.Command {
	c := stub("restore <target>", "Restore a target to a point in time", args("<target>"),
		"Restore prod-pg to just before event 3", "pentym restore prod-pg --before-event 3",
		"Restore prod-pg to just before transaction 8812", "pentym restore prod-pg --xid 8812",
		"Rewind the whole prod-pg cluster to just before event 3 (destructive)", "pentym restore prod-pg --before-event 3 --mode=rewind")
	f := c.Flags()
	f.String("at", "", "restore to this point in time")
	f.String("xid", "", "restore to just before this transaction (Postgres)")
	f.String("before-event", "", "restore to just before this event (see pentym events)")
	f.Bool("in-place", false, "load recovered objects under their original names instead of a safe name")
	f.String("mode", "", "rewind: restore the whole cluster to the point instead of extracting objects (destructive)")
	return c
}

func eventsCommand() *cobra.Command {
	c := stub("events <target>", "List detected destructive events", args("<target>"),
		"List events on prod-pg in the last 2 hours", "pentym events prod-pg --since 2h")
	c.Flags().String("since", "", "show events from this long ago, such as 2h")
	return c
}

func verifyCommand() *cobra.Command {
	c := stub("verify <target>", "Prove that a target's backups restore", args("<target>"),
		"Run the standard verification on prod-mg", "pentym verify prod-mg --level standard")
	c.Flags().String("level", "", "verification level: quick, standard or deep")
	return c
}

func pruneCommand() *cobra.Command {
	return stub("prune", "Apply the retention policy", args(),
		"Show what would be deleted, then confirm", "pentym prune")
}

func pinCommand() *cobra.Command {
	c := stub("pin <snapshot>", "Keep a snapshot until it is unpinned", args("<snapshot>"),
		"Pin a snapshot before a migration", `pentym pin <snapshot> --reason "pre-migration"`)
	c.Flags().String("reason", "", "why the snapshot is pinned")
	return c
}

func historyCommand() *cobra.Command {
	c := stub("history <target>", "Show the operations journal", args("<target>"),
		"Show the last 30 days of operations on prod-mg", "pentym history prod-mg --since 30d")
	c.Flags().String("since", "", "show operations from this long ago, such as 30d")
	return c
}

func notifyCommand() *cobra.Command {
	c := group("notify", "Work with notifications",
		"Send a sample critical alert to a channel", "pentym notify test --channel <name> --severity critical")
	test := stub("test", "Send a sample notification", args(),
		"Send a sample critical alert to a channel", "pentym notify test --channel <name> --severity critical")
	test.Flags().String("channel", "", "the configured channel to send to")
	test.Flags().String("severity", "", "severity of the sample: critical, warning or info")
	c.AddCommand(test)
	return c
}

func debugCommand() *cobra.Command {
	c := group("debug", "Collect diagnostics",
		"Collect a redacted support archive", "pentym debug bundle")
	c.AddCommand(stub("bundle", "Collect a redacted support archive", args(),
		"Collect a redacted support archive", "pentym debug bundle"))
	return c
}
