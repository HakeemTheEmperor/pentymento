// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/devtools/leaktest"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// slotDoc parses `key add --json`.
func slotDoc(t *testing.T, r result) slotInfo {
	t.Helper()
	var doc struct {
		Slot slotInfo `json:"slot"`
	}
	if r.code != msg.ExitOK || json.Unmarshal([]byte(r.stdout), &doc) != nil {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	return doc.Slot
}

func TestKeyList(t *testing.T) {
	c := newCLIEnv(t, "")
	res := c.mustInit()
	r := c.pentym("--json", "key", "list")
	var doc struct {
		Slots []slotInfo `json:"slots"`
	}
	if r.code != msg.ExitOK || json.Unmarshal([]byte(r.stdout), &doc) != nil || len(doc.Slots) != 3 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	// Full slots first, then agent slots; no unlocking needed.
	if doc.Slots[2].Scope != "agent" || doc.Slots[0].Scope != "full" || doc.Slots[1].Scope != "full" {
		t.Errorf("slots = %+v", doc.Slots)
	}
	if strings.Contains(r.stdout, "ciphertext") {
		t.Error("key list prints slot ciphertext")
	}

	r = c.pentym("key", "list")
	if r.code != msg.ExitOK || r.stderr != "" || strings.Count(r.stdout, "\n") != 4 {
		t.Errorf("text: exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}

	// With one full slot left, list warns.
	c.setKey("env:K1")
	if r := c.pentym("key", "remove", res.Slots[1].ID); r.code != msg.ExitOK {
		t.Fatalf("remove: exit %d, %s", r.code, r.stderr)
	}
	r = c.pentym("key", "list")
	if !strings.Contains(r.stderr, "! the repository has only one full key slot\n") {
		t.Errorf("stderr = %q", r.stderr)
	}
}

// TestKeyListMissingRepository: commands that read the repository report a
// missing directory and do not create it.
func TestKeyListMissingRepository(t *testing.T) {
	c := newCLIEnv(t, "")
	r := c.pentym("--json", "key", "list")
	if doc := errorDoc(t, r); r.code != msg.ExitNotFound || doc.ID != repo.IDNotFound {
		t.Errorf("exit %d, %+v", r.code, doc)
	}
	if _, err := os.Stat(c.repoDir); !os.IsNotExist(err) {
		t.Errorf("key list created %s: %v", c.repoDir, err)
	}
	// An existing, empty directory is no repository either.
	if err := os.MkdirAll(c.repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if r := c.pentym("--json", "key", "list"); r.code != msg.ExitNotFound || errorDoc(t, r).ID != repo.IDNotFound {
		t.Errorf("empty directory: exit %d", r.code)
	}
}

func TestKeyAdd(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		args    []string
		secret  string
		kind    crypto.Kind
		scope   crypto.Scope
		label   string
		canRead bool
	}{
		{"full passphrase", []string{"--key", "env:K3", "--label", "ops"}, pass3, crypto.KindPassphrase, crypto.ScopeFull, "ops", true},
		{"agent passphrase", []string{"--agent", "--key", "env:K3"}, pass3, crypto.KindPassphrase, crypto.ScopeAgent, "agent", false},
		{"full age", []string{"--age-recipient", id.Recipient().String()}, id.String(), crypto.KindAge, crypto.ScopeFull, "age", true},
		{"agent age", []string{"--agent", "--age-recipient", id.Recipient().String(), "--label", "db-2"}, id.String(), crypto.KindAge, crypto.ScopeAgent, "db-2", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCLIEnv(t, "env:K1")
			c.mustInit()
			s := slotDoc(t, c.pentym(append([]string{"--json", "key", "add"}, tt.args...)...))
			if s.Kind != string(tt.kind) || s.Scope != string(tt.scope) || s.Label != tt.label {
				t.Errorf("added %+v", s)
			}
			r, slot, err := c.unlock(tt.secret, repo.UnlockOptions{})
			if err != nil || slot.ID != s.ID || r.CanRead() != tt.canRead {
				t.Errorf("unlock with the new slot: %v (CanRead %v)", err, r.CanRead())
			}
			j := c.journal()
			if last := j[len(j)-1]; last.Operation != "key add" || last.Result != repo.ResultOK {
				t.Errorf("journal = %+v", j)
			}
		})
	}
}

func TestKeyAddText(t *testing.T) {
	c := newCLIEnv(t, "env:K1")
	c.mustInit()
	r := c.pentym("key", "add", "--key", "env:K3", "--label", "ops")
	if r.code != msg.ExitOK || !regexp.MustCompile(`^✓ added full passphrase slot [0-9a-f]{16} \(ops\)\n$`).MatchString(r.stderr) ||
		!strings.HasPrefix(r.stdout, "SLOT") || strings.Count(r.stdout, "\n") != 2 {
		t.Errorf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
}

// TestKeyAddInteractive: in a terminal, the unlock passphrase and the new
// one are asked for.
func TestKeyAddInteractive(t *testing.T) {
	c := newCLIEnv(t, "")
	c.mustInit()
	r := c.run(pass1+"\nnew-secret\nnew-secret\n", true, "key", "add", "--label", "laptop")
	if r.code != msg.ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, p := range []string{
		"passphrase, recovery code or age identity for the repository: \n",
		"passphrase for the new key slot: \n", "passphrase for the new key slot again: \n",
	} {
		if !strings.Contains(r.stderr, p) {
			t.Errorf("no prompt %q in:\n%s", p, r.stderr)
		}
	}
	if strings.Contains(r.stdout+r.stderr, "new-secret") || strings.Contains(r.stdout+r.stderr, pass1) {
		t.Errorf("a passphrase was echoed:\n%s%s", r.stdout, r.stderr)
	}
	if _, s, err := c.unlock("new-secret", repo.UnlockOptions{NeedFull: true}); err != nil || s.Label != "laptop" {
		t.Errorf("unlock with the new passphrase: %v", err)
	}
}

func TestKeyAddRefusals(t *testing.T) {
	tests := []struct {
		name    string
		key     string // repository.key
		args    []string
		code    msg.ExitCode
		id      msg.ID
		message string
	}{
		{"no terminal for the new passphrase", "env:K1", nil, msg.ExitUsage, "",
			"no terminal to ask in; give --key instead"},
		{"no way to unlock", "", []string{"--key", "env:K3"}, msg.ExitConfig, idNothingToUnlock,
			"cannot unlock the repository: no terminal to ask for a passphrase, and no repository.key in the config"},
		{"unlocked with the agent slot", "env:KA", []string{"--key", "env:K3"}, msg.ExitAccess, crypto.IDUnlockFailed,
			"no full key slot unlocked with this passphrase or recovery code (2 tried)"},
		{"wrong passphrase", "env:K3", []string{"--key", "env:K3"}, msg.ExitAccess, crypto.IDUnlockFailed,
			"no full key slot unlocked with this passphrase or recovery code (2 tried)"},
		{"both kinds", "env:K1", []string{"--key", "env:K3", "--age-recipient", "age1x"}, msg.ExitUsage, "",
			"--key and --age-recipient cannot be used together"},
		{"plaintext", "env:K1", []string{"--key", "hunter2-plain"}, msg.ExitUsage, "",
			"--key takes a secret reference such as env:NAME or file:/path; a passphrase must never be written on the command line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCLIEnv(t, "")
			c.mustInit()
			c.setKey(tt.key)
			r := c.pentym(append([]string{"--json", "key", "add"}, tt.args...)...)
			doc := errorDoc(t, r)
			if r.code != tt.code || doc.ID != tt.id || doc.Message != tt.message || doc.Next == "" {
				t.Errorf("exit %d, %+v\nwant exit %d, %s %q", r.code, doc, tt.code, tt.id, tt.message)
			}
			if strings.Contains(r.stdout+r.stderr, "hunter2") {
				t.Error("the passphrase was printed")
			}
			if slots, err := c.open().Slots(t.Context()); err != nil || len(slots) != 3 {
				t.Errorf("slots after a refusal: %d, %v", len(slots), err)
			}
		})
	}
}

// TestKeySlotFlag: --slot unlocks with one slot only.
func TestKeySlotFlag(t *testing.T) {
	c := newCLIEnv(t, "env:K2")
	res := c.mustInit()
	if r := c.pentym("key", "add", "--key", "env:K3", "--slot", res.Slots[1].ID); r.code != msg.ExitOK {
		t.Errorf("--slot with its passphrase: exit %d, %s", r.code, r.stderr)
	}
	r := c.pentym("--json", "key", "add", "--key", "env:K3", "--slot", res.Slots[0].ID)
	if doc := errorDoc(t, r); r.code != msg.ExitAccess || doc.Message != "key slot "+res.Slots[0].ID+" (passphrase 1) did not unlock: wrong passphrase" {
		t.Errorf("--slot with another slot's passphrase: exit %d, %+v", r.code, doc)
	}
	r = c.pentym("--json", "key", "add", "--key", "env:K3", "--slot", res.Slots[2].ID)
	if doc := errorDoc(t, r); r.code != msg.ExitAccess || !strings.Contains(doc.Message, "is an agent slot; this needs a full slot") {
		t.Errorf("--slot naming the agent slot: exit %d, %+v", r.code, doc)
	}
}

func TestKeyRemove(t *testing.T) {
	c := newCLIEnv(t, "env:K1")
	res := c.mustInit()
	full1, full2, agent := res.Slots[0].ID, res.Slots[1].ID, res.Slots[2].ID

	r := c.pentym("key", "remove", full2)
	if r.code != msg.ExitOK || r.stdout != full2+"\n" || r.stderr != "✓ removed key slot "+full2+"\n" {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if _, _, err := c.unlock(pass2, repo.UnlockOptions{}); err == nil {
		t.Error("the removed slot's passphrase still unlocks")
	}
	r = c.pentym("--json", "key", "remove", agent)
	if r.code != msg.ExitOK || !strings.Contains(r.stdout, `"removed": "`+agent+`"`) {
		t.Errorf("remove the agent slot: exit %d\n%s", r.code, r.stdout)
	}

	// The last full slot stays.
	r = c.pentym("--json", "key", "remove", full1)
	if doc := errorDoc(t, r); r.code != msg.ExitAccess || doc.ID != crypto.IDLastFullSlot {
		t.Errorf("remove the last full slot: exit %d, %+v", r.code, doc)
	}
	if _, _, err := c.unlock(pass1, repo.UnlockOptions{NeedFull: true}); err != nil {
		t.Errorf("the last full slot was removed: %v", err)
	}
	r = c.pentym("--json", "key", "remove", "0011223344556677")
	if doc := errorDoc(t, r); r.code != msg.ExitNotFound || doc.Message != "no key slot 0011223344556677" {
		t.Errorf("remove an unknown slot: exit %d, %+v", r.code, doc)
	}

	ops := []string{}
	for _, e := range c.journal() {
		ops = append(ops, e.Operation+" "+e.Result+" "+e.Credential)
	}
	want := []string{"init ok env:K1", "key remove ok env:K1", "key remove ok env:K1"}
	if strings.Join(ops, "\n") != strings.Join(want, "\n") {
		t.Errorf("journal:\n%s\nwant:\n%s", strings.Join(ops, "\n"), strings.Join(want, "\n"))
	}
}

func TestKeyRemoveNeedsAFullSlot(t *testing.T) {
	c := newCLIEnv(t, "env:KA")
	res := c.mustInit()
	r := c.pentym("key", "remove", res.Slots[1].ID)
	if r.code != msg.ExitAccess {
		t.Errorf("remove with the agent passphrase: exit %d", r.code)
	}
	if slots, _ := c.open().Slots(t.Context()); len(slots) != 3 {
		t.Errorf("%d slots left", len(slots))
	}
}

func TestKeyExportPaper(t *testing.T) {
	c := newCLIEnv(t, "env:K1")
	c.mustInit()
	r := c.pentym("--json", "key", "export", "--paper")
	var doc struct {
		Slot      slotInfo `json:"slot"`
		PaperCode string   `json:"paper_code"`
	}
	if r.code != msg.ExitOK || json.Unmarshal([]byte(r.stdout), &doc) != nil || !paperCodeRE.MatchString(doc.PaperCode) ||
		doc.Slot.Kind != "paper" || doc.Slot.Scope != "full" {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if _, s, err := c.unlock(doc.PaperCode, repo.UnlockOptions{NeedFull: true}); err != nil || s.ID != doc.Slot.ID {
		t.Errorf("unlock with the paper code: %v", err)
	}
	j := c.journal()
	if last := j[len(j)-1]; last.Operation != "key export --paper" || strings.Contains(fmtEntries(j), doc.PaperCode) {
		t.Errorf("journal = %+v", j)
	}

	r = c.pentym("key", "export", "--paper")
	if r.code != msg.ExitOK || !regexp.MustCompile(`\npaper recovery code: [A-Z2-7-]{47}\n$`).MatchString(r.stdout) {
		t.Errorf("text: exit %d\nstdout: %s", r.code, r.stdout)
	}

	r = c.pentym("key", "export")
	if r.code != msg.ExitUsage || !strings.Contains(r.stderr, "pentym key export needs --paper") {
		t.Errorf("export without --paper: exit %d, %s", r.code, r.stderr)
	}
}

// TestSecretLeakKeys runs init and the key commands with canary
// passphrases, in every output mode and through failures, and scans the
// output and every byte stored in the repository.
func TestSecretLeakKeys(t *testing.T) {
	canaries := leaktest.New(t, 4)
	var outputs []string
	for _, mode := range [][]string{{"-v", "--debug"}, {"--json"}, {"--color=always", "--ascii"}} {
		c := newCLIEnv(t, "env:K1")
		c.env["K1"], c.env["K2"], c.env["KA"], c.env["K3"] = string(canaries[0]), string(canaries[1]), string(canaries[2]), string(canaries[3])
		runs := [][]string{
			{"init", "--key", "env:K1", "--key", "env:K2", "--agent-key", "env:KA"},
			{"init", "--key", "env:K1", "--key", "env:K2", "--agent-key", "env:KA"}, // exists
			{"key", "list"},
			{"key", "add", "--key", "env:K3"},
			{"key", "add", "--key", "env:K3", "--agent"},
			{"key", "export", "--paper"},
			{"key", "remove", "0011223344556677"},
		}
		for _, args := range runs {
			r := c.pentym(append(append([]string{}, mode...), args...)...)
			outputs = append(outputs, r.stdout, r.stderr)
		}
		// Failures: a wrong passphrase, and an agent passphrase where a
		// full slot is needed.
		for _, key := range []string{"env:K3", "env:KA"} {
			c.env["K3"] = string(canaries[3]) + "-wrong"
			c.setKey(key)
			r := c.pentym(append(append([]string{}, mode...), "key", "add", "--key", "env:K2")...)
			outputs = append(outputs, r.stdout, r.stderr)
		}
		outputs = append(outputs, fmtEntries(c.journal()))
		_ = filepath.WalkDir(c.repoDir, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				b, _ := os.ReadFile(p) // #nosec G304 G122 -- a file in the test repository
				outputs = append(outputs, string(b))
			}
			return nil
		})
	}
	leaktest.Scan(t, "init and key commands", canaries, outputs...)
}
