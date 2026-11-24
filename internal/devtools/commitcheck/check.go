// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxSubjectLen is the subject-line limit from the commit conventions
// (Build Order 2.3).
const maxSubjectLen = 72

// crossCuttingPrefixes are the prefixes allowed besides a real directory.
var crossCuttingPrefixes = map[string]bool{"all": true, "docs": true}

var (
	subjectRE = regexp.MustCompile(`^([^\s:]+): (.*)$`)
	signOffRE = regexp.MustCompile(`^Signed-off-by: (.+) <([^<>\s]+)>$`)
)

// Commit is the part of a commit that the checks need.
type Commit struct {
	Message     string
	AuthorName  string
	AuthorEmail string
}

// CheckMessage returns every convention the commit breaks; none means it
// passes. isDir reports whether a path is a real directory in the
// repository at that commit.
func CheckMessage(c Commit, isDir func(string) bool) []string {
	msg := strings.ReplaceAll(c.Message, "\r\n", "\n")
	msg = strings.TrimRight(msg, "\n")
	lines := strings.Split(msg, "\n")
	subject := lines[0]

	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	m := subjectRE.FindStringSubmatch(subject)
	if m == nil {
		add("subject must be \"<package path>: <summary>\"")
	} else {
		prefix, summary := m[1], m[2]
		if !crossCuttingPrefixes[prefix] && (strings.HasSuffix(prefix, "/") || !isDir(prefix)) {
			add("subject prefix %q is not a directory in the repository, all or docs", prefix)
		}
		if summary == "" || strings.TrimSpace(summary) != summary {
			add("summary must be non-empty with no leading or trailing spaces")
		} else if r, _ := utf8.DecodeRuneInString(summary); unicode.IsUpper(r) {
			add("summary must start with a lowercase letter")
		}
	}
	if strings.HasSuffix(subject, ".") {
		add("subject must not end with a full stop")
	}
	if n := utf8.RuneCountInString(subject); n > maxSubjectLen {
		add("subject is %d characters; the limit is %d", n, maxSubjectLen)
	}
	if len(lines) > 1 && strings.TrimSpace(lines[1]) != "" {
		add("second line must be blank")
	}

	if !hasSignOff(lines, c.AuthorEmail) {
		add("last paragraph must contain \"Signed-off-by: <name> <%s>\" matching the author (use git commit -s)", c.AuthorEmail)
	}
	return problems
}

// hasSignOff reports whether the final paragraph of the message (the
// trailer block) has a Signed-off-by line for the author's email. A
// sign-off anywhere else, such as on the subject line, does not count.
func hasSignOff(lines []string, authorEmail string) bool {
	if len(lines) < 2 {
		return false
	}
	start := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			start = i + 1
			break
		}
	}
	if start == 0 {
		// No blank line: the whole message is one paragraph, so there is
		// no trailer block separate from the subject.
		return false
	}
	for _, l := range lines[start:] {
		m := signOffRE.FindStringSubmatch(strings.TrimSpace(l))
		if m != nil && strings.TrimSpace(m[1]) != "" && strings.EqualFold(m[2], authorEmail) {
			return true
		}
	}
	return false
}
