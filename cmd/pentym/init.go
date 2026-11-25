// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/lock"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// idCondCreate warns that the storage ignores conditional create.
const idCondCreate msg.ID = "PTM-3005"

type initFlags struct {
	printSQL, printMongo bool
	insecure             bool
	keys                 []string
	ageRecipients        []string
	paper                bool
	agentKey             string
}

func (a *app) initCommand() *cobra.Command {
	f := &initFlags{}
	c := &cobra.Command{
		Use:   "init",
		Short: "Create an encrypted repository",
		Long: "Create an encrypted repository at repository.path, with at least two full key slots (or one and a paper recovery code) and an agent slot.\n\n" +
			"In a terminal, init asks for the passphrases. Elsewhere, give them as secret references with --key and --agent-key; a passphrase is never written on the command line.",
		Example: example(
			"Create a repository, answering the questions", "pentym init",
			"Create one without a terminal: two passphrase slots and an agent slot", "pentym init --key env:PENTYM_KEY --key file:/root/second-key --agent-key env:PENTYM_AGENT_KEY",
			"Use one passphrase slot and a paper recovery code", "pentym init --key env:PENTYM_KEY --paper --agent-key env:PENTYM_AGENT_KEY",
			"Print the minimal Postgres roles", "pentym init --print-sql",
			"Print the minimal MongoDB roles", "pentym init --print-mongo"),
		Args: args(),
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runInit(cmd, f) },
	}
	fl := c.Flags()
	fl.BoolVar(&f.printSQL, "print-sql", false, "print the SQL that creates the minimal Postgres roles")
	fl.BoolVar(&f.printMongo, "print-mongo", false, "print the commands that create the minimal MongoDB roles")
	fl.BoolVar(&f.insecure, "insecure-test-mode", false, "create an unencrypted repository for local development; refused for anything but a local path")
	fl.StringArrayVar(&f.keys, "key", nil, "add a full passphrase slot whose passphrase is at this secret `reference`, such as env:NAME (repeatable)")
	fl.StringArrayVar(&f.ageRecipients, "age-recipient", nil, "add a full slot unlocked by the age identity of this `recipient` (age1…) (repeatable)")
	fl.BoolVar(&f.paper, "paper", false, "add a full slot unlocked by a paper recovery code, printed once")
	fl.StringVar(&f.agentKey, "agent-key", "", "the passphrase of the agent slot, as a secret `reference` such as env:NAME")
	return c
}

// initResult is what init prints.
type initResult struct {
	Repository        string     `json:"repository"`
	Location          string     `json:"location"`
	Encryption        string     `json:"encryption"`
	ConditionalCreate bool       `json:"conditional_create"`
	Slots             []slotInfo `json:"slots"`
	// PaperCode is shown once and stored nowhere.
	PaperCode string `json:"paper_code,omitempty"`
}

func (a *app) runInit(cmd *cobra.Command, f *initFlags) error {
	ctx := cmd.Context()
	switch {
	case f.printSQL || f.printMongo:
		return &msg.Error{Code: msg.ExitNotImplemented, Message: msg.Message{
			Level: msg.LevelFailed, Fact: "--print-sql and --print-mongo are not implemented in this build", Next: "pentym --help",
		}}
	case f.insecure && (len(f.keys) > 0 || len(f.ageRecipients) > 0 || f.paper || f.agentKey != ""):
		return newUsageError(cmd, "--insecure-test-mode creates no key slots, so it takes no --key, --age-recipient, --paper or --agent-key")
	}
	cfg, err := a.repoConfig()
	if err != nil {
		return err
	}
	be, err := openBackend(cfg)
	if err != nil {
		return err
	}
	if err := repo.CheckEmpty(ctx, be); err != nil {
		return err
	}
	if f.insecure {
		if err := crypto.CheckInsecureTestMode(true); err != nil { // only local storage exists so far
			return err
		}
		return a.createRepo(ctx, be, cfg.Repository.Path, true, nil, nil, secrets.Secret{}, secrets.Ref{})
	}

	plan, err := a.planSlots(cmd, f)
	if err != nil {
		return err
	}
	defer plan.clear()
	m, err := crypto.Generate()
	if err != nil {
		return err
	}
	slots, paper, err := plan.build(m)
	if err != nil {
		return err
	}
	defer paper.Clear()
	return a.createRepo(ctx, be, cfg.Repository.Path, false, m, slots, paper, plan.cred)
}

// slotPlan is the slots init will create, with their secrets.
type slotPlan struct {
	passphrases []secrets.Secret
	ages        []string
	paper       bool
	agent       secrets.Secret
	// cred is the first reference used, for the journal.
	cred secrets.Ref
}

func (p *slotPlan) clear() {
	for _, s := range p.passphrases {
		s.Clear()
	}
	p.agent.Clear()
}

// planSlots gathers the slot secrets from the flags, asking in a terminal
// for whatever the flags leave out.
func (a *app) planSlots(cmd *cobra.Command, f *initFlags) (*slotPlan, error) {
	ctx := cmd.Context()
	p := &slotPlan{paper: f.paper}
	ok := false
	defer func() {
		if !ok {
			p.clear()
		}
	}()
	for _, k := range f.keys {
		s, ref, err := a.resolveFlagRef(ctx, cmd, "--key", k)
		if err != nil {
			return nil, err
		}
		if p.cred.IsZero() {
			p.cred = ref
		}
		p.passphrases = append(p.passphrases, s)
	}
	for _, r := range f.ageRecipients {
		if _, err := crypto.ParsePublicKey(r); err != nil {
			return nil, newUsageError(cmd, fmt.Sprintf("--age-recipient %q is not an age X25519 recipient (age1…)", r))
		}
		p.ages = append(p.ages, r)
	}
	interactive := a.printer.Interactive
	full := func() int { return len(p.passphrases) + len(p.ages) }
	if full() == 0 && interactive {
		s, err := a.askNewSecret(cmd, "passphrase for the first full key slot")
		if err != nil {
			return nil, err
		}
		p.passphrases = append(p.passphrases, s)
	}
	if full() == 1 && !p.paper && interactive {
		s, err := a.printer.ReadSecret("second passphrase (leave empty to print a paper recovery code instead)")
		if err != nil {
			return nil, err
		}
		if s.IsEmpty() {
			p.paper = true
		} else {
			again, err := a.printer.ReadSecret("second passphrase again")
			if err != nil {
				s.Clear()
				return nil, err
			}
			match := bytes.Equal(s.Expose(), again.Expose())
			again.Clear()
			if !match {
				s.Clear()
				return nil, newUsageError(cmd, "the two second passphrase entries do not match")
			}
			p.passphrases = append(p.passphrases, s)
		}
	}
	paper := 0
	if p.paper {
		paper = 1
	}
	if full() == 0 || full()+paper < 2 {
		return nil, &msg.Error{Code: msg.ExitUsage, Message: msg.Message{
			Level: msg.LevelFailed, ID: idTooFewSlots,
			Fact:   fmt.Sprintf("init needs two full key slots, or one and a paper recovery code; got %d", full()+paper),
			Impact: "with one way to unlock the repository, losing it would make every backup unreadable",
			Next:   "pentym init --key env:<NAME1> --key env:<NAME2> --agent-key env:<NAME3>, or add --paper",
		}}
	}
	for i := range p.passphrases {
		for j := range i {
			if bytes.Equal(p.passphrases[i].Expose(), p.passphrases[j].Expose()) {
				return nil, newUsageError(cmd, "two full key slots have the same passphrase; a second slot is only a backup if its passphrase differs")
			}
		}
	}

	switch {
	case f.agentKey != "":
		s, ref, err := a.resolveFlagRef(ctx, cmd, "--agent-key", f.agentKey)
		if err != nil {
			return nil, err
		}
		p.agent = s
		if p.cred.IsZero() {
			p.cred = ref
		}
	case interactive:
		s, err := a.askNewSecret(cmd, "passphrase for the agent slot")
		if err != nil {
			return nil, err
		}
		p.agent = s
	default:
		return nil, newUsageError(cmd, "init needs the agent slot's passphrase; give --agent-key with a reference such as env:NAME")
	}
	for _, s := range p.passphrases {
		if bytes.Equal(s.Expose(), p.agent.Expose()) {
			return nil, newUsageError(cmd, "the agent slot's passphrase is the same as a full slot's; it must differ, or a database host holding it could unlock the full slot")
		}
	}
	ok = true
	return p, nil
}

// resolveFlagRef parses and resolves a secret reference given in a flag.
// A plain value is refused: secrets never go on the command line.
func (a *app) resolveFlagRef(ctx context.Context, cmd *cobra.Command, flag, value string) (secrets.Secret, secrets.Ref, error) {
	ref, err := secrets.ParseRef(value)
	if err == nil && ref.IsPlaintext() {
		ref = secrets.Ref{}
		err = fmt.Errorf("%s takes a secret reference such as env:NAME or file:/path; a passphrase must never be written on the command line", flag)
	}
	if err != nil {
		return secrets.Secret{}, secrets.Ref{}, newUsageError(cmd, err.Error())
	}
	s, err := a.resolver().Resolve(ctx, "", ref)
	if err != nil {
		return secrets.Secret{}, secrets.Ref{}, err
	}
	if s.IsEmpty() {
		return secrets.Secret{}, secrets.Ref{}, newUsageError(cmd, fmt.Sprintf("%s %s holds an empty passphrase", flag, ref))
	}
	return s, ref, nil
}

// build creates the slots: full passphrase slots, age slots, the paper
// slot and the agent slot.
func (p *slotPlan) build(m *crypto.Material) ([]*crypto.Slot, secrets.Secret, error) {
	var slots []*crypto.Slot
	keys := m.Unlocked()
	for i, s := range p.passphrases {
		sl, err := crypto.NewPassphraseSlot(fmt.Sprintf("passphrase %d", i+1), keys, s, slotKDF)
		if err != nil {
			return nil, secrets.Secret{}, err
		}
		slots = append(slots, sl)
	}
	for i, r := range p.ages {
		sl, err := crypto.NewAgeSlot(fmt.Sprintf("age %d", i+1), keys, r)
		if err != nil {
			return nil, secrets.Secret{}, err
		}
		slots = append(slots, sl)
	}
	var code secrets.Secret
	if p.paper {
		sl, c, err := crypto.NewPaperSlot("paper", keys, slotKDF)
		if err != nil {
			return nil, secrets.Secret{}, err
		}
		slots, code = append(slots, sl), c
	}
	sl, err := crypto.NewPassphraseSlot("agent", keys.Agent(), p.agent, slotKDF)
	if err != nil {
		code.Clear()
		return nil, secrets.Secret{}, err
	}
	return append(slots, sl), code, nil
}

// createRepo tests conditional create, creates the repository, journals
// it and prints the result.
func (a *app) createRepo(ctx context.Context, be backend.Backend, location string, insecure bool,
	m *crypto.Material, slots []*crypto.Slot, paper secrets.Secret, cred secrets.Ref,
) error {
	cc, err := lock.TestConditionalCreate(ctx, be)
	if err != nil {
		return err
	}
	start := time.Now()
	r, err := repo.Create(ctx, be, repo.NewConfig(insecure, start), m, slots)
	if err != nil {
		return err
	}
	op := beginOp(r, "init", cred)
	if err := finishOp(ctx, op, nil); err != nil {
		return err
	}

	res := initResult{
		Repository: r.Config().ID, Location: location, Encryption: r.Config().Encryption,
		ConditionalCreate: cc, Slots: slotInfos(slots),
	}
	if !paper.IsEmpty() {
		res.PaperCode = string(paper.Expose())
	}
	if !cc {
		a.printer.Message(condCreateWarning())
	}
	if a.opts.json {
		return a.printer.WriteJSON(res)
	}
	a.printer.Message(msg.Message{Level: msg.LevelOK, Fact: fmt.Sprintf("created repository %s at %s", res.Repository, location)})
	if insecure {
		a.printer.Message(msg.Message{
			Level:  msg.LevelWarning,
			Fact:   "the repository is not encrypted (--insecure-test-mode)",
			Impact: "anyone who can read the directory can read every backup",
			Next:   "use it only for local development",
		})
	}
	if len(slots) > 0 {
		if err := a.printer.Table(a.slotTable(res.Slots)); err != nil {
			return err
		}
	}
	if res.PaperCode != "" {
		a.printPaper(res.PaperCode)
	}
	return nil
}

func condCreateWarning() msg.Message {
	return msg.Message{
		Level: msg.LevelWarning, ID: idCondCreate,
		Fact:   "the storage does not honour conditional create: creating the same object twice succeeded",
		Impact: "locks and capture leases fall back to a weaker protocol; two agents could briefly capture the same target",
		Next:   "use storage that supports conditional create; see docs/errors/PTM-3005",
	}
}

// printPaper prints a paper recovery code on stdout, with a note on
// stderr. The code is not stored anywhere and never logged or journaled.
func (a *app) printPaper(code string) {
	_, _ = fmt.Fprintf(a.printer.Stdout, "\npaper recovery code: %s\n", code)
	a.printer.Message(msg.Message{
		Level:  msg.LevelWarning,
		Fact:   "write the paper recovery code down now: it is shown only once and stored nowhere",
		Impact: "it unlocks every backup in the repository",
		Next:   "keep it offline, somewhere safe",
	})
}
