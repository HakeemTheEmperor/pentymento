// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build integration

package main

import (
	"strings"
	"testing"

	"github.com/HakeemTheEmperor/pentymento/internal/devtools/dbtest"
)

// TestMinIOParity: a repository in a MinIO bucket behaves as one on local
// storage (Build Order C15).
func TestMinIOParity(t *testing.T) {
	m := dbtest.StartMinIO(t)
	local := session(t, localCLIEnv(t))
	c := s3CLIEnv(t, m.Endpoint)
	c.env["S3_KEY_ID"], c.env["S3_SECRET"] = m.AccessKey, m.SecretKey
	remote := session(t, c)
	for i := range local {
		if local[i] != remote[i] {
			t.Errorf("local and MinIO differ:\n--- local\n%s\n--- minio\n%s", local[i], remote[i])
		}
	}
	if ch := doctorRepositoryCheck(t, c); ch.Status != "ok" || !strings.Contains(ch.Result, "conditional create: yes (tested)") {
		t.Errorf("doctor = %+v", ch)
	}
}
