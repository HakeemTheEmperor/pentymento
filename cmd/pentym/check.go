// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/cliout"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/ops"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// Message IDs for what check finds (see docs/errors).
const (
	idObjectMissing msg.ID = "PTM-7001"
	idObjectCorrupt msg.ID = "PTM-7002"
	idPackUnindexed msg.ID = "PTM-7003"
	idObjectStray   msg.ID = "PTM-7004"
	idOneFullSlot   msg.ID = "PTM-4006"
)

// maxProblemsShown bounds the problems printed in text; --json lists all.
const maxProblemsShown = 20

var problemIDs = map[string]msg.ID{
	repo.ProblemMissing:   idObjectMissing,
	repo.ProblemCorrupt:   idObjectCorrupt,
	repo.ProblemUnindexed: idPackUnindexed,
	repo.ProblemStray:     idObjectStray,
	repo.ProblemSlots:     idOneFullSlot,
}

type checkFlags struct {
	full   bool
	sample float64
	slot   string
}

func (a *app) checkCommand() *cobra.Command {
	f := &checkFlags{}
	c := &cobra.Command{
		Use:   "check [target]",
		Short: "Check repository integrity",
		Long: "Check the repository's integrity: key slots, the index, that every pack the index lists exists, " +
			"that every chunk a snapshot references is indexed, and, for a sample of packs and log segments, " +
			"that every chunk decrypts and matches its ID. With a target, only that target's snapshots, packs and log segments are checked.\n\n" +
			"It needs a full key slot, and holds a shared lock so prune cannot delete what is being checked. " +
			"It exits with code 7 when it finds a problem.",
		Example: example(
			"Check the whole repository, reading 5% of the packs", "pentym check",
			"Check prod-mg's data", "pentym check prod-mg",
			"Read and verify every pack", "pentym check --full"),
		Args: optionalArg("[target]"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			target := ""
			if len(argv) == 1 {
				target = argv[0]
			}
			return a.runCheck(cmd, f, target)
		},
	}
	c.Flags().BoolVar(&f.full, "full", false, "read and verify every pack and log segment")
	c.Flags().Float64Var(&f.sample, "sample", repo.DefaultCheckSample*100, "the `percent` of packs and log segments to read and verify (at least one of each)")
	addSlotFlag(c, &f.slot)
	return c
}

func (a *app) runCheck(cmd *cobra.Command, f *checkFlags, target string) error {
	ctx := cmd.Context()
	if f.sample <= 0 || f.sample > 100 {
		return newUsageError(cmd, fmt.Sprintf("--sample must be above 0 and at most 100, not %g", f.sample))
	}
	if f.full && cmd.Flags().Changed("sample") {
		return newUsageError(cmd, "--full and --sample cannot be used together")
	}
	cfg, r, err := a.openRepo(ctx)
	if err != nil {
		return err
	}
	if _, err := a.unlock(ctx, cfg, r, repo.UnlockOptions{SlotID: f.slot, NeedFull: true}); err != nil {
		return err
	}
	tid := ""
	if target != "" {
		if tid, err = r.TargetID(target); err != nil {
			return err
		}
		targets, err := r.Targets(ctx)
		if err != nil {
			return err
		}
		if _, ok := targets[tid]; !ok {
			return &msg.Error{Code: msg.ExitNotFound, Message: msg.Message{
				Level: msg.LevelFailed, Target: target,
				Fact: "the repository holds nothing for this target",
				Next: "pentym list",
			}}
		}
	}

	var rep *repo.CheckReport
	op := beginTargetOp(r, "check", tid, cfg.Repository.Key)
	err = ops.WithLock(ctx, r, false, func() error {
		var err error
		rep, err = r.Check(ctx, repo.CheckOptions{Target: tid, Sample: f.sample / 100, Full: f.full})
		return err
	})
	var integrity error
	if err == nil && rep.Errors() > 0 {
		integrity = &msg.Error{Code: msg.ExitIntegrity, Message: msg.Message{
			Level: msg.LevelFailed, Target: target,
			Fact:   fmt.Sprintf("check found %d %s in the repository", rep.Errors(), plural(rep.Errors(), "problem", "problems")),
			Impact: "backups that use these objects may not restore",
			Next:   "see each message's page with pentym check -v; pentym repo rebuild-index rebuilds an unreadable index",
		}}
	}
	if jerr := finishOp(ctx, op, firstErr(err, integrity)); err == nil && integrity == nil && jerr != nil {
		return jerr
	}
	if err != nil {
		return err
	}
	if a.opts.json {
		if werr := a.printer.WriteJSON(rep); werr != nil {
			return werr
		}
		if integrity != nil {
			return silentExit(msg.ExitIntegrity)
		}
		return nil
	}
	if err := a.printer.Table(a.checkTable(rep, f.full)); err != nil {
		return err
	}
	for i, p := range rep.Problems {
		if i == maxProblemsShown {
			a.printer.Message(msg.Message{Level: msg.LevelWarning,
				Fact: fmt.Sprintf("%d more problems not shown", len(rep.Problems)-maxProblemsShown),
				Next: "pentym check --json lists them all"})
			break
		}
		a.printer.Message(problemMessage(p, target))
	}
	return integrity
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func problemMessage(p repo.Problem, target string) msg.Message {
	level := msg.LevelFailed
	if p.Severity == repo.SeverityWarning {
		level = msg.LevelWarning
	}
	next := map[string]string{
		repo.ProblemMissing:   "restore the object from a copy of the repository, or run a new backup; see docs/errors/PTM-7001",
		repo.ProblemCorrupt:   "restore the object from a copy of the repository, or run a new backup; see docs/errors/PTM-7002",
		repo.ProblemUnindexed: "pentym repo rebuild-index",
		repo.ProblemStray:     "remove the object if nothing else wrote it; see docs/errors/PTM-7004",
		repo.ProblemSlots:     "pentym key add, or pentym key export --paper",
	}[p.Kind]
	return msg.Message{Level: level, ID: problemIDs[p.Kind], Target: target, Fact: p.Object + ": " + p.Detail, Next: next}
}

// checkTable summarises a report, one row per step.
func (a *app) checkTable(rep *repo.CheckReport, full bool) cliout.Table {
	status := func(step, ok string) []cliout.Cell {
		level, text := msg.LevelOK, ok
		for _, p := range rep.Problems {
			if p.Step != step {
				continue
			}
			if p.Severity == repo.SeverityError {
				level = msg.LevelFailed
				break
			}
			level = msg.LevelWarning
		}
		if level != msg.LevelOK {
			n := 0
			for _, p := range rep.Problems {
				if p.Step == step {
					n++
				}
			}
			text = fmt.Sprintf("%s; %d %s", ok, n, plural(n, "problem", "problems"))
		}
		return []cliout.Cell{{Text: step}, a.printer.Status(level, text)}
	}
	read := "sampled"
	if full {
		read = "all"
	}
	return cliout.Table{
		Columns: []cliout.Column{{Header: "CHECK", Priority: 2}, {Header: "RESULT", Priority: 2}},
		Rows: [][]cliout.Cell{
			status(repo.CheckSlots, fmt.Sprintf("%d key slots, %d full", rep.Slots, rep.FullSlots)),
			status(repo.CheckIndex, fmt.Sprintf("%d index files, %d packs, %d chunks", rep.IndexFiles, rep.IndexedPacks, rep.IndexedChunks)),
			status(repo.CheckPacks, fmt.Sprintf("%d packs checked", rep.PacksChecked)),
			status(repo.CheckSnapshots, fmt.Sprintf("%d snapshots, %d chunk references", rep.Snapshots, rep.SnapshotChunks)),
			status(repo.CheckData, fmt.Sprintf("%d packs read (%s), %d chunks verified", rep.PacksRead, read, rep.ChunksVerified)),
			status(repo.CheckSegments, fmt.Sprintf("%d of %d log segments read", rep.SegmentsRead, rep.Segments)),
		},
	}
}
