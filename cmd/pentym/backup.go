// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/cliout"
	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/instance"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/ops"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// newEngine returns the engine for a configured target, through the
// engine registry. Tests replace it.
func (a *app) newEngine(cfg *config.Config, t config.Target) (engine.Engine, error) {
	if a.engineFor != nil {
		return a.engineFor(cfg, t)
	}
	workDir, err := cfg.WorkDirPath()
	if err != nil {
		return nil, err
	}
	e, err := engine.New(t.Engine, engine.Env{
		Resolver: a.resolver(),
		Runner:   &toolrun.Runner{SecretDir: instance.SecretDir(workDir), Logger: a.tel.Component("toolrun")},
		Tools:    cfg.Tools,
		Logger:   a.tel.Component(t.Engine),
	})
	if err != nil {
		return nil, &msg.Error{Code: msg.ExitNotImplemented, Err: err, Message: msg.Message{
			Level: msg.LevelFailed, Fact: t.Engine + " backups are not implemented in this build", Next: "pentym --help",
		}}
	}
	return e, nil
}

// configTarget returns a configured target, or explains it is missing.
func configTarget(cfg *config.Config, name string) (config.Target, error) {
	t, ok := cfg.Targets[name]
	if !ok {
		return t, &msg.Error{Code: msg.ExitNotFound, Message: msg.Message{
			Level: msg.LevelFailed, Fact: "no target " + name + " in the config", Next: "add it under targets: in the config",
		}}
	}
	return t, nil
}

func (a *app) backupCommand() *cobra.Command {
	var now bool
	var slot string
	c := &cobra.Command{
		Use:   "backup <target>",
		Short: "Take a snapshot of a target now",
		Long: "Take a base backup of a target and commit it as a snapshot. The backup streams straight into the repository: " +
			"nothing is written to temporary files, and memory stays constant whatever the database's size. " +
			"A snapshot is committed only if the backup finishes cleanly.\n\n" +
			"It unlocks the repository with any key slot, an agent slot included, and holds a shared lock while it runs.",
		Example: example(
			"Take a snapshot of prod-pg", "pentym backup prod-pg",
			"Take it through the running agent", "pentym backup prod-pg --now"),
		Args: args("<target>"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			if now {
				return &msg.Error{Code: msg.ExitNotImplemented, Message: msg.Message{
					Level: msg.LevelFailed, Fact: "--now needs the agent, which is not implemented in this build",
					Next: "pentym backup " + argv[0],
				}}
			}
			return a.runBackup(cmd.Context(), argv[0], slot)
		},
	}
	c.Flags().BoolVar(&now, "now", false, "run the backup through the running agent")
	addSlotFlag(c, &slot)
	return c
}

func (a *app) runBackup(ctx context.Context, name, slot string) error {
	cfg, r, err := a.openRepo(ctx)
	if err != nil {
		return err
	}
	t, err := configTarget(cfg, name)
	if err != nil {
		return err
	}
	eng, err := a.newEngine(cfg, t)
	if err != nil {
		return err
	}
	if _, err := a.unlock(ctx, cfg, r, repo.UnlockOptions{SlotID: slot}); err != nil {
		return err
	}
	progress, err := a.progress("backup "+name, 0)
	if err != nil {
		return err
	}
	progress.Start()
	b := &ops.Backup{
		Repo: r, Engine: eng, Target: engine.Target{Name: name, Config: t},
		Identity: repo.CurrentIdentity(), Credential: t.Credentials.Backup.Password,
		Telemetry: a.tel, OnBytes: progress.Add,
	}
	res, err := b.Run(ctx)
	progress.Done()
	for _, f := range res.Warnings {
		a.printer.Message(msg.Message{Level: msg.LevelWarning, ID: f.ID, Target: name, Fact: f.Fact, Next: f.Next})
	}
	if err != nil {
		return err
	}
	s, st := res.Snapshot, res.Stats
	if a.opts.json {
		return a.printer.WriteJSON(struct {
			Snapshot snapshotInfo `json:"snapshot"`
			Stats    backupStats  `json:"stats"`
		}{a.snapshotInfo(s, name, nil), backupStats{
			Bytes: st.Bytes, NewBytes: st.NewBytes, Chunks: st.Chunks, NewChunks: st.NewChunks,
			Packs: st.Packs, DedupRatio: st.DedupRatio(), DurationSeconds: res.Duration.Seconds(), RunID: res.RunID,
		}})
	}
	_, _ = fmt.Fprintln(a.printer.Stdout, s.ID)
	a.printer.Message(msg.Message{Level: msg.LevelOK, Target: name, Fact: fmt.Sprintf(
		"snapshot %s: %s in %s, %s new after deduplication",
		s.ID, cliout.Bytes(s.TotalSize()), cliout.Duration(res.Duration), cliout.Bytes(st.NewBytes))})
	return nil
}

// backupStats is what a backup wrote, in --json.
type backupStats struct {
	Bytes           int64   `json:"bytes"`
	NewBytes        int64   `json:"new_bytes"`
	Chunks          int64   `json:"chunks"`
	NewChunks       int64   `json:"new_chunks"`
	Packs           int     `json:"packs"`
	DedupRatio      float64 `json:"dedup_ratio"`
	DurationSeconds float64 `json:"duration_seconds"`
	RunID           string  `json:"run_id"`
}

// snapshotInfo is a snapshot as list and backup print it.
type snapshotInfo struct {
	ID            string    `json:"id"`
	Target        string    `json:"target"`
	Engine        string    `json:"engine"`
	EngineVersion string    `json:"engine_version,omitempty"`
	Start         time.Time `json:"start"`
	End           time.Time `json:"end"`
	Size          int64     `json:"size"`
	Parts         []string  `json:"parts"`
	StartPosition string    `json:"start_position,omitempty"`
	EndPosition   string    `json:"end_position,omitempty"`
	Timeline      string    `json:"timeline,omitempty"`
	Method        string    `json:"method,omitempty"`
	Pinned        bool      `json:"pinned"`
	PinReason     string    `json:"pin_reason,omitempty"`
}

func (a *app) snapshotInfo(s *repo.Snapshot, target string, pin *repo.Pin) snapshotInfo {
	parts := []string{}
	if s.Name != "" {
		parts = append(parts, s.Name)
	}
	for _, p := range s.Parts {
		parts = append(parts, p.Name)
	}
	info := snapshotInfo{
		ID: s.ID, Target: target, Engine: s.Engine, EngineVersion: s.EngineVersion,
		Start: s.Start.UTC(), End: s.End.UTC(), Size: s.TotalSize(), Parts: parts,
		StartPosition: s.StartPosition, EndPosition: s.EndPosition, Timeline: s.Timeline, Method: s.Method,
	}
	if pin != nil && pin.Pinned {
		info.Pinned, info.PinReason = true, pin.Reason
	}
	return info
}

func (a *app) listCommand() *cobra.Command {
	var slot string
	c := &cobra.Command{
		Use:   "list",
		Short: "List snapshots and log coverage",
		Long: "List the repository's snapshots, oldest first, with their target, engine version, start time, duration and size, " +
			"and whether they are pinned. It unlocks the repository with any key slot.",
		Example: example(
			"List snapshots", "pentym list",
			"List them as JSON", "pentym list --json"),
		Args: args(),
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runList(cmd.Context(), slot) },
	}
	addSlotFlag(c, &slot)
	return c
}

func (a *app) runList(ctx context.Context, slot string) error {
	cfg, r, err := a.openRepo(ctx)
	if err != nil {
		return err
	}
	if _, err := a.unlock(ctx, cfg, r, repo.UnlockOptions{SlotID: slot}); err != nil {
		return err
	}
	snaps, err := r.Snapshots(ctx)
	if err != nil {
		return err
	}
	targets, err := r.Targets(ctx)
	if err != nil {
		return err
	}
	pins, err := r.Pins(ctx)
	if err != nil {
		return err
	}
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].Start.Before(snaps[j].Start) })
	infos := make([]snapshotInfo, 0, len(snaps))
	for i := range snaps {
		name := snaps[i].Target
		if t, ok := targets[name]; ok {
			name = t.Name
		}
		var pin *repo.Pin
		if p, ok := pins[snaps[i].ID]; ok {
			pin = &p
		}
		infos = append(infos, a.snapshotInfo(&snaps[i], name, pin))
	}
	if a.opts.json {
		return a.printer.WriteJSON(struct {
			Snapshots []snapshotInfo `json:"snapshots"`
		}{infos})
	}
	if len(infos) == 0 {
		a.printer.Message(msg.Message{Level: msg.LevelOK, Fact: "the repository has no snapshots yet", Next: "pentym backup <target>"})
		return nil
	}
	t := cliout.Table{Columns: []cliout.Column{
		{Header: "SNAPSHOT", Priority: 5}, {Header: "TARGET", Priority: 5}, {Header: "ENGINE", Priority: 2},
		{Header: "TAKEN", Priority: 4}, {Header: "DURATION", Priority: 1}, {Header: "SIZE", Priority: 3}, {Header: "PINNED", Priority: 3},
	}}
	for _, s := range infos {
		eng := s.Engine
		if s.EngineVersion != "" {
			eng += " " + s.EngineVersion
		}
		pinned := ""
		if s.Pinned {
			pinned = "pinned"
			if s.PinReason != "" {
				pinned += " (" + s.PinReason + ")"
			}
		}
		t.Rows = append(t.Rows, []cliout.Cell{
			{Text: s.ID, Style: cliout.Dim}, {Text: s.Target, Style: cliout.Bold}, {Text: eng},
			{Text: a.printer.Time(s.Start)}, {Text: cliout.Duration(s.End.Sub(s.Start))},
			{Text: cliout.Bytes(s.Size)}, {Text: pinned},
		})
	}
	return a.printer.Table(t)
}

// engineFactory builds an engine for a target; tests set app.engineFor.
type engineFactory func(*config.Config, config.Target) (engine.Engine, error)
