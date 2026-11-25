// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/HakeemTheEmperor/pentymento/internal/devtools/leaktest"
)

type exported struct {
	Name     string
	Password Secret
	Ref      Ref
}

type unexported struct {
	name     string
	password Secret
	ref      Ref
}

// TestSecretLeakFormatting sends a Secret and a plaintext Ref through every
// formatting, encoding and logging path and checks that none reveals them.
func TestSecretLeakFormatting(t *testing.T) {
	canaries := leaktest.New(t, 2)
	s := New([]byte(canaries[0]))
	ref, err := ParseRef(string(canaries[1]))
	if err != nil || !ref.IsPlaintext() {
		t.Fatalf("ParseRef(canary) = %v, %v", ref, err)
	}
	ex := exported{Name: "db", Password: s, Ref: ref}
	un := unexported{name: "db", password: s, ref: ref}

	var outputs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T"} {
		outputs = append(outputs,
			fmt.Sprintf(verb, s), fmt.Sprintf(verb, &s), fmt.Sprintf(verb, ref),
			fmt.Sprintf(verb, ex), fmt.Sprintf(verb, &ex), fmt.Sprintf(verb, un), fmt.Sprintf(verb, &un),
			fmt.Sprintf(verb, []Secret{s}), fmt.Sprintf(verb, map[string]Secret{"k": s}),
		)
	}
	outputs = append(outputs, fmt.Sprint(s, ref, ex, un), fmt.Sprintln(s, ref), s.String(), ref.String(), s.GoString())
	outputs = append(outputs, fmt.Errorf("connect: %v, %w", s, errors.New(fmt.Sprint(ref))).Error())

	for _, v := range []any{s, &s, ref, ex, &ex, un, []Secret{s}, map[string]any{"s": s, "r": ref}} {
		j, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal(%T): %v", v, err)
		}
		y, err := yaml.Marshal(v)
		if err != nil {
			t.Fatalf("yaml.Marshal(%T): %v", v, err)
		}
		outputs = append(outputs, string(j), string(y))
	}
	if txt, err := s.MarshalText(); err != nil || string(txt) != Redacted {
		t.Errorf("MarshalText = %q, %v", txt, err)
	}

	for _, newHandler := range []func(*bytes.Buffer) slog.Handler{
		func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var buf bytes.Buffer
		log := slog.New(newHandler(&buf))
		log.Info("connect", "password", s, "ptr", &s, "ref", ref, "struct", ex, "unexported", un,
			slog.Group("g", "password", s), slog.Any("any", s))
		log.Error("failed", "err", fmt.Errorf("auth with %v", s))
		outputs = append(outputs, buf.String())
		if !strings.Contains(buf.String(), Redacted) {
			t.Errorf("log does not show %s: %s", Redacted, buf.String())
		}
	}

	leaktest.Scan(t, "formatting paths", canaries, outputs...)
}

func TestSecretExpose(t *testing.T) {
	b := []byte("hunter2")
	s := New(b)
	b[0] = 'X'
	if string(s.Expose()) != "hunter2" {
		t.Fatal("New did not copy its input")
	}
	if s.IsEmpty() || !(Secret{}).IsEmpty() {
		t.Error("IsEmpty is wrong")
	}
	s.Clear()
	if !bytes.Equal(s.Expose(), make([]byte, 7)) {
		t.Errorf("Clear left %q", s.Expose())
	}
	(Secret{}).Clear() // must not panic
}

func TestParseRef(t *testing.T) {
	tests := []struct {
		in      string
		scheme  Scheme
		str     string
		wantErr bool
	}{
		{"env:PGPASSWORD", SchemeEnv, "env:PGPASSWORD", false},
		{"file:/run/secrets/db", SchemeFile, "file:/run/secrets/db", false},
		{`file:C:\secrets\db.txt`, SchemeFile, `file:C:\secrets\db.txt`, false},
		{"keychain:pentym/prod-pg", SchemeKeychain, "keychain:pentym/prod-pg", false},
		{"keychain:svc/a/b", SchemeKeychain, "keychain:svc/a/b", false},
		{"cmd:op read op://vault/db/password", SchemeCmd, "cmd:op read op://vault/db/password", false},
		{"hunter2", SchemePlaintext, "[plaintext secret]", false},
		{"user:pass", SchemePlaintext, "[plaintext secret]", false},
		{"vault:kv/db", SchemePlaintext, "[plaintext secret]", false},
		{"ENV:X", SchemePlaintext, "[plaintext secret]", false},
		{"", "", "", true},
		{"env:", "", "", true},
		{"cmd:  ", "", "", true},
		{"keychain:nosuffix", "", "", true},
		{"keychain:/item", "", "", true},
	}
	for _, tt := range tests {
		r, err := ParseRef(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseRef(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if err == nil && (r.Scheme() != tt.scheme || r.String() != tt.str) {
			t.Errorf("ParseRef(%q) = %s %q, want %s %q", tt.in, r.Scheme(), r.String(), tt.scheme, tt.str)
		}
	}
}

func TestRefYAML(t *testing.T) {
	var v struct {
		A Ref `yaml:"a"`
		B Ref `yaml:"b"`
	}
	if err := yaml.Unmarshal([]byte("a: env:X\n\nb: hunter2\n"), &v); err != nil {
		t.Fatal(err)
	}
	if v.A.Scheme() != SchemeEnv || v.A.Line != 1 || !v.B.IsPlaintext() || v.B.Line != 3 {
		t.Errorf("decoded %+v (lines %d, %d)", v, v.A.Line, v.B.Line)
	}
	err := yaml.Unmarshal([]byte("a: [x]\n"), &v)
	if err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("list as secret: %v", err)
	}
	err = yaml.Unmarshal([]byte("a: 'env:'\n"), &v)
	if err == nil || !strings.Contains(err.Error(), "missing its argument") {
		t.Errorf("empty env reference: %v", err)
	}
	if (Ref{}).String() != "" || !(Ref{}).IsZero() {
		t.Error("zero Ref is not empty")
	}
}

func TestScrub(t *testing.T) {
	known := New([]byte("s3cr3t-value"))
	tests := []struct {
		in, want string
	}{
		{"auth failed for s3cr3t-value", "auth failed for [redacted]"},
		{"dial postgres://app:hunter2@db:5432/x: refused", "dial postgres://app:[redacted]@db:5432/x: refused"},
		{"mongodb://u:p%40ss@h1,h2/?replicaSet=rs0", "mongodb://u:[redacted]@h1,h2/?replicaSet=rs0"},
		{"host=db user=app password=hunter2 sslmode=require", "host=db user=app password=[redacted] sslmode=require"},
		{`password="two words" next`, `password=[redacted] next`},
		{"PASSWORD: hunter2", "PASSWORD: [redacted]"},
		{"token=abc&x=1", "token=[redacted]&x=1"},
		{"api_key=abc, other", "api_key=[redacted], other"},
		{"password authentication failed for user \"pentym_backup\"", "password authentication failed for user \"pentym_backup\""},
		{"https://example.com/path", "https://example.com/path"},
	}
	for _, tt := range tests {
		if got := Scrub(tt.in, known); got != tt.want {
			t.Errorf("Scrub(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	base := errors.New("login as app with s3cr3t-value failed")
	err := ScrubError(fmt.Errorf("connect: %w", base), known)
	if err.Error() != "connect: login as app with [redacted] failed" || !errors.Is(err, base) {
		t.Errorf("ScrubError = %q, Is = %v", err, errors.Is(err, base))
	}
	if ScrubError(nil) != nil {
		t.Error("ScrubError(nil) != nil")
	}
}

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"op read op://vault/db/password", []string{"op", "read", "op://vault/db/password"}, false},
		{"  vault   kv get  -field=pw secret/db ", []string{"vault", "kv", "get", "-field=pw", "secret/db"}, false},
		{`'C:\Program Files\helper.exe' --x`, []string{`C:\Program Files\helper.exe`, "--x"}, false},
		{`C:\tools\helper.exe a`, []string{`C:\tools\helper.exe`, "a"}, false},
		{`"a b" "c\"d" "e\\f" "g\h"`, []string{"a b", `c"d`, `e\f`, `g\h`}, false},
		{`x '' ""`, []string{"x", "", ""}, false},
		{`a'b c'd`, []string{"ab cd"}, false},
		{"", nil, true},
		{"   ", nil, true},
		{`'open`, nil, true},
		{`"open`, nil, true},
	}
	for _, tt := range tests {
		got, err := SplitCommand(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("SplitCommand(%q) error = %v", tt.in, err)
			continue
		}
		if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", tt.want) {
			t.Errorf("SplitCommand(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
