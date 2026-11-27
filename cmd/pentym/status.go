// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/agent"
	"github.com/HakeemTheEmperor/pentymento/internal/cliout"
	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/control"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/ops"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

func (a *app) statusCommand() *cobra.Command {
	var live bool
	var slot string
	c := &cobra.Command{
		Use:   "status",
		Short: "Show recovery windows, agent heartbeats and the last verify",
		Long: "Show every configured target's real protection, read from the repository: the point-in-time recovery window " +
			"actually covered beside the configured one (gaps shorten it), the agent capturing it and how long ago it was " +
			"last seen (a warning at 5 minutes, critical at 15), and the last verification. It works without an agent, " +
			"from any machine that can read the repository, with any key slot.\n\n" +
			"When no agent captures a target that had one, status prints the pentym catch-up command to run; with " +
			"catch_up.auto: true in the config it runs catch-up itself, for targets whose capture lease has expired.\n\n" +
			"--live also asks the running agent on this machine, through its control socket, for capture lag, the WAL " +
			"the replication slot retains or the oplog window, and its upload queue.",
		Example: example(
			"Show every target's recovery window", "pentym status",
			"Ask the running agent for live status", "pentym status --live",
			"Print the status as JSON", "pentym status --json"),
		Args: args(),
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runStatus(cmd.Context(), live, slot) },
	}
	c.Flags().BoolVar(&live, "live", false, "also ask the running agent for live status through the control socket")
	addSlotFlag(c, &slot)
	return c
}

// statusReport is status in --json.
type statusReport struct {
	Time         time.Time            `json:"time"`
	Targets      []targetStatusReport `json:"targets"`
	FullKeySlots int                  `json:"full_key_slots"`
	AgentReached *bool                `json:"agent_reachable,omitempty"`
	UploadQueue  *int                 `json:"upload_queue,omitempty"`
	AgentError   string               `json:"agent_error,omitempty"`
}

type targetStatusReport struct {
	Name           string             `json:"name"`
	Engine         string             `json:"engine"`
	EngineVersion  string             `json:"engine_version,omitempty"`
	Agent          agentReport        `json:"agent"`
	Window         windowReport       `json:"window"`
	SnapshotsOnly  bool               `json:"snapshots_only,omitempty"`
	Snapshots      int                `json:"snapshots"`
	LatestSnapshot *snapshotRef       `json:"latest_snapshot,omitempty"`
	Checkpoint     *checkpointReport  `json:"checkpoint,omitempty"`
	Gaps           []gapReport        `json:"gaps"`
	LastVerify     *struct{}          `json:"last_verify"`
	DeadMan        *struct{}          `json:"dead_man"`
	Live           *agent.LiveCapture `json:"live,omitempty"`
	CatchUp        *catchUpReport     `json:"catch_up,omitempty"`
}

type agentReport struct {
	State      string    `json:"state"`
	Level      string    `json:"level"`
	Host       string    `json:"host,omitempty"`
	AgentID    string    `json:"agent_id,omitempty"`
	Generation uint64    `json:"lease_generation,omitempty"`
	LastSeen   time.Time `json:"last_seen,omitzero"`
	AgeSeconds float64   `json:"last_seen_seconds_ago,omitempty"`
}

type windowReport struct {
	ConfiguredSeconds float64   `json:"configured_seconds"`
	ActualSeconds     float64   `json:"actual_seconds"`
	PITR              bool      `json:"pitr"`
	Start             time.Time `json:"start,omitzero"`
	End               time.Time `json:"end,omitzero"`
	Break             string    `json:"break,omitempty"`
	BreakTime         time.Time `json:"break_time,omitzero"`
	Shortened         bool      `json:"shortened_by_gap"`
}

type snapshotRef struct {
	ID  string    `json:"id"`
	End time.Time `json:"end"`
}

type checkpointReport struct {
	Position string    `json:"position"`
	Time     time.Time `json:"time"`
}

type gapReport struct {
	From   string    `json:"from,omitempty"`
	To     string    `json:"to,omitempty"`
	Reason string    `json:"reason"`
	Time   time.Time `json:"time"`
}

func (a *app) now() time.Time {
	if a.printer.Now != nil {
		return a.printer.Now()
	}
	return time.Now()
}

func (a *app) runStatus(ctx context.Context, live bool, slot string) error {
	cfg, r, err := a.openRepo(ctx)
	if err != nil {
		return err
	}
	if _, err := a.unlock(ctx, cfg, r, repo.UnlockOptions{SlotID: slot}); err != nil {
		return err
	}
	st, err := ops.ReadStatus(ctx, r, cfg, a.now())
	if err != nil {
		return err
	}
	rep := statusReport{Time: st.Time.UTC(), FullKeySlots: st.FullSlots}
	catchUps := map[string]*catchUpReport{}
	if cfg.CatchUp.Auto {
		for i, ts := range st.Targets {
			if !needsCatchUp(ts) {
				continue
			}
			cr := a.autoCatchUp(ctx, cfg, r, ts.Name)
			if cr == nil {
				continue
			}
			catchUps[ts.Name] = cr
			if next, err := ops.ReadTargetStatus(ctx, r, cfg, ts.Name, nil, nil, a.now()); err == nil {
				st.Targets[i] = *next
			}
		}
	}
	var liveStatus *agent.Status
	if live {
		liveStatus = a.liveStatus(ctx, cfg, &rep)
	}
	for _, ts := range st.Targets {
		tr := a.targetReport(ts, st.Time)
		tr.CatchUp = catchUps[ts.Name]
		if liveStatus != nil {
			for _, lt := range liveStatus.Targets {
				if lt.Name == ts.Name && lt.Capture != nil {
					tr.Live = lt.Capture
				}
			}
		}
		rep.Targets = append(rep.Targets, tr)
	}
	if a.opts.json {
		return a.printer.WriteJSON(rep)
	}
	return a.printStatus(st, rep, liveStatus)
}

// needsCatchUp reports whether a target had capture and no agent does it
// now: its lease was released, or its holder has been silent for at least
// the stale warning time, well past the lease length.
func needsCatchUp(ts ops.TargetStatus) bool {
	if ts.Checkpoint == nil || ts.SnapshotsOnly {
		return false
	}
	return ts.Agent.State == ops.AgentStopped || ts.Agent.State == ops.AgentStale
}

// autoCatchUp runs catch-up for a target (catch_up.auto) and reports it,
// or nil when it could not run.
func (a *app) autoCatchUp(ctx context.Context, cfg *config.Config, r *repo.Repository, name string) *catchUpReport {
	if !a.opts.json {
		a.printer.Message(msg.Message{Level: msg.LevelWarning, Target: name, Fact: "no agent captures this target; running catch-up (catch_up.auto)"})
	}
	eng, err := a.newEngine(cfg, cfg.Targets[name])
	var res *agent.CatchUpResult
	if err == nil {
		res, err = agent.CatchUp(ctx, agent.Options{
			Config: cfg, Repo: r, Telemetry: a.tel,
			Engine: func(agent.Target) (engine.Engine, error) { return eng, nil },
		}, name, nil)
	}
	if err != nil {
		var me *msg.Error
		if errors.As(err, &me) {
			m := me.Message
			m.Target = name
			a.printer.Message(m)
		} else {
			a.printer.Message(msg.Message{Level: msg.LevelFailed, Target: name, Fact: "catch-up failed", Cause: err.Error(), Next: "pentym catch-up " + name})
		}
		return nil
	}
	cr := catchUpReport{
		Target: name, From: string(res.From), To: string(res.To), ServerPosition: string(res.Head),
		Segments: res.Segments, Bytes: res.Bytes, Generation: res.Generation,
		LeaseWait: res.LeaseWait.Seconds(), DurationSeconds: res.Duration.Seconds(), Finished: time.Now().UTC(),
	}
	if g := res.Gap; g != nil {
		cr.Gap = &gapInfo{From: string(g.From), To: string(g.To), Reason: g.Reason}
	}
	if !a.opts.json {
		a.printCatchUp(cr)
	}
	return &cr
}

// liveStatus asks the agent on this machine; when it does not answer,
// status falls back to the repository and says so (PRD 5.3).
func (a *app) liveStatus(ctx context.Context, cfg *config.Config, rep *statusReport) *agent.Status {
	sock, err := cfg.AgentSocket()
	var s *agent.Status
	if err == nil {
		s, err = control.NewClient(sock).Status(ctx)
	}
	reached := err == nil
	rep.AgentReached = &reached
	if err != nil {
		rep.AgentError = err.Error()
		if !a.opts.json {
			fact := "agent not reachable"
			if sock != "" {
				fact += " at " + sock
			}
			a.printer.Message(msg.Message{Level: msg.LevelWarning, Fact: fact + "; showing what the repository holds",
				Next: "pentym agent, on the machine that should capture"})
		}
		return nil
	}
	rep.UploadQueue = &s.UploadQueue
	return s
}

func (a *app) targetReport(ts ops.TargetStatus, now time.Time) targetStatusReport {
	w := ts.Window
	tr := targetStatusReport{
		Name: ts.Name, Engine: ts.Engine, EngineVersion: ts.EngineVersion, SnapshotsOnly: ts.SnapshotsOnly, Snapshots: ts.Snapshots,
		Agent: agentReport{State: ts.Agent.State, Level: ts.Agent.Level.String(), Host: ts.Agent.Host, AgentID: ts.Agent.AgentID,
			Generation: ts.Agent.Generation, LastSeen: ts.Agent.LastSeen.UTC(), AgeSeconds: ts.Agent.Age.Seconds()},
		Window: windowReport{ConfiguredSeconds: w.Configured.Seconds(), ActualSeconds: w.Actual().Seconds(), PITR: w.PITR,
			Start: w.Start.UTC(), End: w.End.UTC(), Break: w.Break, BreakTime: w.BreakTime.UTC(), Shortened: w.Shortened(now)},
		Gaps: []gapReport{},
	}
	if ts.Agent.LastSeen.IsZero() {
		tr.Agent.LastSeen = time.Time{}
	}
	if !w.PITR {
		tr.Window.Start, tr.Window.End = time.Time{}, time.Time{}
	}
	if w.BreakTime.IsZero() {
		tr.Window.BreakTime = time.Time{}
	}
	if s := ts.LatestSnapshot; s != nil {
		tr.LatestSnapshot = &snapshotRef{ID: s.ID, End: s.End.UTC()}
	}
	if cp := ts.Checkpoint; cp != nil {
		tr.Checkpoint = &checkpointReport{Position: cp.Position, Time: cp.Time.UTC()}
	}
	for _, g := range ts.Gaps {
		tr.Gaps = append(tr.Gaps, gapReport{From: g.From, To: g.To, Reason: g.Reason, Time: g.Time.UTC()})
	}
	return tr
}

// engineLabel is the engine and its major version, as "postgres 17" or
// "mongodb 8.0".
func engineLabel(engineName, version string) string {
	if version == "" {
		return engineName
	}
	parts := strings.SplitN(version, ".", 3)
	if engineName == "postgres" || len(parts) < 2 {
		return engineName + " " + parts[0]
	}
	return engineName + " " + parts[0] + "." + parts[1]
}

func (a *app) agentCell(ts ops.TargetStatus) cliout.Cell {
	v := ts.Agent
	switch v.State {
	case ops.AgentCapturing:
		return a.printer.Status(v.Level, "capturing ("+cliout.Duration(v.Age)+")")
	case ops.AgentStale:
		return a.printer.Status(v.Level, "stale ("+cliout.Duration(v.Age)+")")
	case ops.AgentStopped:
		return a.printer.Status(v.Level, "stopped ("+cliout.Duration(v.Age)+")")
	}
	if ts.SnapshotsOnly {
		return cliout.Cell{Text: "none", Style: cliout.Dim}
	}
	return a.printer.Status(v.Level, "none")
}

// upTo says how recent the window's newest point is: relative while
// recent, a time otherwise.
func (a *app) upTo(t, now time.Time) string {
	if d := now.Sub(t); d < time.Hour {
		return "up to " + cliout.Duration(max(d, 0)) + " ago"
	}
	return "up to " + a.printer.Time(t)
}

func (a *app) windowCell(ts ops.TargetStatus, now time.Time) cliout.Cell {
	w := ts.Window
	switch {
	case ts.Snapshots == 0:
		return a.printer.Status(msg.LevelFailed, "no snapshot")
	case ts.SnapshotsOnly:
		return a.printer.Status(msg.LevelOK, "snapshots only · "+a.printer.Time(ts.LatestSnapshot.End))
	case !w.Logs:
		return a.printer.Status(msg.LevelWarning, "snapshots only · "+a.printer.Time(ts.LatestSnapshot.End))
	case !w.PITR:
		return a.printer.Status(msg.LevelFailed, "no snapshot in the captured log")
	}
	level := msg.LevelOK
	if w.Shortened(now) {
		level = msg.LevelWarning
	}
	return a.printer.Status(level, cliout.Duration(w.Actual())+" · "+a.upTo(w.End, now))
}

func (a *app) printStatus(st *ops.Status, rep statusReport, live *agent.Status) error {
	now := st.Time
	t := cliout.Table{Columns: []cliout.Column{
		{Header: "TARGET", Priority: 5}, {Header: "ENGINE", Priority: 2}, {Header: "AGENT", Priority: 4},
		{Header: "PITR WINDOW", Priority: 5}, {Header: "LAST VERIFY", Priority: 3},
	}}
	for _, ts := range st.Targets {
		t.Rows = append(t.Rows, []cliout.Cell{
			{Text: ts.Name, Style: cliout.Bold}, {Text: engineLabel(ts.Engine, ts.EngineVersion)},
			a.agentCell(ts), a.windowCell(ts, now), {Text: "—", Style: cliout.Dim},
		})
	}
	if len(st.Targets) > 0 {
		if err := a.printer.Table(t); err != nil {
			return err
		}
	}
	if live != nil {
		if err := a.printLive(rep, live); err != nil {
			return err
		}
	}
	for _, ts := range st.Targets {
		for _, m := range a.statusMessages(ts, now) {
			a.printer.Message(m)
		}
	}
	if st.FullSlots < 2 {
		a.printer.Message(msg.Message{Level: msg.LevelWarning, ID: idOneFullSlot,
			Fact:   fmt.Sprintf("the repository has only %d full key slot", st.FullSlots),
			Impact: "losing its passphrase or identity would make every backup unreadable",
			Next:   "pentym key add, or pentym key export --paper"})
	}
	if len(st.Targets) == 0 {
		a.printer.Message(msg.Message{Level: msg.LevelWarning, Fact: "no targets in the config", Next: "add them under targets: in the config"})
	}
	return nil
}

// statusMessages are a target's warnings, each with its next step
// (Brand 11).
func (a *app) statusMessages(ts ops.TargetStatus, now time.Time) []msg.Message {
	var out []msg.Message
	w, v := ts.Window, ts.Agent
	since := "the last stored change"
	if w.PITR {
		since = a.printer.Time(w.End)
	} else if ts.Checkpoint != nil {
		since = a.printer.Time(ts.Checkpoint.Time)
	}
	switch {
	case ts.SnapshotsOnly:
	case v.State == ops.AgentStale:
		out = append(out, msg.Message{Level: v.Level, Target: ts.Name,
			Fact: fmt.Sprintf("agent last seen %s ago on %s; changes since %s are not protected", cliout.Duration(v.Age), v.Host, since),
			Next: "pentym catch-up " + ts.Name})
	case v.State == ops.AgentStopped && ts.Checkpoint != nil:
		out = append(out, msg.Message{Level: v.Level, Target: ts.Name,
			Fact: fmt.Sprintf("the agent on %s stopped %s ago; changes since %s are not protected", v.Host, cliout.Duration(v.Age), since),
			Next: "pentym catch-up " + ts.Name + ", or start the agent"})
	case v.State == ops.AgentNone || v.State == ops.AgentStopped:
		out = append(out, msg.Message{Level: msg.LevelWarning, Target: ts.Name,
			Fact: "no agent captures this target, so point-in-time recovery is off",
			Next: "pentym agent"})
	}
	switch {
	case ts.Snapshots == 0:
		out = append(out, msg.Message{Level: msg.LevelFailed, Target: ts.Name, Fact: "no snapshot yet: nothing can be restored",
			Next: "pentym backup " + ts.Name})
	case !ts.SnapshotsOnly && w.Logs && !w.PITR:
		out = append(out, msg.Message{Level: msg.LevelFailed, Target: ts.Name,
			Fact:   "no snapshot lies within the captured log, so point-in-time recovery has no starting point",
			Impact: "only the snapshots themselves can be restored",
			Next:   "pentym backup " + ts.Name})
	case w.Shortened(now):
		out = append(out, msg.Message{Level: msg.LevelWarning, ID: agent.IDGap, Target: ts.Name,
			Fact: fmt.Sprintf("configured %s, actual %s (gap at %s): %s", cliout.Duration(w.Configured), cliout.Duration(w.Actual()),
				a.printer.Time(w.BreakTime), w.Break),
			Impact: "point-in-time recovery cannot reach before the gap",
			Next:   "nothing to do: the window grows back as capture continues"})
	}
	return out
}

func (a *app) printLive(rep statusReport, live *agent.Status) error {
	t := cliout.Table{Columns: []cliout.Column{
		{Header: "TARGET", Priority: 5}, {Header: "LEASE", Priority: 4}, {Header: "LAG", Priority: 5}, {Header: "RETAINED / WINDOW", Priority: 3},
	}}
	for _, lt := range live.Targets {
		lease := cliout.Cell{Text: "standby", Style: cliout.Dim}
		if lt.Holding {
			lease = a.printer.Status(msg.LevelOK, fmt.Sprintf("held (generation %d)", lt.Generation))
		}
		lag, held := cliout.Cell{Text: "—", Style: cliout.Dim}, cliout.Cell{Text: "—", Style: cliout.Dim}
		if c := lt.Capture; c != nil {
			lag = cliout.Cell{Text: cliout.Duration(time.Duration(c.LagSeconds * float64(time.Second)))}
			switch {
			case c.OplogWindowSeconds > 0:
				held = cliout.Cell{Text: "oplog " + cliout.Duration(time.Duration(c.OplogWindowSeconds*float64(time.Second)))}
			case c.RetainedLimit > 0:
				held = cliout.Cell{Text: cliout.Bytes(c.RetainedBytes) + " of " + cliout.Bytes(c.RetainedLimit)}
			default:
				held = cliout.Cell{Text: cliout.Bytes(c.RetainedBytes) + " (no limit)"}
			}
		}
		t.Rows = append(t.Rows, []cliout.Cell{{Text: lt.Name, Style: cliout.Bold}, lease, lag, held})
	}
	_, _ = fmt.Fprintln(a.printer.Stdout)
	if err := a.printer.Table(t); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(a.printer.Stdout, "\nagent %s on %s · upload queue %d\n", live.AgentID, live.Host, *rep.UploadQueue)
	return nil
}
