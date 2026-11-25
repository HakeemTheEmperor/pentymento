// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

func TestRepositorySection(t *testing.T) {
	repoDir := filepath.Join(t.TempDir(), "repo")
	dir := files(t, map[string]string{"config.yaml": "repository:\n  path: '" + repoDir + "'\n  key: env:PENTYM_KEY\n"})
	c, err := Load(filepath.Join(dir, "config.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Repository.Path != repoDir || c.Repository.Key.Scheme() != secrets.SchemeEnv || c.Repository.Key.String() != "env:PENTYM_KEY" {
		t.Errorf("repository = %+v", c.Repository)
	}

	// Both keys can be overridden from the environment.
	other := filepath.Join(t.TempDir(), "other")
	c, err = Load(filepath.Join(dir, "config.yaml"), []string{
		"PENTYM_REPOSITORY__PATH=" + other, "PENTYM_REPOSITORY__KEY=file:/run/secrets/key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Repository.Path != other || c.Repository.Key.String() != "file:/run/secrets/key" {
		t.Errorf("overridden repository = %+v", c.Repository)
	}
}

func TestRepositoryPathMustBeAbsolute(t *testing.T) {
	dir := files(t, map[string]string{"config.yaml": "repository:\n  path: repo\n"})
	_, err := Load(filepath.Join(dir, "config.yaml"), nil)
	wantConfigError(t, err, IDInvalid, `repository.path must be an absolute path, not "repo"`)
}

// TestRepositoryKeyPlaintext: repository.key is a secret, so a value
// written in the file is refused like any other.
func TestRepositoryKeyPlaintext(t *testing.T) {
	dir := files(t, map[string]string{"config.yaml": "repository:\n  key: hunter2\n"})
	_, err := Load(filepath.Join(dir, "config.yaml"), nil)
	m := wantConfigError(t, err, IDPlaintextRejected, "repository.key")
	if strings.Contains(fmt.Sprintf("%+v", m), "hunter2") {
		t.Errorf("message shows the secret: %+v", m)
	}
}
