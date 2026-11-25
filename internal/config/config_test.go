// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/devtools/leaktest"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// testConfig is a schema with secrets, to exercise the generic loader.
type testConfig struct {
	Include               []string              `yaml:"include"`
	AllowPlaintextSecrets bool                  `yaml:"allow_plaintext_secrets"`
	Port                  int                   `yaml:"port"`
	Enabled               bool                  `yaml:"enabled"`
	Name                  string                `yaml:"name"`
	Password              secrets.Ref           `yaml:"password"`
	Targets               map[string]testTarget `yaml:"targets"`
	Nested                struct {
		Level string `yaml:"level_name"`
	} `yaml:"nested"`
}

type testTarget struct {
	Engine   string      `yaml:"engine"`
	Password secrets.Ref `yaml:"password"`
	Tags     []string    `yaml:"tags"`
}

// files writes name → content into a temporary directory, readable only by
// the owner, and returns the directory.
func files(t *testing.T, contents map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, c := range contents {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func loadTest(t *testing.T, dir string, environ ...string) (*testConfig, error) {
	t.Helper()
	c, _, err := load[testConfig](filepath.Join(dir, "config.yaml"), environ)
	return c, err
}

// wantConfigError checks err is a config *msg.Error with the given ID whose
// fact contains each part, and returns its message.
func wantConfigError(t *testing.T, err error, id msg.ID, parts ...string) msg.Message {
	t.Helper()
	var me *msg.Error
	if !errors.As(err, &me) {
		t.Fatalf("error %v is not a *msg.Error", err)
	}
	if me.Code != msg.ExitConfig || me.Message.ID != id || me.Message.Next == "" {
		t.Fatalf("error = %+v (code %d), want ID %s, exit code 3 and a next step", me.Message, me.Code, id)
	}
	for _, p := range parts {
		if !strings.Contains(me.Message.Fact, p) {
			t.Errorf("fact %q does not contain %q", me.Message.Fact, p)
		}
	}
	return me.Message
}

func TestLoadConfig(t *testing.T) {
	dir := files(t, map[string]string{"config.yaml": `
targets:
  prod-pg:
    engine: postgres
  prod-mg:
    engine: mongodb
`})
	c, err := Load(filepath.Join(dir, "config.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Targets) != 2 || c.Targets["prod-pg"].Engine != "postgres" || c.Targets["prod-mg"].Engine != "mongodb" {
		t.Errorf("targets = %+v", c.Targets)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	dir := files(t, map[string]string{"config.yaml": ""})
	c, err := Load(filepath.Join(dir, "config.yaml"), nil)
	if err != nil || len(c.Targets) != 0 {
		t.Fatalf("Load(empty) = %+v, %v", c, err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name, yaml string
		want       string
	}{
		{"missing engine", "targets:\n  prod-pg: {}\n", "target prod-pg has no engine"},
		{"unknown engine", "targets:\n  prod:\n    engine: mysql\n", `engine must be postgres or mongodb, not "mysql"`},
		{"upper-case name", "targets:\n  Prod:\n    engine: postgres\n", `target name "Prod"`},
		{"name with space", "targets:\n  'prod pg':\n    engine: postgres\n", `target name "prod pg"`},
		{"leading dash", "targets:\n  -prod:\n    engine: postgres\n", `target name "-prod"`},
		{"names differ in - and _", "targets:\n  prod-pg:\n    engine: postgres\n  prod_pg:\n    engine: postgres\n", "differ only in - and _"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := files(t, map[string]string{"config.yaml": tt.yaml})
			_, err := Load(filepath.Join(dir, "config.yaml"), nil)
			wantConfigError(t, err, IDInvalid, tt.want)
		})
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name, yaml string
		parts      []string
	}{
		{"unknown key", "port: 1\nenigne: postgres\n", []string{"config.yaml line 2", `unknown key "enigne"`}},
		{"unknown nested key", "targets:\n  a:\n    engin: postgres\n", []string{"line 3", `unknown key "engin"`}},
		{"wrong type", "port: [1, 2]\n", []string{"line 1", "expected a number, got a list"}},
		{"syntax error", "port: 1\n  bad: indent\n", []string{"line 2"}},
		{"not a mapping", "- a\n- b\n", []string{"must be a mapping"}},
		{"secret as list", "password: [a]\n", []string{"line 1", "must be a reference"}},
		{"empty env reference", "password: 'env:'\n", []string{"line 1", "missing its argument"}},
		{"two problems", "a: 1\nb: 2\n", []string{`unknown key "a"`, "and 1 more problems"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := files(t, map[string]string{"config.yaml": tt.yaml})
			_, err := loadTest(t, dir)
			wantConfigError(t, err, IDInvalid, tt.parts...)
		})
	}
}

func TestNotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := loadTest(t, dir)
	wantConfigError(t, err, IDNotFound, "config.yaml not found")
}

func TestIncludes(t *testing.T) {
	dir := files(t, map[string]string{
		"config.yaml": `
include:
  - base.yaml
  - sub/targets.yaml
port: 3
targets:
  a:
    engine: postgres
`,
		"base.yaml": `
port: 1
name: base
nested:
  level_name: base
targets:
  a:
    engine: mongodb
    tags: [x, y]
`,
		"sub/targets.yaml": `
include: [more.yaml]
port: 2
targets:
  b:
    engine: mongodb
`,
		"sub/more.yaml": "enabled: true\n",
	})
	c, err := loadTest(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 3 || c.Name != "base" || !c.Enabled || c.Nested.Level != "base" {
		t.Errorf("scalars = port %d, name %q, enabled %v, level %q", c.Port, c.Name, c.Enabled, c.Nested.Level)
	}
	// Mappings merge: a keeps its tags from base.yaml, engine from config.yaml.
	if a := c.Targets["a"]; a.Engine != "postgres" || len(a.Tags) != 2 {
		t.Errorf("target a = %+v", a)
	}
	if c.Targets["b"].Engine != "mongodb" {
		t.Errorf("target b = %+v", c.Targets["b"])
	}
}

func TestIncludeListsReplace(t *testing.T) {
	dir := files(t, map[string]string{
		"config.yaml": "include: [base.yaml]\ntargets:\n  a:\n    tags: [z]\n",
		"base.yaml":   "targets:\n  a:\n    engine: postgres\n    tags: [x, y]\n",
	})
	c, err := loadTest(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if tags := c.Targets["a"].Tags; len(tags) != 1 || tags[0] != "z" {
		t.Errorf("tags = %v, want [z]", tags)
	}
}

func TestIncludeErrors(t *testing.T) {
	t.Run("cycle", func(t *testing.T) {
		dir := files(t, map[string]string{
			"config.yaml": "include: [a.yaml]\n",
			"a.yaml":      "include: [b.yaml]\n",
			"b.yaml":      "include: [config.yaml]\n",
		})
		_, err := loadTest(t, dir)
		wantConfigError(t, err, IDInvalid, "include cycle")
	})
	t.Run("self", func(t *testing.T) {
		dir := files(t, map[string]string{"config.yaml": "include: [./config.yaml]\n"})
		_, err := loadTest(t, dir)
		wantConfigError(t, err, IDInvalid, "include cycle")
	})
	t.Run("diamond is not a cycle", func(t *testing.T) {
		dir := files(t, map[string]string{
			"config.yaml": "include: [a.yaml, b.yaml]\n",
			"a.yaml":      "include: [common.yaml]\n",
			"b.yaml":      "include: [common.yaml]\n",
			"common.yaml": "port: 7\n",
		})
		if c, err := loadTest(t, dir); err != nil || c.Port != 7 {
			t.Fatalf("diamond = %+v, %v", c, err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		dir := files(t, map[string]string{"config.yaml": "include: [nope.yaml]\n"})
		_, err := loadTest(t, dir)
		m := wantConfigError(t, err, IDNotFound, "nope.yaml", "included from")
		if !strings.Contains(m.Next, "config.yaml") {
			t.Errorf("next = %q", m.Next)
		}
	})
	t.Run("error in an include names that file", func(t *testing.T) {
		dir := files(t, map[string]string{
			"config.yaml": "include: [base.yaml]\n",
			"base.yaml":   "port: 1\nbogus: 2\n",
		})
		_, err := loadTest(t, dir)
		wantConfigError(t, err, IDInvalid, "base.yaml line 2", `unknown key "bogus"`)
	})
}

func TestEnvOverrides(t *testing.T) {
	dir := files(t, map[string]string{"config.yaml": `
port: 1
targets:
  prod-pg:
    engine: mongodb
`})
	c, err := loadTest(t, dir,
		"PENTYM_PORT=5432",
		"pentym_enabled=true",
		"PENTYM_TARGETS__PROD_PG__ENGINE=postgres",
		"PENTYM_TARGETS__PROD_PG__PASSWORD=env:PGPASSWORD",
		"PENTYM_NESTED__LEVEL_NAME=deep",
		"PENTYM_CONFIG=/elsewhere.yaml",
		"PENTYM_TEST_POSTGRES=17",
		"PATH=/usr/bin",
	)
	if err != nil {
		t.Fatal(err)
	}
	pg := c.Targets["prod-pg"]
	if c.Port != 5432 || !c.Enabled || pg.Engine != "postgres" || pg.Password.String() != "env:PGPASSWORD" || c.Nested.Level != "deep" {
		t.Errorf("overrides not applied: port %d, enabled %v, target %+v, level %q", c.Port, c.Enabled, pg, c.Nested.Level)
	}
}

func TestEnvOverrideErrors(t *testing.T) {
	yaml := "targets:\n  prod-pg:\n    engine: postgres\n  other:\n    engine: postgres\n"
	tests := []struct {
		name string
		env  string
		want string
	}{
		{"unknown key", "PENTYM_PROT=1", "the config has no key matching PROT"},
		{"unknown target", "PENTYM_TARGETS__STAGING__ENGINE=postgres", "targets has no entry matching STAGING"},
		{"below a value", "PENTYM_PORT__X=1", "port has no keys below it"},
		{"not a value", "PENTYM_TARGETS=x", "targets is not a single value"},
		{"a list", "PENTYM_INCLUDE=x.yaml", "include is not a single value"},
		{"empty segment", "PENTYM_TARGETS____ENGINE=x", "empty key"},
		{"wrong type", "PENTYM_PORT=abc", "expected a number, got a string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := files(t, map[string]string{"config.yaml": yaml})
			_, err := loadTest(t, dir, tt.env)
			wantConfigError(t, err, IDInvalid, tt.want)
		})
	}

	t.Run("ambiguous target", func(t *testing.T) {
		dir := files(t, map[string]string{"config.yaml": "targets:\n  a-b: {}\n  a_b: {}\n"})
		_, err := loadTest(t, dir, "PENTYM_TARGETS__A_B__ENGINE=postgres")
		wantConfigError(t, err, IDInvalid, "matches several entries", "a-b", "a_b")
	})
}

func TestPlaintextSecrets(t *testing.T) {
	t.Run("rejected by default", func(t *testing.T) {
		dir := files(t, map[string]string{"config.yaml": "port: 1\ntargets:\n  a:\n    password: hunter2\n"})
		_, err := loadTest(t, dir)
		wantConfigError(t, err, IDPlaintextRejected, "config.yaml line 4", "targets.a.password")
	})
	t.Run("rejected in an include", func(t *testing.T) {
		dir := files(t, map[string]string{
			"config.yaml": "include: [secret.yaml]\n",
			"secret.yaml": "password: hunter2\n",
		})
		_, err := loadTest(t, dir)
		wantConfigError(t, err, IDPlaintextRejected, "secret.yaml line 1", "(password)")
	})
	t.Run("references are fine", func(t *testing.T) {
		dir := files(t, map[string]string{"config.yaml": "password: env:PW\ntargets:\n  a:\n    password: file:/run/secrets/a\n"})
		if _, err := loadTest(t, dir); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("allowed in a private file", func(t *testing.T) {
		dir := files(t, map[string]string{"config.yaml": "allow_plaintext_secrets: true\npassword: hunter2\n"})
		c, err := loadTest(t, dir)
		if err != nil || !c.Password.IsPlaintext() {
			t.Fatalf("Load = %+v, %v", c, err)
		}
	})
	t.Run("allowed by an include", func(t *testing.T) {
		dir := files(t, map[string]string{
			"config.yaml": "include: [allow.yaml]\npassword: hunter2\n",
			"allow.yaml":  "allow_plaintext_secrets: true\n",
		})
		if _, err := loadTest(t, dir); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("allowed but readable by others", func(t *testing.T) {
		dir := files(t, map[string]string{"config.yaml": "allow_plaintext_secrets: true\npassword: hunter2\n"})
		makeReadableByOthers(t, filepath.Join(dir, "config.yaml"))
		_, err := loadTest(t, dir)
		wantConfigError(t, err, IDPlaintextReadable, "config.yaml", "other users can read it")
	})
	t.Run("readable file without plaintext is fine", func(t *testing.T) {
		dir := files(t, map[string]string{"config.yaml": "password: env:PW\n"})
		makeReadableByOthers(t, filepath.Join(dir, "config.yaml"))
		if _, err := loadTest(t, dir); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("environment override must be a reference", func(t *testing.T) {
		dir := files(t, map[string]string{"config.yaml": "allow_plaintext_secrets: true\ntargets:\n  a: {}\n"})
		_, err := loadTest(t, dir, "PENTYM_TARGETS__A__PASSWORD=hunter2")
		wantConfigError(t, err, IDPlaintextRejected, "environment variable PENTYM_TARGETS__A__PASSWORD")
	})
	t.Run("environment cannot allow plaintext for itself", func(t *testing.T) {
		dir := files(t, map[string]string{"config.yaml": "password: hunter2\n"})
		makeReadableByOthers(t, filepath.Join(dir, "config.yaml"))
		_, err := loadTest(t, dir, "PENTYM_ALLOW_PLAINTEXT_SECRETS=true")
		wantConfigError(t, err, IDPlaintextReadable)
	})
}

// TestSecretLeakConfig puts canary secrets in every place the loader
// reports on and checks no error shows them.
func TestSecretLeakConfig(t *testing.T) {
	canaries := leaktest.New(t, 1)
	c := string(canaries[0])
	cases := []struct {
		yaml string
		env  []string
	}{
		{yaml: "password: " + c + "\n"},                                                  // plaintext rejected
		{yaml: "port: " + c + "\n"},                                                      // wrong type
		{yaml: "enabled: " + c + "\n"},                                                   // wrong type
		{yaml: "targets:\n  a:\n    tags: " + c + "\n"},                                  // wrong type
		{yaml: "port: 1\n", env: []string{"PENTYM_PORT=" + c}},                           // bad override value
		{yaml: "targets:\n  a: {}\n", env: []string{"PENTYM_TARGETS__A__PASSWORD=" + c}}, // plaintext override
		{yaml: "password: '" + c + "\n"},                                                 // syntax error
		{yaml: "allow_plaintext_secrets: true\npassword: " + c + "\n"},                   // readable file
	}
	for i, tc := range cases {
		dir := files(t, map[string]string{"config.yaml": tc.yaml})
		if i == len(cases)-1 {
			makeReadableByOthers(t, filepath.Join(dir, "config.yaml"))
		}
		_, err := loadTest(t, dir, tc.env...)
		var me *msg.Error
		if !errors.As(err, &me) {
			t.Fatalf("case %d: error %v is not a *msg.Error", i, err)
		}
		leaktest.Scan(t, "config error", canaries, err.Error()+me.Message.Next+me.Message.Impact)
	}
}

func TestPath(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := Path("/a.yaml", env(map[string]string{EnvConfigPath: "/b.yaml"})); got != "/a.yaml" {
		t.Errorf("flag should win, got %q", got)
	}
	if got := Path("", env(map[string]string{EnvConfigPath: "/b.yaml"})); got != "/b.yaml" {
		t.Errorf("PENTYM_CONFIG should win over the default, got %q", got)
	}
	if got := Path("", env(nil)); got == "" {
		t.Error("no default path")
	}
	tests := []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"linux", nil, "/etc/pentym/config.yaml"},
		{"freebsd", nil, "/etc/pentym/config.yaml"},
		{"darwin", nil, "/usr/local/etc/pentym/config.yaml"},
		{"windows", map[string]string{"ProgramData": `D:\PD`}, `D:\PD\pentym\config.yaml`},
		{"windows", nil, `C:\ProgramData\pentym\config.yaml`},
	}
	for _, tt := range tests {
		if got := DefaultPath(tt.goos, env(tt.env)); got != tt.want {
			t.Errorf("DefaultPath(%s) = %q, want %q", tt.goos, got, tt.want)
		}
	}
}

func TestYAMLProblemWording(t *testing.T) {
	tests := []struct {
		yaml, want string
	}{
		{"port: [1]\n", "expected a number, got a list"},
		{"enabled: maybe\n", "expected true or false, got a string"},
		{"targets: [a]\n", "expected a mapping, got a list"},
		{"targets:\n  a: x\n", "expected a mapping, got a string"},
		{"targets:\n  a:\n    tags: x\n", "expected a list, got a string"},
		{"name: {a: 1}\n", "expected a string, got a mapping"},
	}
	for _, tt := range tests {
		dir := files(t, map[string]string{"config.yaml": tt.yaml})
		_, err := loadTest(t, dir)
		wantConfigError(t, err, IDInvalid, tt.want)
	}
}
