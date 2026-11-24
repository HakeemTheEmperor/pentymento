// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"path"
	"regexp"
	"strings"
)

const spdxLine = "SPDX-License-Identifier: Apache-2.0"

var copyrightRE = regexp.MustCompile(`^Copyright \d{4}(-\d{4})? Toluwalase Akinyemi$`)

// hashCommentExts are file extensions whose comments start with "#".
var hashCommentExts = map[string]bool{
	".yml": true, ".yaml": true, ".sh": true, ".conf": true, ".toml": true,
}

// hashCommentNames are file names whose comments start with "#".
var hashCommentNames = map[string]bool{"Makefile": true, "Dockerfile": true}

// commentPrefix returns the line-comment prefix for a file that must carry
// the licence header, or false when the file is exempt: files without
// comment syntax (Markdown, LICENSE, NOTICE, go.mod, go.sum, .gitkeep) and
// fixtures under testdata/ or the design documents under docs/design/.
func commentPrefix(p string) (string, bool) {
	p = path.Clean(strings.ReplaceAll(p, `\`, "/"))
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if seg == "testdata" {
			return "", false
		}
	}
	if strings.HasPrefix(p, "docs/design/") {
		return "", false
	}
	base := path.Base(p)
	switch {
	case path.Ext(base) == ".go":
		return "//", true
	case hashCommentExts[path.Ext(base)], hashCommentNames[base]:
		return "#", true
	}
	return "", false
}

// hasHeader reports whether content starts with the SPDX line and the
// copyright line, each as a line comment with the given prefix. A leading
// "#!" line is allowed before the header.
func hasHeader(content []byte, prefix string) bool {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := strings.SplitN(text, "\n", 4)
	if len(lines) > 0 && strings.HasPrefix(lines[0], "#!") {
		lines = lines[1:]
	}
	if len(lines) < 2 {
		return false
	}
	return lines[0] == prefix+" "+spdxLine &&
		strings.HasPrefix(lines[1], prefix+" ") &&
		copyrightRE.MatchString(strings.TrimPrefix(lines[1], prefix+" "))
}
