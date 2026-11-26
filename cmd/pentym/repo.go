// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"context"
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/local"
	"github.com/HakeemTheEmperor/pentymento/internal/cliout"
	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/telemetry"
)

// Message IDs for the CLI's repository and key handling (see docs/errors).
const (
	idRepoNotConfigured msg.ID = "PTM-1009"
	idNothingToUnlock   msg.ID = "PTM-4004"
	idTooFewSlots       msg.ID = "PTM-4005"
)

// slotKDF is the Argon2id cost of new passphrase and paper slots. Tests
// lower it.
var slotKDF = crypto.DefaultKDF

// repoConfig loads the config and checks it names a repository.
func (a *app) repoConfig() (*config.Config, error) {
	cfg, err := a.loadConfig(os.Environ())
	if err != nil {
		return nil, err
	}
	if cfg.Repository.Path == "" {
		return nil, &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
			Level: msg.LevelFailed, ID: idRepoNotConfigured,
			Fact: "no repository location in " + a.configPath(),
			Next: "set repository.path to an absolute directory in the config",
		}}
	}
	return cfg, nil
}

// openBackend returns the storage backend for the configured repository.
func openBackend(cfg *config.Config) (backend.Backend, error) {
	be, err := local.New(cfg.Repository.Path)
	if err != nil {
		return nil, &msg.Error{Code: msg.ExitAccess, Err: err, Message: msg.Message{
			Level: msg.LevelFailed,
			Fact:  "cannot open the repository directory " + cfg.Repository.Path,
			Cause: err.Error(),
			Next:  "check that the directory exists and pentym's user can write to it",
		}}
	}
	return be, nil
}

// openRepo opens the configured repository without unlocking it.
func (a *app) openRepo(ctx context.Context) (*config.Config, *repo.Repository, error) {
	cfg, err := a.repoConfig()
	if err != nil {
		return nil, nil, err
	}
	if _, err := os.Stat(cfg.Repository.Path); errors.Is(err, os.ErrNotExist) {
		return nil, nil, &msg.Error{Code: msg.ExitNotFound, Err: err, Message: msg.Message{
			Level: msg.LevelFailed, ID: repo.IDNotFound,
			Fact: "no repository found at " + cfg.Repository.Path + " (the directory does not exist)",
			Next: "check repository.path in the config, or create a repository with pentym init",
		}}
	}
	be, err := openBackend(cfg)
	if err != nil {
		return nil, nil, err
	}
	r, err := repo.Open(ctx, be)
	if err != nil {
		return nil, nil, err
	}
	return cfg, r, nil
}

// resolver resolves secret references with the app's environment.
func (a *app) resolver() secrets.Resolver { return secrets.Resolver{Getenv: a.getenv} }

// unlock unlocks r with repository.key, or by asking in a terminal, and
// returns the slot's keys. An insecure repository needs no unlocking and
// returns nil keys.
func (a *app) unlock(ctx context.Context, cfg *config.Config, r *repo.Repository, opts repo.UnlockOptions) (*crypto.Unlocked, error) {
	if r.Config().Insecure() {
		return nil, nil
	}
	var secret secrets.Secret
	var err error
	switch {
	case !cfg.Repository.Key.IsZero():
		secret, err = a.resolver().Resolve(ctx, "", cfg.Repository.Key)
	case a.printer.Interactive:
		secret, err = a.printer.ReadSecret("passphrase, recovery code or age identity for the repository")
	default:
		return nil, &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
			Level: msg.LevelFailed, ID: idNothingToUnlock,
			Fact: "cannot unlock the repository: no terminal to ask for a passphrase, and no repository.key in the config",
			Next: "set repository.key to a reference such as env:PENTYM_KEY, or run the command in a terminal",
		}}
	}
	if err != nil {
		return nil, err
	}
	defer secret.Clear()
	u, _, err := r.UnlockWith(ctx, secret, opts)
	return u, err
}

// addSlotFlag adds --slot, which limits unlocking to one key slot.
func addSlotFlag(c *cobra.Command, dst *string) {
	c.Flags().StringVar(dst, "slot", "", "unlock with this key slot only (see pentym key list); saves trying every slot")
}

// askNewSecret asks for a new passphrase twice and checks both match.
func (a *app) askNewSecret(cmd *cobra.Command, what string) (secrets.Secret, error) {
	first, err := a.printer.ReadSecret(what)
	if err != nil {
		return secrets.Secret{}, err
	}
	again, err := a.printer.ReadSecret(what + " again")
	if err != nil {
		first.Clear()
		return secrets.Secret{}, err
	}
	defer again.Clear()
	if !bytes.Equal(first.Expose(), again.Expose()) {
		first.Clear()
		return secrets.Secret{}, newUsageError(cmd, "the two "+what+" entries do not match")
	}
	return first, nil
}

// notInteractive turns cliout.ErrNotInteractive into a usage error that
// names the flag to use instead.
func notInteractive(cmd *cobra.Command, err error, flag string) error {
	if errors.Is(err, cliout.ErrNotInteractive) {
		return newUsageError(cmd, "no terminal to ask in; give "+flag+" instead")
	}
	return err
}

// beginOp starts a journal entry for a repository-wide operation, before
// the operation runs.
func beginOp(r *repo.Repository, operation string, cred secrets.Ref) *repo.Operation {
	return beginTargetOp(r, operation, "", cred)
}

// beginTargetOp starts a journal entry for an operation on one target (its
// ID), or on the whole repository when tid is empty.
func beginTargetOp(r *repo.Repository, operation, tid string, cred secrets.Ref) *repo.Operation {
	return r.BeginOperation(repo.CurrentIdentity(), operation, tid, telemetry.NewRunID(), cred)
}

// finishOp records how an operation ended. A failure to write the entry is
// returned only when the operation itself succeeded.
func finishOp(ctx context.Context, op *repo.Operation, opErr error) error {
	jerr := op.Finish(context.WithoutCancel(ctx), repo.ResultOK, opErr)
	if opErr != nil {
		return opErr
	}
	return jerr
}
