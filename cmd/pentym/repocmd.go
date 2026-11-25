// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

func (a *app) repoCommand() *cobra.Command {
	c := group("repo", "Maintain the repository",
		"Upgrade the repository format", "pentym repo upgrade",
		"Rebuild the index from the packs", "pentym repo rebuild-index")
	c.AddCommand(a.repoUpgradeCommand(), a.repoRebuildIndexCommand())
	return c
}

func (a *app) repoUpgradeCommand() *cobra.Command {
	var slot string
	c := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade the repository to the newest format this pentym understands",
		Long: "Upgrade the repository's format. Upgrading pentym never changes the format; only this command does.\n\n" +
			"Run it only after every pentym that uses the repository (agents, CLIs, CI jobs) has been upgraded: older versions refuse the newer format. " +
			"It holds the exclusive lock, asks for confirmation in a terminal, and is journaled.",
		Example: example(
			"Upgrade the repository format", "pentym repo upgrade"),
		Args: args(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, r, err := a.openRepo(ctx)
			if err != nil {
				return err
			}
			if _, err := a.unlock(ctx, cfg, r, repo.UnlockOptions{SlotID: slot, NeedFull: true}); err != nil {
				return err
			}
			from := r.Config().Version
			type result struct {
				From     int      `json:"from"`
				To       int      `json:"to"`
				Upgraded bool     `json:"upgraded"`
				Steps    []string `json:"steps"`
			}
			res := result{From: from, To: from, Steps: []string{}}
			op := beginOp(r, "repo upgrade", cfg.Repository.Key)
			outcome := repo.ResultOK
			err = withLock(ctx, r, true, func() error {
				planFn := (*repo.Repository).UpgradePlan
				if a.upgradePlan != nil {
					planFn = a.upgradePlan
				}
				plan, err := planFn(r)
				if err != nil {
					return err
				}
				if len(plan) == 0 {
					outcome = repo.ResultSkipped
					return nil
				}
				for _, m := range plan {
					res.Steps = append(res.Steps, fmt.Sprintf("format %d → %d: %s", m.From, m.From+1, m.Describe))
				}
				if a.printer.Interactive {
					for _, s := range res.Steps {
						a.printer.Message(msg.Message{Level: msg.LevelWarning, Fact: "upgrade " + s})
					}
					ok, err := a.printer.Confirm(fmt.Sprintf("every pentym using this repository must already understand format %d\ncontinue?", repo.FormatVersion))
					if err != nil {
						return err
					}
					if !ok {
						outcome = repo.ResultSkipped
						return errDeclined
					}
				}
				if err := r.Upgrade(ctx, plan); err != nil {
					return err
				}
				res.To, res.Upgraded = r.Config().Version, true
				return nil
			})
			if errors.Is(err, errDeclined) {
				_ = op.Finish(ctx, repo.ResultSkipped, errors.New("declined at the confirmation prompt"))
				return &msg.Error{Code: msg.ExitDeclined, Message: msg.Message{
					Level: msg.LevelFailed, Fact: "upgrade declined; nothing was changed", Next: "pentym repo upgrade",
				}}
			}
			if ferr := op.Finish(ctx, outcome, err); err == nil {
				err = ferr
			}
			if err != nil {
				return err
			}
			if a.opts.json {
				return a.printer.WriteJSON(res)
			}
			if !res.Upgraded {
				a.printer.Message(msg.Message{Level: msg.LevelOK, Fact: fmt.Sprintf("repository format %d is current; nothing to upgrade", from)})
				return nil
			}
			a.printer.Message(msg.Message{Level: msg.LevelOK, Fact: fmt.Sprintf("upgraded the repository from format %d to %d", res.From, res.To)})
			return nil
		},
	}
	addSlotFlag(c, &slot)
	return c
}

// errDeclined ends an operation the user declined at a prompt.
var errDeclined = errors.New("declined")

func (a *app) repoRebuildIndexCommand() *cobra.Command {
	var slot string
	c := &cobra.Command{
		Use:   "rebuild-index",
		Short: "Rebuild the index from the packs",
		Long: "Rebuild the index by reading every pack's trailer, then replace the old index files. " +
			"Use it when pentym check reports an unreadable index file or unindexed packs.\n\n" +
			"It needs any key slot, an agent slot included, and holds the exclusive lock. Packs whose trailer cannot be read are reported, and the command exits with code 7.",
		Example: example(
			"Rebuild the index", "pentym repo rebuild-index"),
		Args: args(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, r, err := a.openRepo(ctx)
			if err != nil {
				return err
			}
			if _, err := a.unlock(ctx, cfg, r, repo.UnlockOptions{SlotID: slot}); err != nil {
				return err
			}
			var stats *repo.RebuildStats
			op := beginOp(r, "repo rebuild-index", cfg.Repository.Key)
			err = withLock(ctx, r, true, func() error {
				var err error
				stats, err = r.RebuildIndex(ctx)
				return err
			})
			var integrity error
			if err == nil && len(stats.Unreadable) > 0 {
				integrity = &msg.Error{Code: msg.ExitIntegrity, Message: msg.Message{
					Level: msg.LevelFailed, ID: idObjectCorrupt,
					Fact:   fmt.Sprintf("%d %s could not be read; their chunks are not in the index", len(stats.Unreadable), plural(len(stats.Unreadable), "pack", "packs")),
					Impact: "backups that use those chunks will not restore",
					Next:   "pentym check --full",
				}}
			}
			if ferr := finishOp(ctx, op, firstErr(err, integrity)); err == nil && integrity == nil && ferr != nil {
				return ferr
			}
			if err != nil {
				return err
			}
			if a.opts.json {
				if stats.Unreadable == nil {
					stats.Unreadable = []string{}
				}
				if werr := a.printer.WriteJSON(stats); werr != nil {
					return werr
				}
				if integrity != nil {
					return silentExit(msg.ExitIntegrity)
				}
				return nil
			}
			a.printer.Message(msg.Message{Level: msg.LevelOK, Fact: fmt.Sprintf(
				"rebuilt the index from %d packs (%d chunks): %d index files written, %d removed",
				stats.Packs, stats.Chunks, stats.Written, stats.Removed)})
			for _, id := range stats.Unreadable {
				_, _ = fmt.Fprintln(a.printer.Stdout, id)
			}
			return integrity
		},
	}
	addSlotFlag(c, &slot)
	return c
}

// keyRotateCommand is pentym key rotate.
func (a *app) keyRotateCommand() *cobra.Command {
	var slot string
	c := &cobra.Command{
		Use:   "rotate",
		Short: "Generate a new repository key pair and re-wrap every data key",
		Long: "Generate a new repository key pair, re-wrap every data key in the index and every log segment's key to it, and keep the replaced private key, " +
			"encrypted, so data whose keys cannot be re-wrapped (pack trailers) stays readable. Packs are never rewritten. " +
			"Key slots are unchanged.\n\n" +
			"Rotation protects data written after it: whoever holds a replaced private key can still read what it protected. " +
			"It needs a full key slot, holds the exclusive lock, and is safe to run again after an interruption.",
		Example: example(
			"Rotate the repository key pair", "pentym key rotate"),
		Args: args(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, r, err := a.openRepo(ctx)
			if err != nil {
				return err
			}
			if r.Config().Insecure() {
				return insecureNoKeys()
			}
			keys, err := a.unlock(ctx, cfg, r, repo.UnlockOptions{SlotID: slot, NeedFull: true})
			if err != nil {
				return err
			}
			var stats *repo.RotateStats
			op := beginOp(r, "key rotate", cfg.Repository.Key)
			err = withLock(ctx, r, true, func() error {
				var err error
				stats, err = r.RotateKeys(ctx, *keys)
				return err
			})
			if err := finishOp(ctx, op, err); err != nil {
				return err
			}
			if a.opts.json {
				return a.printer.WriteJSON(stats)
			}
			a.printer.Message(msg.Message{Level: msg.LevelOK, Fact: fmt.Sprintf(
				"rotated the repository key pair: re-wrapped the keys of %d packs in %d index files and %d log segments; the replaced private key is kept as %s",
				stats.Packs, stats.IndexFiles, stats.Segments, stats.PreviousKey)})
			return nil
		},
	}
	addSlotFlag(c, &slot)
	return c
}
