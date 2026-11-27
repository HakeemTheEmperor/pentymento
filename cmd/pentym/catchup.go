// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/agent"
	"github.com/HakeemTheEmperor/pentymento/internal/cliout"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/lock"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

func (a *app) catchUpCommand() *cobra.Command {
	var slot string
	c := &cobra.Command{
		Use:   "catch-up <target>",
		Short: "Upload the WAL or oplog retained while the agent was down",
		Long: "Store the WAL or oplog a target's server still retains while no agent captures it, then exit. " +
			"Postgres keeps WAL for pentym's replication slot, and MongoDB keeps oplog entries until the oplog rolls over, " +
			"so catch-up fills the recovery window back up to where the server stands when it starts.\n\n" +
			"It takes the target's capture lease first, and only once the lease has expired or was released: " +
			"if an agent stopped without releasing it, catch-up waits up to " + cliout.Duration(lock.LeaseLength) + " for it to expire, " +
			"and if an agent keeps renewing it, catch-up refuses, since that agent is capturing. It releases the lease when it exits.\n\n" +
			"If the log since the last checkpoint is gone (the slot was invalidated by max_slot_wal_keep_size, or the oplog rolled over), " +
			"catch-up records the gap, stores what the server still has, and exits with code 8: take a new base backup so recovery " +
			"starts again after the gap. It unlocks the repository with any key slot, an agent slot included.",
		Example: example(
			"Catch up prod-mg after agent downtime", "pentym catch-up prod-mg",
			"Catch up from cron, with the report as JSON", "pentym catch-up prod-pg --json"),
		Args: args("<target>"),
		RunE: func(cmd *cobra.Command, argv []string) error { return a.runCatchUp(cmd.Context(), argv[0], slot) },
	}
	addSlotFlag(c, &slot)
	return c
}

// catchUpReport is what catch-up reports in --json.
type catchUpReport struct {
	Target          string    `json:"target"`
	From            string    `json:"from,omitempty"`
	To              string    `json:"to,omitempty"`
	ServerPosition  string    `json:"server_position,omitempty"`
	Segments        int64     `json:"segments"`
	Bytes           int64     `json:"bytes"`
	Generation      uint64    `json:"lease_generation"`
	LeaseWait       float64   `json:"lease_wait_seconds"`
	DurationSeconds float64   `json:"duration_seconds"`
	Gap             *gapInfo  `json:"gap,omitempty"`
	Finished        time.Time `json:"finished"`
}

// gapInfo is a capture gap in --json.
type gapInfo struct {
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Reason string `json:"reason"`
}

func (a *app) runCatchUp(ctx context.Context, name, slot string) error {
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
	progress, err := a.progress("catch-up "+name, 0)
	if err != nil {
		return err
	}
	var started atomic.Bool
	var last int64
	onProgress := func(phase string, stored int64) {
		switch phase {
		case "waiting":
			a.printer.Message(msg.Message{Level: msg.LevelWarning, Target: name, Fact: fmt.Sprintf(
				"the capture lease was not released; waiting up to %s for it to expire", cliout.Duration(lock.LeaseLength))})
		case "draining":
			if started.CompareAndSwap(false, true) {
				progress.Start()
			}
			progress.Add(stored - last)
			last = stored
		}
	}
	res, err := agent.CatchUp(ctx, agent.Options{
		Config: cfg, Repo: r, Telemetry: a.tel,
		Engine: func(agent.Target) (engine.Engine, error) { return eng, nil },
	}, name, onProgress)
	if started.Load() {
		progress.Done()
	}
	if err != nil {
		return err
	}
	rep := catchUpReport{
		Target: name, From: string(res.From), To: string(res.To), ServerPosition: string(res.Head),
		Segments: res.Segments, Bytes: res.Bytes, Generation: res.Generation,
		LeaseWait: res.LeaseWait.Seconds(), DurationSeconds: res.Duration.Seconds(), Finished: time.Now().UTC(),
	}
	if g := res.Gap; g != nil {
		rep.Gap = &gapInfo{From: string(g.From), To: string(g.To), Reason: g.Reason}
	}
	if a.opts.json {
		if err := a.printer.WriteJSON(rep); err != nil {
			return err
		}
	} else {
		a.printCatchUp(rep)
	}
	if res.Gap != nil {
		return silentExit(msg.ExitUnrecoverable) // reported above
	}
	return nil
}

// gapMessage reports a capture gap (Brand 11).
func gapMessage(target string, g *engine.Gap) msg.Message {
	fact := "a gap in captured log: " + g.Reason
	if g.From != "" {
		fact = "a gap in captured log after " + string(g.From) + ": " + g.Reason
	}
	return msg.Message{
		Level: msg.LevelFailed, ID: agent.IDGap, Target: target, Fact: fact,
		Impact: "point-in-time recovery cannot reach a time inside the gap until a new base backup is taken",
		Next:   "pentym backup " + target,
	}
}

func (a *app) printCatchUp(rep catchUpReport) {
	stored := fmt.Sprintf("%d %s, %s", rep.Segments, plural(int(rep.Segments), "segment", "segments"), cliout.Bytes(rep.Bytes))
	took := cliout.Duration(time.Duration(rep.DurationSeconds * float64(time.Second)))
	switch {
	case rep.Gap != nil:
		a.printer.Message(gapMessage(rep.Target, &engine.Gap{From: engine.Position(rep.Gap.From), To: engine.Position(rep.Gap.To), Reason: rep.Gap.Reason}))
		if rep.Segments > 0 {
			a.printer.Message(msg.Message{Level: msg.LevelWarning, Target: rep.Target, Fact: "stored what the server still had: " + stored + ", up to " + rep.To})
		}
	case rep.Segments == 0 && rep.From != "":
		a.printer.Message(msg.Message{Level: msg.LevelOK, Target: rep.Target, Fact: "already caught up at " + rep.From + "; nothing to store"})
	default:
		a.printer.Message(msg.Message{Level: msg.LevelOK, Target: rep.Target, Fact: "caught up to " + rep.To + ": stored " + stored + " in " + took})
	}
}
