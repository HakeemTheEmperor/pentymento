// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package secrets

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/HakeemTheEmperor/pentymento/internal/devtools/leaktest"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// helperEnv makes the test binary act as a cmd: helper (see TestHelper);
// helperValueEnv carries the value it prints, so it is not on the command
// line (which is part of the reference and is shown in messages).
const (
	helperEnv      = "PENTYM_SECRETS_TEST_HELPER"
	helperValueEnv = "PENTYM_SECRETS_TEST_VALUE"
)

// TestHelper is not a test: when helperEnv is set, the test binary acts as
// a cmd: helper whose behaviour is chosen by helperEnv.
func TestHelper(*testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	value := os.Getenv(helperValueEnv)
	switch mode {
	case "print":
		fmt.Println(value)
	case "fail":
		fmt.Print(value)
		fmt.Fprintln(os.Stderr, "login failed for", value)
		os.Exit(3)
	case "silent":
	case "sleep":
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

// helperRef returns a cmd: reference that runs this test binary as a
// helper in the given mode, printing value.
func helperRef(t *testing.T, mode, value string) Ref {
	t.Helper()
	t.Setenv(helperEnv, mode)
	t.Setenv(helperValueEnv, value)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseRef(fmt.Sprintf("cmd:'%s' -test.run=^TestHelper$", exe))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustRef(t *testing.T, s string) Ref {
	t.Helper()
	r, err := ParseRef(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// wantFailure checks err is a config *msg.Error with the given ID and
// returns its message.
func wantFailure(t *testing.T, err error, id msg.ID, factPart string) msg.Message {
	t.Helper()
	var me *msg.Error
	if !errors.As(err, &me) {
		t.Fatalf("error %v is not a *msg.Error", err)
	}
	if m := me.Message; me.Code != msg.ExitConfig || m.ID != id || !strings.Contains(m.Fact, factPart) || m.Next == "" {
		t.Fatalf("error = %+v, want code %d, ID %s, fact containing %q and a next step", me.Message, msg.ExitConfig, id, factPart)
	}
	return me.Message
}

func TestResolveEnv(t *testing.T) {
	r := Resolver{Getenv: func(k string) string { return map[string]string{"PW": "hunter2"}[k] }}
	s, err := r.Resolve(context.Background(), "prod-pg", mustRef(t, "env:PW"))
	if err != nil || string(s.Expose()) != "hunter2" {
		t.Fatalf("Resolve(env:PW) = %q, %v", s.Expose(), err)
	}
	_, err = r.Resolve(context.Background(), "prod-pg", mustRef(t, "env:MISSING"))
	me := wantFailure(t, err, IDEnvUnset, "environment variable MISSING is not set")
	if me.Target != "prod-pg" {
		t.Errorf("target = %q", me.Target)
	}
}

func TestResolveFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name, content, want string
	}{
		{"plain", "hunter2", "hunter2"},
		{"newline", "hunter2\n", "hunter2"},
		{"crlf", "hunter2\r\n", "hunter2"},
		{"two newlines keep one", "hunter2\n\n", "hunter2\n"},
		{"inner spaces", " pass word ", " pass word "},
	}
	for _, tt := range tests {
		p := write(tt.name, tt.content)
		s, err := Resolver{}.Resolve(context.Background(), "", mustRef(t, "file:"+p))
		if err != nil || string(s.Expose()) != tt.want {
			t.Errorf("%s: Resolve = %q, %v; want %q", tt.name, s.Expose(), err, tt.want)
		}
	}

	_, err := Resolver{}.Resolve(context.Background(), "", mustRef(t, "file:"+filepath.Join(dir, "nope")))
	if me := wantFailure(t, err, IDFileUnreadable, "cannot be read"); me.Cause != "the file does not exist" {
		t.Errorf("cause = %q", me.Cause)
	}
	_, err = Resolver{}.Resolve(context.Background(), "", mustRef(t, "file:"+write("empty", "\n")))
	wantFailure(t, err, IDFileUnreadable, "the file is empty")

	denied := Resolver{ReadFile: func(string) ([]byte, error) { return nil, fs.ErrPermission }}
	_, err = denied.Resolve(context.Background(), "", mustRef(t, "file:/x"))
	if me := wantFailure(t, err, IDFileUnreadable, "cannot be read"); me.Cause != "permission denied" {
		t.Errorf("cause = %q", me.Cause)
	}
}

func TestResolveKeychain(t *testing.T) {
	keyring.MockInit()
	if err := keyring.Set("pentym", "prod-pg", "hunter2"); err != nil {
		t.Fatal(err)
	}
	s, err := Resolver{}.Resolve(context.Background(), "", mustRef(t, "keychain:pentym/prod-pg"))
	if err != nil || string(s.Expose()) != "hunter2" {
		t.Fatalf("Resolve(keychain) = %q, %v", s.Expose(), err)
	}
	_, err = Resolver{}.Resolve(context.Background(), "", mustRef(t, "keychain:pentym/other"))
	if me := wantFailure(t, err, IDKeychainMissing, "keychain:pentym/other"); me.Cause != "no such item" {
		t.Errorf("cause = %q", me.Cause)
	}
	broken := Resolver{KeychainGet: func(string, string) (string, error) { return "", errors.New("dbus: no session bus") }}
	_, err = broken.Resolve(context.Background(), "", mustRef(t, "keychain:a/b"))
	if me := wantFailure(t, err, IDKeychainMissing, "cannot be read"); me.Cause != "the keychain could not be read" {
		t.Errorf("cause = %q", me.Cause)
	}
}

func TestResolveCmd(t *testing.T) {
	s, err := Resolver{}.Resolve(context.Background(), "", helperRef(t, "print", "hunter2"))
	if err != nil || string(s.Expose()) != "hunter2" {
		t.Fatalf("Resolve(cmd print) = %q, %v", s.Expose(), err)
	}

	_, err = Resolver{}.Resolve(context.Background(), "", helperRef(t, "silent", "x"))
	wantFailure(t, err, IDCommandFailed, "printed nothing")

	_, err = Resolver{}.Resolve(context.Background(), "", mustRef(t, "cmd:pentym-no-such-helper-xyz"))
	wantFailure(t, err, IDCommandFailed, "failed")

	_, err = Resolver{}.Resolve(context.Background(), "", mustRef(t, "cmd:'unterminated"))
	wantFailure(t, err, IDCommandFailed, "cannot be parsed")

	start := time.Now()
	_, err = Resolver{Timeout: 500 * time.Millisecond}.Resolve(context.Background(), "", helperRef(t, "sleep", "x"))
	if me := wantFailure(t, err, IDCommandFailed, "failed"); !strings.Contains(me.Cause, "did not finish within 500ms") {
		t.Errorf("cause = %q", me.Cause)
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("timeout took %s", time.Since(start))
	}
}

func TestResolvePlaintextAndZero(t *testing.T) {
	s, err := Resolver{}.Resolve(context.Background(), "", mustRef(t, "hunter2"))
	if err != nil || string(s.Expose()) != "hunter2" {
		t.Fatalf("Resolve(plaintext) = %q, %v", s.Expose(), err)
	}
	_, err = Resolver{}.Resolve(context.Background(), "", Ref{})
	var me *msg.Error
	if !errors.As(err, &me) || me.Code != msg.ExitConfig {
		t.Fatalf("Resolve(zero) = %v", err)
	}
}

// TestSecretLeakResolve checks that failed resolutions never show the
// secret: a helper that prints the secret and then fails, with the secret
// in its error output too.
func TestSecretLeakResolve(t *testing.T) {
	canaries := leaktest.New(t, 1)
	_, err := Resolver{}.Resolve(context.Background(), "prod-pg", helperRef(t, "fail", string(canaries[0])))
	me := wantFailure(t, err, IDCommandFailed, "failed")
	if !strings.Contains(me.Cause, Redacted) {
		t.Errorf("cause was not scrubbed: %q", me.Cause)
	}
	leaktest.Scan(t, "cmd: failure", canaries, err.Error(), fmt.Sprintf("%+v", me))
}
