// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/cliout"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// slotInfo is one key slot as printed: never its ciphertext.
type slotInfo struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Scope     string    `json:"scope"`
	Label     string    `json:"label"`
	Recipient string    `json:"recipient,omitempty"`
	Created   time.Time `json:"created"`
}

func slotInfos(slots []*crypto.Slot) []slotInfo {
	out := make([]slotInfo, 0, len(slots))
	for _, s := range slots {
		out = append(out, slotInfo{
			ID: s.ID, Kind: string(s.Kind), Scope: string(s.Scope), Label: s.Label,
			Recipient: s.Recipient, Created: s.Created.UTC(),
		})
	}
	return out
}

func (a *app) slotTable(slots []slotInfo) cliout.Table {
	t := cliout.Table{Columns: []cliout.Column{
		{Header: "SLOT", Priority: 3}, {Header: "KIND", Priority: 3}, {Header: "SCOPE", Priority: 3},
		{Header: "LABEL", Priority: 2}, {Header: "CREATED", Priority: 1},
	}}
	for _, s := range slots {
		t.Rows = append(t.Rows, []cliout.Cell{
			{Text: s.ID, Style: cliout.Dim}, {Text: s.Kind}, {Text: s.Scope}, {Text: s.Label},
			{Text: a.printer.Time(s.Created), Style: cliout.Dim},
		})
	}
	return t
}

func (a *app) keyCommand() *cobra.Command {
	c := group("key", "Manage key slots",
		"List key slots", "pentym key list",
		"Add a full passphrase slot", "pentym key add --label ops",
		"Print a paper recovery code", "pentym key export --paper")
	c.AddCommand(
		a.keyAddCommand(),
		a.keyListCommand(),
		a.keyRemoveCommand(),
		a.keyRotateCommand(),
		a.keyExportCommand(),
	)
	return c
}

func (a *app) keyListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List key slots",
		Long:  "List the repository's key slots. Slots can be listed without unlocking the repository.",
		Example: example(
			"List key slots", "pentym key list",
			"List them as JSON", "pentym key list --json"),
		Args: args(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, r, err := a.openRepo(cmd.Context())
			if err != nil {
				return err
			}
			slots, err := r.Slots(cmd.Context())
			if err != nil {
				return err
			}
			sort.SliceStable(slots, func(i, j int) bool {
				si, sj := slots[i], slots[j]
				if si.Scope != sj.Scope {
					return si.Scope == crypto.ScopeFull
				}
				if !si.Created.Equal(sj.Created) {
					return si.Created.Before(sj.Created)
				}
				return si.Label < sj.Label
			})
			infos := slotInfos(slots)
			if a.opts.json {
				return a.printer.WriteJSON(struct {
					Slots []slotInfo `json:"slots"`
				}{infos})
			}
			if err := a.printer.Table(a.slotTable(infos)); err != nil {
				return err
			}
			if !r.Config().Insecure() && crypto.FullSlots(slots) < 2 {
				a.printer.Message(oneFullSlotWarning())
			}
			return nil
		},
	}
}

func oneFullSlotWarning() msg.Message {
	return msg.Message{
		Level: msg.LevelWarning, ID: idOneFullSlot,
		Fact:   "the repository has only one full key slot",
		Impact: "losing its passphrase or identity would make every backup unreadable",
		Next:   "pentym key add, or pentym key export --paper",
	}
}

type keyAddFlags struct {
	label  string
	agent  bool
	key    string
	age    string
	unlock string
}

func (a *app) keyAddCommand() *cobra.Command {
	f := &keyAddFlags{}
	c := &cobra.Command{
		Use:   "add",
		Short: "Add a key slot",
		Long: "Add a key slot: a new way to unlock the repository. It needs a full slot to unlock first.\n\n" +
			"The new slot is a passphrase slot unless --age-recipient is given. In a terminal, the new passphrase is asked for; elsewhere, give it with --key as a secret reference.",
		Example: example(
			"Add a full passphrase slot, typing the passphrase", "pentym key add --label ops",
			"Add a full slot for an age identity", "pentym key add --age-recipient age1… --label ci",
			"Add an agent slot whose passphrase is in a file", "pentym key add --agent --key file:/etc/pentym/agent-key --label db-host-2"),
		Args: args(),
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runKeyAdd(cmd, f) },
	}
	fl := c.Flags()
	fl.StringVar(&f.label, "label", "", "a name for the slot, shown in pentym key list")
	fl.BoolVar(&f.agent, "agent", false, "add an agent slot, which can write backups but never read them")
	fl.StringVar(&f.key, "key", "", "the new slot's passphrase, as a secret `reference` such as env:NAME")
	fl.StringVar(&f.age, "age-recipient", "", "add a slot unlocked by the age identity of this `recipient` (age1…)")
	addSlotFlag(c, &f.unlock)
	return c
}

func (a *app) runKeyAdd(cmd *cobra.Command, f *keyAddFlags) error {
	ctx := cmd.Context()
	if f.key != "" && f.age != "" {
		return newUsageError(cmd, "--key and --age-recipient cannot be used together")
	}
	if f.age != "" {
		if _, err := crypto.ParsePublicKey(f.age); err != nil {
			return newUsageError(cmd, fmt.Sprintf("--age-recipient %q is not an age X25519 recipient (age1…)", f.age))
		}
	}
	cfg, r, err := a.openRepo(ctx)
	if err != nil {
		return err
	}
	if r.Config().Insecure() {
		return insecureNoKeys()
	}
	keys, err := a.unlock(ctx, cfg, r, repo.UnlockOptions{SlotID: f.unlock, NeedFull: true})
	if err != nil {
		return err
	}
	scope := keys
	label := f.label
	if f.agent {
		agent := keys.Agent()
		scope = &agent
		if label == "" {
			label = "agent"
		}
	}

	var slot *crypto.Slot
	cred := cfg.Repository.Key
	if f.age != "" {
		if label == "" {
			label = "age"
		}
		slot, err = crypto.NewAgeSlot(label, *scope, f.age)
	} else {
		var pass secrets.Secret
		if f.key != "" {
			var ref secrets.Ref
			if pass, ref, err = a.resolveFlagRef(ctx, cmd, "--key", f.key); err == nil {
				cred = ref
			}
		} else {
			pass, err = a.askNewSecret(cmd, "passphrase for the new key slot")
			err = notInteractive(cmd, err, "--key")
		}
		if err != nil {
			return err
		}
		defer pass.Clear()
		if label == "" {
			label = "passphrase"
		}
		slot, err = crypto.NewPassphraseSlot(label, *scope, pass, slotKDF)
	}
	if err != nil {
		return err
	}
	op := beginOp(r, "key add", cred)
	if err := finishOp(ctx, op, r.AddSlot(ctx, slot)); err != nil {
		return err
	}
	info := slotInfos([]*crypto.Slot{slot})
	if a.opts.json {
		return a.printer.WriteJSON(struct {
			Slot slotInfo `json:"slot"`
		}{info[0]})
	}
	a.printer.Message(msg.Message{Level: msg.LevelOK, Fact: fmt.Sprintf("added %s %s slot %s (%s)", slot.Scope, slot.Kind, slot.ID, slot.Label)})
	return a.printer.Table(a.slotTable(info))
}

func (a *app) keyRemoveCommand() *cobra.Command {
	var unlock string
	c := &cobra.Command{
		Use:   "remove <slot>",
		Short: "Remove a key slot",
		Long: "Remove a key slot, so its passphrase or identity no longer unlocks the repository. It needs a full slot to unlock first.\n\n" +
			"The last full slot cannot be removed.",
		Example: example(
			"Remove a key slot", "pentym key remove <slot>"),
		Args: args("<slot>"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			ctx := cmd.Context()
			cfg, r, err := a.openRepo(ctx)
			if err != nil {
				return err
			}
			if r.Config().Insecure() {
				return insecureNoKeys()
			}
			slots, err := r.Slots(ctx)
			if err != nil {
				return err
			}
			if err := crypto.CheckRemove(slots, argv[0]); err != nil {
				return err
			}
			if _, err := a.unlock(ctx, cfg, r, repo.UnlockOptions{SlotID: unlock, NeedFull: true}); err != nil {
				return err
			}
			op := beginOp(r, "key remove", cfg.Repository.Key)
			if err := finishOp(ctx, op, r.RemoveSlot(ctx, argv[0])); err != nil {
				return err
			}
			if a.opts.json {
				return a.printer.WriteJSON(struct {
					Removed string `json:"removed"`
				}{argv[0]})
			}
			_, _ = fmt.Fprintln(a.printer.Stdout, argv[0])
			a.printer.Message(msg.Message{Level: msg.LevelOK, Fact: "removed key slot " + argv[0]})
			return nil
		},
	}
	addSlotFlag(c, &unlock)
	return c
}

func (a *app) keyExportCommand() *cobra.Command {
	var paper bool
	var unlock string
	c := &cobra.Command{
		Use:   "export",
		Short: "Export a key",
		Long: "Export a way to unlock the repository. With --paper, add a full slot unlocked by a new paper recovery code and print the code once. " +
			"It needs a full slot to unlock first.",
		Example: example(
			"Print a paper recovery code", "pentym key export --paper"),
		Args: args(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if !paper {
				return newUsageError(cmd, "pentym key export needs --paper")
			}
			cfg, r, err := a.openRepo(ctx)
			if err != nil {
				return err
			}
			if r.Config().Insecure() {
				return insecureNoKeys()
			}
			keys, err := a.unlock(ctx, cfg, r, repo.UnlockOptions{SlotID: unlock, NeedFull: true})
			if err != nil {
				return err
			}
			slot, code, err := crypto.NewPaperSlot("paper", *keys, slotKDF)
			if err != nil {
				return err
			}
			defer code.Clear()
			op := beginOp(r, "key export --paper", cfg.Repository.Key)
			if err := finishOp(ctx, op, r.AddSlot(ctx, slot)); err != nil {
				return err
			}
			info := slotInfos([]*crypto.Slot{slot})
			if a.opts.json {
				return a.printer.WriteJSON(struct {
					Slot      slotInfo `json:"slot"`
					PaperCode string   `json:"paper_code"`
				}{info[0], string(code.Expose())})
			}
			a.printer.Message(msg.Message{Level: msg.LevelOK, Fact: "added paper slot " + slot.ID})
			if err := a.printer.Table(a.slotTable(info)); err != nil {
				return err
			}
			a.printPaper(string(code.Expose()))
			return nil
		},
	}
	c.Flags().BoolVar(&paper, "paper", false, "add a full slot unlocked by a paper recovery code, and print the code once")
	addSlotFlag(c, &unlock)
	return c
}

// insecureNoKeys is the error for key commands on an unencrypted
// repository.
func insecureNoKeys() error {
	return &msg.Error{Code: msg.ExitFailure, Message: msg.Message{
		Level: msg.LevelFailed,
		Fact:  "the repository is not encrypted (--insecure-test-mode), so it has no key slots",
		Next:  "create an encrypted repository with pentym init",
	}}
}
