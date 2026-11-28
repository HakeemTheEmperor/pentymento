// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

func TestParseTable(t *testing.T) {
	for in, want := range map[string]qualified{
		"orders": {"public", "orders"}, "billing.invoices": {"billing", "invoices"}, "Orders": {"public", "Orders"},
	} {
		if got, err := parseTable(in); err != nil || got != want {
			t.Errorf("parseTable(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", ".", "a.", ".b", "a.b.c"} {
		if _, err := parseTable(in); err == nil {
			t.Errorf("parseTable(%q) succeeded", in)
		}
	}
}

func TestWithSuffix(t *testing.T) {
	if got := withSuffix("orders", "_recovered_20261122"); got != "orders_recovered_20261122" {
		t.Errorf("withSuffix = %q", got)
	}
	long := strings.Repeat("a", 60)
	if got := withSuffix(long, "_recovered_20261122_2"); len(got) != maxIdentifier || !strings.HasSuffix(got, "_recovered_20261122_2") {
		t.Errorf("a long name became %q (%d bytes)", got, len(got))
	}
	// Multibyte characters are not cut in half.
	got := withSuffix(strings.Repeat("é", 30), "_recovered_20261122")
	if len(got) > maxIdentifier || !strings.HasSuffix(got, "_recovered_20261122") || strings.ContainsRune(got, '�') {
		t.Errorf("multibyte: %q", got)
	}
	for _, r := range strings.TrimSuffix(got, "_recovered_20261122") {
		if r != 'é' {
			t.Errorf("a character was cut: %q", got)
		}
	}
}

func TestQuote(t *testing.T) {
	if quoteIdent(`we"ird`) != `"we""ird"` || quoteLiteral("it's") != `'it''s'` {
		t.Error("quoting is wrong")
	}
	if q := (qualified{"billing", "invoices"}).sql(); q != `"billing"."invoices"` {
		t.Errorf("sql = %s", q)
	}
}

func TestExtractNeedsRestoreRole(t *testing.T) {
	tg := engine.Target{Name: "prod-pg", Config: config.Target{Engine: "postgres", Credentials: config.Credentials{
		Backup: config.Credential{User: "pentym_backup"}}}}
	_, err := (&Engine{}).Extract(context.Background(), nil, tg, engine.ExtractRequest{Objects: []string{"orders"}, Suffix: "_x"})
	var me *msg.Error
	if !errors.As(err, &me) || me.Code != msg.ExitConfig || !strings.Contains(me.Message.Fact, "credentials.restore.user") {
		t.Errorf("Extract without a restore role = %v", err)
	}
}
