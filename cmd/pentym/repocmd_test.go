// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// runWithPlan runs pentym with a replaced upgrade plan: one test migration
// from the current format.
func (c *cliEnv) runWithPlan(stdin string, interactive bool, ran *int, args ...string) result {
	c.t.Helper()
	var stdout, stderr bytes.Buffer
	a := newApp(strings.NewReader(stdin), &stdout, &stderr, func(k string) string { return c.env[k] })
	a.interactive = interactive
	a.upgradePlan = func(*repo.Repository) ([]repo.Migration, error) {
		return []repo.Migration{{From: repo.FormatVersion, Describe: "test step", Run: func(context.Context, *repo.Repository) error {
			*ran++
			return nil
		}}}, nil
	}
	code := a.execute(context.Background(), append([]string{"--config", c.config}, args...))
	return result{code, stdout.String(), stderr.String()}
}

func TestRepoUpgradeCurrent(t *testing.T) {
	c := newCLIEnv(t, "env:K1")
	c.mustInit()
	r := c.pentym("repo", "upgrade")
	if r.code != msg.ExitOK || r.stderr != "✓ repository format 1 is current; nothing to upgrade\n" || r.stdout != "" {
		t.Errorf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	r = c.pentym("--json", "repo", "upgrade")
	var res struct {
		From, To int
		Upgraded bool
		Steps    []string
	}
	if r.code != msg.ExitOK || json.Unmarshal([]byte(r.stdout), &res) != nil || res.From != 1 || res.To != 1 || res.Upgraded || res.Steps == nil {
		t.Errorf("--json: exit %d, %s", r.code, r.stdout)
	}
	j := c.journal()
	if last := j[len(j)-1]; last.Operation != "repo upgrade" || last.Result != repo.ResultSkipped {
		t.Errorf("journal: %+v", last)
	}
	// The lock is released afterwards.
	if objs := c.objects("locks/exclusive-"); len(objs) != 0 {
		t.Errorf("locks left: %v", objs)
	}
}

func TestRepoUpgradeConfirmation(t *testing.T) {
	t.Run("declined", func(t *testing.T) {
		c := newCLIEnv(t, "env:K1")
		c.mustInit()
		ran := 0
		r := c.runWithPlan("n\n", true, &ran, "repo", "upgrade")
		if r.code != msg.ExitDeclined || ran != 0 || !strings.Contains(r.stderr, "! upgrade format 1 → 2: test step\n") ||
			!strings.Contains(r.stderr, "continue? [y/N] ") || !strings.Contains(r.stderr, "✗ upgrade declined; nothing was changed\n") {
			t.Fatalf("exit %d, ran %d\nstderr: %s", r.code, ran, r.stderr)
		}
		if c.open().Config().Version != repo.FormatVersion {
			t.Error("the format changed")
		}
		j := c.journal()
		if last := j[len(j)-1]; last.Operation != "repo upgrade" || last.Result != repo.ResultSkipped || !strings.Contains(last.Detail, "declined") {
			t.Errorf("journal: %+v", last)
		}
	})
	t.Run("accepted", func(t *testing.T) {
		c := newCLIEnv(t, "env:K1")
		c.mustInit()
		ran := 0
		r := c.runWithPlan("y\n", true, &ran, "repo", "upgrade")
		if r.code != msg.ExitOK || ran != 1 || !strings.Contains(r.stderr, "✓ upgraded the repository from format 1 to 2\n") {
			t.Fatalf("exit %d, ran %d\nstderr: %s", r.code, ran, r.stderr)
		}
		// This pentym now refuses the newer format, as every older one must.
		if r := c.pentym("--json", "key", "list"); errorDoc(t, r).ID != repo.IDFormatNewer {
			t.Errorf("key list after the upgrade: exit %d", r.code)
		}
	})
	t.Run("without a terminal", func(t *testing.T) {
		c := newCLIEnv(t, "env:K1")
		c.mustInit()
		ran := 0
		r := c.runWithPlan("", false, &ran, "--json", "repo", "upgrade")
		if r.code != msg.ExitOK || ran != 1 || !strings.Contains(r.stdout, `"upgraded": true`) || !strings.Contains(r.stdout, "format 1 → 2: test step") {
			t.Fatalf("exit %d, ran %d\nstdout: %s\nstderr: %s", r.code, ran, r.stdout, r.stderr)
		}
	})
}

func TestRepoRebuildIndexCommand(t *testing.T) {
	c := newCLIEnv(t, "env:KA") // an agent slot is enough
	c.mustInit()
	c.addSnapshot("prod-pg")
	c.addSnapshot("prod-mg")
	for _, k := range c.objects("index/") {
		if err := os.Remove(c.objectPath(k)); err != nil {
			t.Fatal(err)
		}
	}
	r := c.pentym("repo", "rebuild-index")
	if r.code != msg.ExitOK || !strings.HasPrefix(r.stderr, "✓ rebuilt the index from 2 packs (") || !strings.HasSuffix(r.stderr, "1 index files written, 0 removed\n") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	c.setKey("env:K1")
	if rep := checkReport(t, c.pentym("--json", "check", "--full")); len(rep.Problems) != 0 {
		t.Errorf("check after the rebuild: %+v", rep.Problems)
	}
	j := c.journal()
	ops := []string{}
	for _, e := range j {
		ops = append(ops, e.Operation)
	}
	if !contains(ops, "repo rebuild-index") {
		t.Errorf("journal: %v", ops)
	}
}

func TestRepoRebuildIndexUnreadablePack(t *testing.T) {
	c := newCLIEnv(t, "env:K1")
	c.mustInit()
	bad := c.addSnapshot("prod-pg")
	c.addSnapshot("prod-mg")
	b, err := os.ReadFile(c.objectPath(bad))
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-20] ^= 0xff                                              // inside the encrypted trailer
	if err := os.WriteFile(c.objectPath(bad), b, 0o600); err != nil { // #nosec G703 -- corrupting a pack in the test repository
		t.Fatal(err)
	}
	r := c.pentym("--json", "repo", "rebuild-index")
	var st repo.RebuildStats
	if r.code != msg.ExitIntegrity || r.stderr != "" || json.Unmarshal([]byte(r.stdout), &st) != nil || len(st.Unreadable) != 1 || st.Packs != 1 ||
		!strings.HasSuffix(bad, st.Unreadable[0]) {
		t.Errorf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	r = c.pentym("repo", "rebuild-index")
	if r.code != msg.ExitIntegrity || !strings.Contains(r.stderr, "✗ 1 pack could not be read; their chunks are not in the index\n") {
		t.Errorf("text: exit %d\nstderr: %s", r.code, r.stderr)
	}
}

func TestKeyRotateCommand(t *testing.T) {
	c := newCLIEnv(t, "env:K1")
	c.mustInit()
	c.addSnapshot("prod-pg")
	r := c.pentym("--json", "key", "rotate")
	var st repo.RotateStats
	if r.code != msg.ExitOK || json.Unmarshal([]byte(r.stdout), &st) != nil || st.PreviousKey != "keys/private-000001" || st.Packs != 1 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	r = c.pentym("key", "rotate")
	if r.code != msg.ExitOK || !strings.Contains(r.stderr, "the replaced private key is kept as keys/private-000002") {
		t.Errorf("text: exit %d, %s", r.code, r.stderr)
	}
	// Every slot still unlocks, and everything still reads.
	for _, k := range []string{pass1, pass2} {
		if _, _, err := c.unlock(k, repo.UnlockOptions{NeedFull: true}); err != nil {
			t.Errorf("unlock after rotation: %v", err)
		}
	}
	if rep := checkReport(t, c.pentym("--json", "check", "--full")); len(rep.Problems) != 0 {
		t.Errorf("check after rotation: %+v", rep.Problems)
	}
	j := c.journal()
	if last := j[len(j)-1]; last.Operation != "check" || j[len(j)-2].Operation != "key rotate" {
		t.Errorf("journal: %+v", j)
	}

	c.setKey("env:KA")
	if r := c.pentym("key", "rotate"); r.code != msg.ExitAccess {
		t.Errorf("rotate with the agent passphrase: exit %d", r.code)
	}
}
