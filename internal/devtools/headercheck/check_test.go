// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import "testing"

func TestCommentPrefix(t *testing.T) {
	tests := []struct {
		path   string
		prefix string
		ok     bool
	}{
		{"cmd/pentym/main.go", "//", true},
		{"internal/repo/pack_test.go", "//", true},
		{".github/workflows/pr.yml", "#", true},
		{".golangci.yml", "#", true},
		{"deploy/k8s/agent.yaml", "#", true},
		{"Makefile", "#", true},
		{"deploy/Dockerfile", "#", true},
		{"scripts/x.sh", "#", true},
		{"staticcheck.conf", "#", true},
		{`internal\repo\pack.go`, "//", true},

		{"README.md", "", false},
		{"LICENSE", "", false},
		{"NOTICE", "", false},
		{"go.mod", "", false},
		{"go.sum", "", false},
		{"bench/.gitkeep", "", false},
		{".gitignore", "", false},
		{"testdata/golden/v1/config", "", false},
		{"internal/repo/testdata/fuzz/FuzzPack/abc", "", false},
		{"internal/repo/testdata/x.go", "", false},
		{"docs/design/pentymento-prd.md", "", false},
	}
	for _, tt := range tests {
		prefix, ok := commentPrefix(tt.path)
		if prefix != tt.prefix || ok != tt.ok {
			t.Errorf("commentPrefix(%q) = %q, %v; want %q, %v", tt.path, prefix, ok, tt.prefix, tt.ok)
		}
	}
}

func TestHasHeader(t *testing.T) {
	const good = "// SPDX-License-Identifier: Apache-2.0\n// Copyright 2026 Toluwalase Akinyemi\n\npackage x\n"
	tests := []struct {
		name    string
		content string
		prefix  string
		want    bool
	}{
		{"go file", good, "//", true},
		{"hash comments", "# SPDX-License-Identifier: Apache-2.0\n# Copyright 2026 Toluwalase Akinyemi\n", "#", true},
		{"shebang first", "#!/usr/bin/env bash\n# SPDX-License-Identifier: Apache-2.0\n# Copyright 2026 Toluwalase Akinyemi\n", "#", true},
		{"CRLF", "// SPDX-License-Identifier: Apache-2.0\r\n// Copyright 2026 Toluwalase Akinyemi\r\n", "//", true},
		{"year range", "// SPDX-License-Identifier: Apache-2.0\n// Copyright 2026-2027 Toluwalase Akinyemi\n", "//", true},

		{"empty", "", "//", false},
		{"no header", "package x\n", "//", false},
		{"only SPDX", "// SPDX-License-Identifier: Apache-2.0\npackage x\n", "//", false},
		{"wrong licence", "// SPDX-License-Identifier: MIT\n// Copyright 2026 Toluwalase Akinyemi\n", "//", false},
		{"wrong holder", "// SPDX-License-Identifier: Apache-2.0\n// Copyright 2026 Someone Else\n", "//", false},
		{"header not first", "package x\n// SPDX-License-Identifier: Apache-2.0\n// Copyright 2026 Toluwalase Akinyemi\n", "//", false},
		{"wrong comment style", good, "#", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasHeader([]byte(tt.content), tt.prefix); got != tt.want {
				t.Fatalf("hasHeader = %v, want %v", got, tt.want)
			}
		})
	}
}
