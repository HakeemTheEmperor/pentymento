// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/ops"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// restoreFlags are pentym restore's options.
type restoreFlags struct {
	at, xid, lsn, beforeEvent string
	tables                    []string
	inPlace                   bool
	mode, dataDir, slot       string
}

func (a *app) restoreCommand() *cobra.Command {
	var f restoreFlags
	c := &cobra.Command{
		Use:   "restore <target>",
		Short: "Restore a target to a point in time",
		Long: "Restore a target to just before a point in time, and load the tables you lost back into production.\n\n" +
			"pentym restores the snapshot taken before the point into a temporary side instance, replays the captured WAL " +
			"up to just before the point, and copies only the tables named with --table into production: under a safe name, " +
			"such as orders_recovered_20261122, so you can compare and copy rows back, or under their own name with --in-place " +
			"when they no longer exist. Everything else in production is left as it is. The side instance is removed afterwards.\n\n" +
			"--mode=rewind instead restores the whole cluster to the point into an empty directory, ready to replace " +
			"production's data directory; pentym never stops or overwrites production itself.\n\n" +
			"It reads only the repository, never needs the agent, and unlocks the repository with a full key slot. " +
			"Tables are loaded with the target's restore role (credentials.restore).",
		Example: example(
			"Restore the orders table to just before transaction 8812", "pentym restore prod-pg --xid 8812 --table orders",
			"Restore two tables to a time, in place", `pentym restore prod-pg --at "2026-11-22 14:03" --table orders --table billing.invoices --in-place`,
			"Restore to just before a WAL position", "pentym restore prod-pg --lsn 0/3A00F128 --table orders",
			"Rewind the whole cluster into a new directory (destructive once swapped in)", "pentym restore prod-pg --at 2026-11-22T14:03:00+01:00 --mode=rewind --data-dir /srv/pg-rewound"),
		Args: args("<target>"),
		RunE: func(cmd *cobra.Command, argv []string) error { return a.runRestore(cmd, argv[0], f) },
	}
	fl := c.Flags()
	fl.StringVar(&f.at, "at", "", "restore to just before this time: RFC 3339, or 2026-11-22 14:03[:12] in local time")
	fl.StringVar(&f.xid, "xid", "", "restore to just before this transaction (Postgres)")
	fl.StringVar(&f.lsn, "lsn", "", "restore to just before this WAL position, such as 0/3A00F128 (Postgres)")
	fl.StringVar(&f.beforeEvent, "before-event", "", "restore to just before this event (see pentym events)")
	fl.StringArrayVar(&f.tables, "table", nil, "a table to load into production, such as orders or billing.invoices; repeat for more")
	fl.BoolVar(&f.inPlace, "in-place", false, "load the tables under their own names; they must no longer exist")
	fl.StringVar(&f.mode, "mode", "", "rewind: restore the whole cluster into --data-dir instead of loading tables (destructive once swapped in)")
	fl.StringVar(&f.dataDir, "data-dir", "", "with --mode=rewind: an empty directory to write the rewound cluster into")
	addSlotFlag(c, &f.slot)
	return c
}

// parseAt reads --at: RFC 3339, or a date and time in loc.
func parseAt(s string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--at %q is not a time; write it as 2026-11-22T14:03:00+01:00, or 2026-11-22 14:03 in local time", s)
}

// point reads the restore point from the flags.
func (a *app) point(cmd *cobra.Command, f restoreFlags) (engine.Point, error) {
	n := 0
	for _, v := range []string{f.at, f.xid, f.lsn, f.beforeEvent} {
		if v != "" {
			n++
		}
	}
	if n != 1 {
		return engine.Point{}, newUsageError(cmd, "pentym restore needs one restore point: --at, --xid or --lsn")
	}
	switch {
	case f.beforeEvent != "":
		return engine.Point{}, &msg.Error{Code: msg.ExitNotImplemented, Message: msg.Message{
			Level: msg.LevelFailed, Fact: "restoring to just before an event is not implemented in this build",
			Next: "restore to the event's time with --at, or its transaction with --xid"}}
	case f.at != "":
		t, err := parseAt(f.at, a.location())
		if err != nil {
			return engine.Point{}, newUsageError(cmd, err.Error())
		}
		return engine.Point{Time: t}, nil
	case f.xid != "":
		if _, err := strconv.ParseUint(f.xid, 10, 64); err != nil {
			return engine.Point{}, newUsageError(cmd, fmt.Sprintf("--xid %q is not a transaction ID", f.xid))
		}
		return engine.Point{XID: f.xid}, nil
	}
	return engine.Point{Before: engine.Position(f.lsn)}, nil
}

// restoreReport is what a restore reports in --json.
type restoreReport struct {
	Target          string              `json:"target"`
	Snapshot        string              `json:"snapshot"`
	ReachedPosition string              `json:"reached_position,omitempty"`
	ReachedTime     time.Time           `json:"reached_time,omitzero"`
	Tables          []restoredTable     `json:"tables,omitempty"`
	SkippedFKs      []skippedForeignKey `json:"skipped_foreign_keys,omitempty"`
	Rewound         *rewoundCluster     `json:"rewound,omitempty"`
	DurationSeconds float64             `json:"duration_seconds"`
	RunID           string              `json:"run_id"`
}

type restoredTable struct {
	Source string `json:"source"`
	Name   string `json:"name"`
	Rows   int64  `json:"rows"`
}

type skippedForeignKey struct {
	Table      string `json:"table"`
	Name       string `json:"name"`
	References string `json:"references"`
	Statement  string `json:"statement"`
}

type rewoundCluster struct {
	DataDir string `json:"data_dir"`
}

func (a *app) runRestore(cmd *cobra.Command, name string, f restoreFlags) error {
	ctx := cmd.Context()
	p, err := a.point(cmd, f)
	if err != nil {
		return err
	}
	rewind := false
	switch f.mode {
	case "":
	case "rewind":
		rewind = true
	default:
		return newUsageError(cmd, fmt.Sprintf("--mode %q is not a mode; the only one is rewind", f.mode))
	}
	switch {
	case rewind && f.dataDir == "":
		return newUsageError(cmd, "--mode=rewind needs --data-dir, an empty directory to write the rewound cluster into")
	case rewind && (len(f.tables) > 0 || f.inPlace):
		return newUsageError(cmd, "--mode=rewind restores the whole cluster; it takes no --table or --in-place")
	case !rewind && f.dataDir != "":
		return newUsageError(cmd, "--data-dir is for --mode=rewind")
	case !rewind && len(f.tables) == 0:
		return newUsageError(cmd, "name the tables to load into production with --table, such as --table orders")
	}
	cfg, r, err := a.openRepo(ctx)
	if err != nil {
		return err
	}
	t, err := configTarget(cfg, name)
	if err != nil {
		return err
	}
	if t.Engine != "postgres" {
		return &msg.Error{Code: msg.ExitNotImplemented, Message: msg.Message{Level: msg.LevelFailed, Target: name,
			Fact: "point-in-time restores of " + t.Engine + " are not implemented in this build", Next: "pentym --help"}}
	}
	eng, err := a.newEngine(cfg, t)
	if err != nil {
		return err
	}
	if rewind && a.printer.Interactive {
		a.printer.Message(msg.Message{Level: msg.LevelWarning, Target: name,
			Fact:   "this restores the whole cluster to " + describeFlagPoint(a, p) + " into " + f.dataDir,
			Impact: "once it replaces production's data directory, every change after that point is gone"})
		ok, err := a.printer.ConfirmTyped(name)
		if err != nil {
			return err
		}
		if !ok {
			return &msg.Error{Code: msg.ExitDeclined, Message: msg.Message{Level: msg.LevelFailed, Target: name,
				Fact: "rewind declined; nothing was written", Next: "pentym restore " + name + " --table <table>, to load only what was lost"}}
		}
	}
	if _, err := a.unlock(ctx, cfg, r, repo.UnlockOptions{SlotID: f.slot, NeedFull: true}); err != nil {
		return err
	}
	work, err := cfg.WorkDirPath()
	if err != nil {
		return err
	}
	set := instance.NewSet(instance.NewEnv(work, cfg.Tools, &toolrun.Runner{Logger: a.tel.Component("toolrun")}), cfg.Providers())
	progress, err := a.progress("restore "+name, 0)
	if err != nil {
		return err
	}
	progress.Start()
	o := &ops.Restore{
		Repo: r, Engine: eng, Target: engine.Target{Name: name, Config: t},
		Point: p, When: a.printer.Time, Instances: set, WorkDir: work,
		Identity: repo.CurrentIdentity(), Telemetry: a.tel, OnBytes: progress.Add,
	}
	var cred secrets.Ref
	if rewind {
		o.RewindDir = f.dataDir
	} else {
		cred = t.Credentials.Restore.Password
		loc := a.location()
		o.Extract = &engine.ExtractRequest{Objects: f.tables, InPlace: f.inPlace}
		o.Suffix = func(at time.Time) string { return "_recovered_" + at.In(loc).Format("20060102") }
	}
	o.Credential = cred
	res, err := o.Run(ctx)
	progress.Done()
	if err != nil {
		return err
	}
	rep := restoreReport{Target: name, Snapshot: res.Snapshot.ID, DurationSeconds: res.Duration.Seconds(), RunID: res.RunID}
	if res.Reached != nil {
		rep.ReachedPosition, rep.ReachedTime = string(res.Reached.Position), res.Reached.Time.UTC()
	}
	if ex := res.Extracted; ex != nil {
		for _, o := range ex.Objects {
			rep.Tables = append(rep.Tables, restoredTable{Source: o.Source, Name: o.Name, Rows: o.Rows})
		}
		for _, s := range ex.SkippedForeignKeys {
			rep.SkippedFKs = append(rep.SkippedFKs, skippedForeignKey{Table: s.Table, Name: s.Name, References: s.References, Statement: s.Statement})
		}
	}
	if rewind {
		rep.Rewound = &rewoundCluster{DataDir: f.dataDir}
	}
	if a.opts.json {
		return a.printer.WriteJSON(rep)
	}
	a.printRestore(rep, p)
	return nil
}

func describeFlagPoint(a *app, p engine.Point) string {
	switch {
	case !p.Time.IsZero():
		return "just before " + a.printer.Time(p.Time)
	case p.XID != "":
		return "just before transaction " + p.XID
	}
	return "just before WAL position " + string(p.Before)
}

func (a *app) printRestore(rep restoreReport, p engine.Point) {
	as := describeFlagPoint(a, p)
	if !rep.ReachedTime.IsZero() {
		as += " (last change replayed: " + a.printer.Time(rep.ReachedTime) + ")"
	}
	for _, t := range rep.Tables {
		what := "restored " + t.Source
		if t.Name != t.Source {
			what += " as " + t.Name
		}
		a.printer.Message(msg.Message{Level: msg.LevelOK, Target: rep.Target,
			Fact: fmt.Sprintf("%s: %d %s, as %s", what, t.Rows, pluralRows(t.Rows), as)})
	}
	for _, s := range rep.SkippedFKs {
		a.printer.Message(msg.Message{Level: msg.LevelWarning, Target: rep.Target,
			Fact:   "left out foreign key " + s.Name + " of " + s.Table + ", which refers to " + s.References + ", a table that was not restored",
			Impact: "the restored rows are not checked against " + s.References,
			Next:   s.Statement})
	}
	if rep.Rewound != nil {
		a.printer.Message(msg.Message{Level: msg.LevelOK, Target: rep.Target,
			Fact: "rewound the whole cluster to " + as + " into " + rep.Rewound.DataDir})
		a.printer.Message(msg.Message{Level: msg.LevelWarning, Target: rep.Target,
			Fact: "production is unchanged; to replace it with the rewound cluster: stop its server, move its data directory aside, " +
				"put " + rep.Rewound.DataDir + " in its place (owned by the server's user, mode 0700), and start the server",
			Impact: "changes made after the point are lost; standbys must be rebuilt from the rewound server",
			Next:   "pentym backup " + rep.Target + ", once it runs, so recovery starts again from the rewound cluster"})
	}
	// stdout: what was written, one per line.
	for _, t := range rep.Tables {
		_, _ = fmt.Fprintln(a.printer.Stdout, t.Name)
	}
	if rep.Rewound != nil {
		_, _ = fmt.Fprintln(a.printer.Stdout, rep.Rewound.DataDir)
	}
}

// location is the time zone human times are read and written in.
func (a *app) location() *time.Location {
	if a.printer.Location != nil {
		return a.printer.Location
	}
	return time.Local
}

func pluralRows(n int64) string {
	if n == 1 {
		return "row"
	}
	return "rows"
}
