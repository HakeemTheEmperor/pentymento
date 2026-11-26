// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package config

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestInstanceSettings(t *testing.T) {
	work := filepath.Join(t.TempDir(), "work")
	pg := filepath.Join(t.TempDir(), "pg17")
	dir := files(t, map[string]string{"config.yaml": "instance_provider: [local, docker]\nwork_dir: '" + work + "'\ntools:\n  postgres: ['" + pg + "']\n"})
	c, err := Load(filepath.Join(dir, "config.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Providers(), []string{"local", "docker"}) || c.WorkDir != work || !slices.Equal(c.Tools.Postgres, []string{pg}) {
		t.Errorf("config = %+v", c)
	}
	if got, err := c.WorkDirPath(); err != nil || got != work {
		t.Errorf("WorkDirPath = %q, %v", got, err)
	}

	// Defaults.
	dir = files(t, map[string]string{"config.yaml": ""})
	c, err = Load(filepath.Join(dir, "config.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Providers(), []string{"docker", "local"}) {
		t.Errorf("default providers = %v", c.Providers())
	}
	if got, err := c.WorkDirPath(); err != nil || filepath.Base(got) != "work" || !filepath.IsAbs(got) {
		t.Errorf("default work dir = %q, %v", got, err)
	}
	// Providers returns a copy.
	p := c.Providers()
	p[0] = "x"
	if c.Providers()[0] != "docker" {
		t.Error("Providers shares its slice")
	}
}

func TestInstanceSettingsInvalid(t *testing.T) {
	for _, tt := range []struct{ yaml, want string }{
		{"instance_provider: [podman]\n", `instance_provider: "podman" is not a provider; use docker and/or local`},
		{"instance_provider: [local, local]\n", "instance_provider lists local twice"},
		{"work_dir: work\n", `work_dir must be an absolute path, not "work"`},
		{"tools:\n  mongodb: [bin]\n", `tools.mongodb: "bin" must be an absolute path`},
	} {
		dir := files(t, map[string]string{"config.yaml": tt.yaml})
		_, err := Load(filepath.Join(dir, "config.yaml"), nil)
		wantConfigError(t, err, IDInvalid, tt.want)
	}
}
