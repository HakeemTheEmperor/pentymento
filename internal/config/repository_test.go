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

func TestRepositoryS3(t *testing.T) {
	dir := files(t, map[string]string{"config.yaml": `
repository:
  s3:
    bucket: acme-pentym
    prefix: /prod/
    region: eu-west-1
    endpoint: https://minio.internal:9000
    path_style: true
    credentials:
      access_key_id: env:S3_KEY_ID
      secret_access_key: env:S3_SECRET
  key: env:PENTYM_KEY
`})
	c, err := Load(filepath.Join(dir, "config.yaml"), []string{"PENTYM_REPOSITORY__S3__REGION=auto"})
	if err != nil {
		t.Fatal(err)
	}
	s := c.Repository.S3
	if s.Bucket != "acme-pentym" || s.Region != "auto" || !s.PathStyle || s.Endpoint != "https://minio.internal:9000" ||
		s.Credentials.AccessKeyID.String() != "env:S3_KEY_ID" || s.Credentials.SecretAccessKey.String() != "env:S3_SECRET" {
		t.Errorf("s3 = %+v", s)
	}
	if !c.Repository.Configured() || c.Repository.Location() != "s3://acme-pentym/prod" {
		t.Errorf("location = %q", c.Repository.Location())
	}
	// Without credentials, the default chain is used.
	dir = files(t, map[string]string{"config.yaml": "repository:\n  s3:\n    bucket: b-1\n    region: us-east-1\n"})
	if c, err := Load(filepath.Join(dir, "config.yaml"), nil); err != nil || c.Repository.Location() != "s3://b-1" {
		t.Errorf("minimal: %v, %v", c, err)
	}
}

func TestRepositoryS3Invalid(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "repo")
	for _, tt := range []struct{ yaml, want string }{
		{"path: '" + abs + "'\n  s3:\n    bucket: b-1\n    region: r", "set repository.path or repository.s3, not both"},
		{"s3:\n    region: r", "repository.s3.bucket is required"},
		{"s3:\n    bucket: Bad_Bucket\n    region: r", `repository.s3.bucket "Bad_Bucket" is not a valid bucket name (3-63 lowercase letters, digits, . and -)`},
		{"s3:\n    bucket: b-1", "repository.s3.region is required (for S3-compatible services, the region they expect, such as us-east-1 or auto)"},
		{"s3:\n    bucket: b-1\n    region: r\n    prefix: ../up", `repository.s3.prefix "../up" may hold only letters, digits, ., _, - and /, and no segment may start with a dot`},
		{"s3:\n    bucket: b-1\n    region: r\n    endpoint: minio:9000", `repository.s3.endpoint "minio:9000" must be a URL such as https://s3.example.com`},
		{"s3:\n    bucket: b-1\n    region: r\n    endpoint: http://minio.internal:9000", "repository.s3.endpoint must use https; plain http is allowed only on the loopback interface, not minio.internal:9000"},
		{"s3:\n    bucket: b-1\n    region: r\n    credentials:\n      access_key_id: env:K", "repository.s3.credentials needs both access_key_id and secret_access_key, or neither for the default credential chain"},
	} {
		dir := files(t, map[string]string{"config.yaml": "repository:\n  " + tt.yaml + "\n"})
		_, err := Load(filepath.Join(dir, "config.yaml"), nil)
		wantConfigError(t, err, IDInvalid, tt.want)
	}
	for _, ok := range []string{"http://127.0.0.1:9000", "http://localhost:9000", "http://[::1]:9000"} {
		dir := files(t, map[string]string{"config.yaml": "repository:\n  s3:\n    bucket: b-1\n    region: r\n    endpoint: '" + ok + "'\n"})
		if _, err := Load(filepath.Join(dir, "config.yaml"), nil); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	// The secret key is a secret: a value written in the file is refused.
	dir := files(t, map[string]string{"config.yaml": "repository:\n  s3:\n    bucket: b-1\n    region: r\n    credentials:\n      access_key_id: env:K\n      secret_access_key: hunter2\n"})
	_, err := Load(filepath.Join(dir, "config.yaml"), nil)
	wantConfigError(t, err, IDPlaintextRejected, "repository.s3.credentials.secret_access_key")
}
