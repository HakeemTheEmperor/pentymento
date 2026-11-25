// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/backend/local"
	"github.com/HakeemTheEmperor/pentymento/internal/cliout"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

func TestMain(m *testing.M) {
	// New slots use the cheapest Argon2id parameters a slot accepts, so the
	// tests stay fast; the default parameters are tested in crypto.
	slotKDF = crypto.KDFParams{Name: "argon2id", MemoryKiB: 8 << 10, Time: 1, Threads: 1}
	os.Exit(m.Run())
}

// Passphrases in the test environment.
const (
	pass1     = "first passphrase"
	pass2     = "second passphrase"
	passAgent = "agent passphrase"
	pass3     = "third passphrase"
)

// cliEnv is a config file pointing at a repository directory, and an
// environment holding passphrases.
type cliEnv struct {
	t       *testing.T
	repoDir string
	config  string
	env     map[string]string
}

// newCLIEnv writes a config whose repository.key is key (none if empty).
func newCLIEnv(t *testing.T, key string) *cliEnv {
	t.Helper()
	dir := t.TempDir()
	c := &cliEnv{
		t: t, repoDir: filepath.Join(dir, "repo"), config: filepath.Join(dir, "config.yaml"),
		env: map[string]string{"LANG": "en_US.UTF-8", "K1": pass1, "K2": pass2, "KA": passAgent, "K3": pass3},
	}
	c.setKey(key)
	return c
}

// setKey rewrites the config with repository.key set to key.
func (c *cliEnv) setKey(key string) {
	c.t.Helper()
	cfg := "repository:\n  path: '" + c.repoDir + "'\n"
	if key != "" {
		cfg += "  key: " + key + "\n"
	}
	if err := os.WriteFile(c.config, []byte(cfg), 0o600); err != nil {
		c.t.Fatal(err)
	}
}

// run runs pentym with the config; stdin feeds prompts, which work when
// interactive is set.
func (c *cliEnv) run(stdin string, interactive bool, args ...string) result {
	c.t.Helper()
	var stdout, stderr bytes.Buffer
	a := newApp(strings.NewReader(stdin), &stdout, &stderr, func(k string) string { return c.env[k] })
	a.interactive = interactive
	code := a.execute(context.Background(), append([]string{"--config", c.config}, args...))
	return result{code, stdout.String(), stderr.String()}
}

func (c *cliEnv) pentym(args ...string) result {
	c.t.Helper()
	return c.run("", false, args...)
}

// mustInit creates a repository with two passphrase slots and an agent
// slot.
func (c *cliEnv) mustInit() initResult {
	c.t.Helper()
	r := c.pentym("--json", "init", "--key", "env:K1", "--key", "env:K2", "--agent-key", "env:KA")
	if r.code != msg.ExitOK {
		c.t.Fatalf("init: exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	var res initResult
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil {
		c.t.Fatalf("init --json: %v\n%s", err, r.stdout)
	}
	return res
}

// open opens the repository directly.
func (c *cliEnv) open() *repo.Repository {
	c.t.Helper()
	be, err := local.New(c.repoDir)
	if err != nil {
		c.t.Fatal(err)
	}
	r, err := repo.Open(context.Background(), be)
	if err != nil {
		c.t.Fatal(err)
	}
	return r
}

// unlock opens the repository and unlocks it with secret.
func (c *cliEnv) unlock(secret string, opts repo.UnlockOptions) (*repo.Repository, *crypto.Slot, error) {
	c.t.Helper()
	r := c.open()
	_, s, err := r.UnlockWith(context.Background(), secrets.New([]byte(secret)), opts)
	return r, s, err
}

// journal returns the operations journaled, unlocking with the passphrase
// in K1.
func (c *cliEnv) journal() []repo.JournalEntry {
	c.t.Helper()
	r, _, err := c.unlock(c.env["K1"], repo.UnlockOptions{})
	if err != nil {
		c.t.Fatal(err)
	}
	entries, err := r.Journal(context.Background(), time.Time{}, "")
	if err != nil {
		c.t.Fatal(err)
	}
	return entries
}

// hasRepository reports whether a repository was created.
func (c *cliEnv) hasRepository() bool {
	_, err := os.Stat(filepath.Join(c.repoDir, "config"))
	return err == nil
}

// errorDoc parses a --json error document.
func errorDoc(t *testing.T, r result) cliout.ErrorBody {
	t.Helper()
	var doc cliout.ErrorDoc
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("not a JSON error document: %v\nstdout: %s\nstderr: %s", err, r.stdout, r.stderr)
	}
	return doc.Error
}

func TestInit(t *testing.T) {
	c := newCLIEnv(t, "")
	res := c.mustInit()
	if res.Encryption != repo.EncryptionOn || !res.ConditionalCreate || res.Location != c.repoDir || len(res.Repository) != 32 || res.PaperCode != "" {
		t.Errorf("init result = %+v", res)
	}
	want := []struct{ kind, scope, label string }{
		{"passphrase", "full", "passphrase 1"}, {"passphrase", "full", "passphrase 2"}, {"passphrase", "agent", "agent"},
	}
	if len(res.Slots) != len(want) {
		t.Fatalf("slots = %+v", res.Slots)
	}
	for i, w := range want {
		if s := res.Slots[i]; s.Kind != w.kind || s.Scope != w.scope || s.Label != w.label {
			t.Errorf("slot %d = %+v, want %+v", i, s, w)
		}
	}

	// Each full passphrase unlocks everything; the agent passphrase only
	// the agent slot, which cannot read data.
	for _, tt := range []struct {
		pass    string
		canRead bool
	}{{pass1, true}, {pass2, true}, {passAgent, false}} {
		r, _, err := c.unlock(tt.pass, repo.UnlockOptions{})
		if err != nil || r.CanRead() != tt.canRead {
			t.Errorf("unlock with %q: CanRead %v, %v; want %v", tt.pass, r.CanRead(), err, tt.canRead)
		}
	}
	if _, _, err := c.unlock(passAgent, repo.UnlockOptions{NeedFull: true}); err == nil {
		t.Error("the agent passphrase unlocked a full slot")
	}

	j := c.journal()
	if len(j) != 1 || j[0].Operation != "init" || j[0].Result != repo.ResultOK || j[0].Credential != "env:K1" {
		t.Errorf("journal = %+v", j)
	}
}

func TestInitText(t *testing.T) {
	c := newCLIEnv(t, "")
	r := c.pentym("init", "--key", "env:K1", "--key", "env:K2", "--agent-key", "env:KA")
	if r.code != msg.ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if !strings.HasPrefix(r.stderr, "✓ created repository ") || !strings.HasSuffix(r.stderr, " at "+c.repoDir+"\n") {
		t.Errorf("stderr = %q", r.stderr)
	}
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if len(lines) != 4 || !regexp.MustCompile(`^SLOT +KIND +SCOPE +LABEL +CREATED$`).MatchString(lines[0]) ||
		!regexp.MustCompile(`^[0-9a-f]{16}  passphrase  agent  agent `).MatchString(lines[3]) {
		t.Errorf("stdout:\n%s", r.stdout)
	}

	// Quiet prints only the result.
	q := newCLIEnv(t, "")
	r = q.pentym("-q", "init", "--key", "env:K1", "--key", "env:K2", "--agent-key", "env:KA")
	if r.code != msg.ExitOK || r.stderr != "" || !strings.HasPrefix(r.stdout, "SLOT") {
		t.Errorf("-q: exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
}

var paperCodeRE = regexp.MustCompile(`^[A-Z2-7]{5}(-[A-Z2-7]{5}){7}$`)

func TestInitPaper(t *testing.T) {
	c := newCLIEnv(t, "")
	r := c.pentym("--json", "init", "--key", "env:K1", "--paper", "--agent-key", "env:KA")
	if r.code != msg.ExitOK {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	var res initResult
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil {
		t.Fatal(err)
	}
	if !paperCodeRE.MatchString(res.PaperCode) || len(res.Slots) != 3 || res.Slots[1].Kind != "paper" || res.Slots[1].Scope != "full" {
		t.Fatalf("init --paper = %+v", res)
	}
	if rr, s, err := c.unlock(res.PaperCode, repo.UnlockOptions{NeedFull: true}); err != nil || s.Kind != crypto.KindPaper || !rr.CanRead() {
		t.Errorf("unlock with the paper code: %v", err)
	}

	// In text, the code is on stdout and the reminder on stderr.
	c = newCLIEnv(t, "")
	r = c.pentym("init", "--key", "env:K1", "--paper", "--agent-key", "env:KA")
	m := regexp.MustCompile(`\npaper recovery code: (\S+)\n$`).FindStringSubmatch(r.stdout)
	if r.code != msg.ExitOK || m == nil || !paperCodeRE.MatchString(m[1]) {
		t.Fatalf("exit %d\nstdout: %s", r.code, r.stdout)
	}
	if !strings.Contains(r.stderr, "! write the paper recovery code down now: it is shown only once and stored nowhere\n") || strings.Contains(r.stderr, m[1]) {
		t.Errorf("stderr = %q", r.stderr)
	}
	if strings.Contains(fmtEntries(c.journal()), m[1]) {
		t.Error("the paper code is in the journal")
	}
}

func TestInitAge(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	c := newCLIEnv(t, "")
	r := c.pentym("--json", "init", "--key", "env:K1", "--age-recipient", id.Recipient().String(), "--agent-key", "env:KA")
	if r.code != msg.ExitOK {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if _, s, err := c.unlock(id.String(), repo.UnlockOptions{NeedFull: true}); err != nil || s.Kind != crypto.KindAge || s.Recipient != id.Recipient().String() {
		t.Errorf("unlock with the age identity: %v", err)
	}
}

func TestInitInteractive(t *testing.T) {
	t.Run("two passphrases", func(t *testing.T) {
		c := newCLIEnv(t, "")
		r := c.run("uno-secret\nuno-secret\ndos-secret\ndos-secret\nagent-secret\nagent-secret\n", true, "init")
		if r.code != msg.ExitOK {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		for _, p := range []string{
			"passphrase for the first full key slot: \n",
			"passphrase for the first full key slot again: \n",
			"second passphrase (leave empty to print a paper recovery code instead): \n",
			"second passphrase again: \n",
			"passphrase for the agent slot: \n",
		} {
			if !strings.Contains(r.stderr, p) {
				t.Errorf("stderr has no prompt %q:\n%s", p, r.stderr)
			}
		}
		if strings.Contains(r.stdout+r.stderr, "-secret") {
			t.Errorf("a passphrase was echoed:\n%s%s", r.stdout, r.stderr)
		}
		if _, s, err := c.unlock("dos-secret", repo.UnlockOptions{NeedFull: true}); err != nil || s.Label != "passphrase 2" {
			t.Errorf("unlock with the second passphrase: %v", err)
		}
		if _, s, err := c.unlock("agent-secret", repo.UnlockOptions{}); err != nil || s.Scope != crypto.ScopeAgent {
			t.Errorf("unlock with the agent passphrase: %v", err)
		}
	})
	t.Run("one passphrase and a paper code", func(t *testing.T) {
		c := newCLIEnv(t, "")
		r := c.run("one\none\n\nagent\nagent\n", true, "init")
		if r.code != msg.ExitOK || !strings.Contains(r.stdout, "paper recovery code: ") {
			t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
		}
	})
	t.Run("flags fill in what is given", func(t *testing.T) {
		c := newCLIEnv(t, "")
		r := c.run("two\ntwo\n", true, "init", "--key", "env:K1", "--agent-key", "env:KA")
		if r.code != msg.ExitOK || strings.Contains(r.stderr, "first full key slot") || strings.Contains(r.stderr, "agent slot:") {
			t.Fatalf("exit %d\nstderr: %s", r.code, r.stderr)
		}
	})
	t.Run("entries that differ", func(t *testing.T) {
		c := newCLIEnv(t, "")
		r := c.run("one\nonf\n", true, "init")
		if r.code != msg.ExitUsage || !strings.Contains(r.stderr, "✗ the two passphrase for the first full key slot entries do not match\n") || c.hasRepository() {
			t.Fatalf("exit %d\nstderr: %s", r.code, r.stderr)
		}
	})
}

func TestInitRefusals(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		code    msg.ExitCode
		id      msg.ID
		message string
	}{
		{"one full slot", []string{"--key", "env:K1", "--agent-key", "env:KA"}, msg.ExitUsage, idTooFewSlots,
			"init needs two full key slots, or one and a paper recovery code; got 1"},
		{"a paper code alone", []string{"--paper", "--agent-key", "env:KA"}, msg.ExitUsage, idTooFewSlots,
			"init needs two full key slots, or one and a paper recovery code; got 1"},
		{"no slots at all", nil, msg.ExitUsage, idTooFewSlots,
			"init needs two full key slots, or one and a paper recovery code; got 0"},
		{"no agent slot", []string{"--key", "env:K1", "--key", "env:K2"}, msg.ExitUsage, "",
			"init needs the agent slot's passphrase; give --agent-key with a reference such as env:NAME"},
		{"the same passphrase twice", []string{"--key", "env:K1", "--key", "env:K1", "--agent-key", "env:KA"}, msg.ExitUsage, "",
			"two full key slots have the same passphrase; a second slot is only a backup if its passphrase differs"},
		{"agent passphrase reused", []string{"--key", "env:K1", "--key", "env:K2", "--agent-key", "env:K2"}, msg.ExitUsage, "",
			"the agent slot's passphrase is the same as a full slot's; it must differ, or a database host holding it could unlock the full slot"},
		{"passphrase on the command line", []string{"--key", "hunter2-on-the-command-line", "--key", "env:K2", "--agent-key", "env:KA"}, msg.ExitUsage, "",
			"--key takes a secret reference such as env:NAME or file:/path; a passphrase must never be written on the command line"},
		{"agent passphrase on the command line", []string{"--key", "env:K1", "--key", "env:K2", "--agent-key", "hunter2-on-the-command-line"}, msg.ExitUsage, "",
			"--agent-key takes a secret reference such as env:NAME or file:/path; a passphrase must never be written on the command line"},
		{"unset variable", []string{"--key", "env:MISSING", "--key", "env:K2", "--agent-key", "env:KA"}, msg.ExitConfig, "PTM-1004",
			"secret reference env:MISSING: environment variable MISSING is not set"},
		{"bad age recipient", []string{"--key", "env:K1", "--age-recipient", "age1nope", "--agent-key", "env:KA"}, msg.ExitUsage, "",
			`--age-recipient "age1nope" is not an age X25519 recipient (age1…)`},
		{"insecure with slots", []string{"--insecure-test-mode", "--key", "env:K1"}, msg.ExitUsage, "",
			"--insecure-test-mode creates no key slots, so it takes no --key, --age-recipient, --paper or --agent-key"},
		{"role printing", []string{"--print-sql"}, msg.ExitNotImplemented, "",
			"--print-sql and --print-mongo are not implemented in this build"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCLIEnv(t, "")
			r := c.pentym(append([]string{"--json", "init"}, tt.args...)...)
			doc := errorDoc(t, r)
			if r.code != tt.code || doc.ID != tt.id || doc.Message != tt.message || doc.Next == "" {
				t.Errorf("exit %d, %+v\nwant exit %d, %s %q", r.code, doc, tt.code, tt.id, tt.message)
			}
			if strings.Contains(r.stdout+r.stderr, "hunter2") {
				t.Errorf("a passphrase given on the command line was printed:\n%s%s", r.stdout, r.stderr)
			}
			if c.hasRepository() {
				t.Error("a repository was created")
			}
		})
	}
}

func TestInitExisting(t *testing.T) {
	c := newCLIEnv(t, "")
	first := c.mustInit()
	r := c.pentym("--json", "init", "--key", "env:K1", "--key", "env:K3", "--agent-key", "env:KA")
	if doc := errorDoc(t, r); r.code != msg.ExitFailure || doc.ID != repo.IDExists {
		t.Fatalf("second init: exit %d, %+v", r.code, doc)
	}
	slots, err := c.open().Slots(context.Background())
	if err != nil || len(slots) != len(first.Slots) {
		t.Errorf("second init changed the slots: %d, %v", len(slots), err)
	}
	if _, _, err := c.unlock(pass3, repo.UnlockOptions{}); err == nil {
		t.Error("the second init's passphrase unlocks the repository")
	}
}

func TestInitNotConfigured(t *testing.T) {
	c := newCLIEnv(t, "")
	if err := os.WriteFile(c.config, []byte("targets: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--key", "env:K1", "--key", "env:K2", "--agent-key", "env:KA"}, {"key", "list"}} {
		r := c.pentym(append([]string{"--json"}, args...)...)
		if doc := errorDoc(t, r); r.code != msg.ExitConfig || doc.ID != idRepoNotConfigured || doc.Message != "no repository location in "+c.config {
			t.Errorf("%v: exit %d, %+v", args, r.code, doc)
		}
	}
}

func TestInitInsecure(t *testing.T) {
	c := newCLIEnv(t, "")
	r := c.pentym("--json", "init", "--insecure-test-mode")
	var res initResult
	if r.code != msg.ExitOK || json.Unmarshal([]byte(r.stdout), &res) != nil || res.Encryption != repo.EncryptionInsecure || len(res.Slots) != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if !c.open().Config().Insecure() {
		t.Error("the repository is encrypted")
	}
	r = c.pentym("init", "--insecure-test-mode")
	if r.code != msg.ExitFailure {
		t.Errorf("second init: exit %d", r.code)
	}

	c = newCLIEnv(t, "")
	r = c.pentym("init", "--insecure-test-mode")
	if r.code != msg.ExitOK || !strings.Contains(r.stderr, "! the repository is not encrypted (--insecure-test-mode)\n") || r.stdout != "" {
		t.Errorf("text: exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if r := c.pentym("key", "add", "--key", "env:K1"); r.code != msg.ExitFailure || !strings.Contains(r.stderr, "has no key slots") {
		t.Errorf("key add on an insecure repository: exit %d, %s", r.code, r.stderr)
	}
}

// overwriting ignores IfNotExists, like storage without conditional
// create.
type overwriting struct{ backend.Backend }

func (o overwriting) Put(ctx context.Context, key string, r io.Reader, _ backend.PutOptions) error {
	return o.Backend.Put(ctx, key, r, backend.PutOptions{})
}

// TestInitWarnsWithoutConditionalCreate: init still creates the
// repository, and warns.
func TestInitWarnsWithoutConditionalCreate(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		be, err := local.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		a := newApp(nil, &stdout, &stderr, nil)
		a.opts.json = jsonOut
		a.printer = &cliout.Printer{Stdout: &stdout, Stderr: &stderr, JSON: jsonOut, Unicode: true, Verbosity: cliout.Verbose}
		if err := a.createRepo(context.Background(), overwriting{be}, "/repo", true, nil, nil, secrets.Secret{}, secrets.Ref{}); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(stderr.String(), "! the storage does not honour conditional create: creating the same object twice succeeded (PTM-3005)\n") {
			t.Errorf("stderr = %q", stderr.String())
		}
		if jsonOut && !strings.Contains(stdout.String(), `"conditional_create": false`) {
			t.Errorf("stdout = %s", stdout.String())
		}
	}
}

// fmtEntries renders journal entries for searching.
func fmtEntries(entries []repo.JournalEntry) string {
	b, _ := json.Marshal(entries)
	return string(b)
}
