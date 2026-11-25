// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package secrets

import (
	"errors"
	"strings"
)

// SplitCommand splits a cmd: helper into arguments, shell-style but without
// a shell: whitespace separates arguments; single quotes keep everything
// literally; double quotes keep spaces and allow \" and \\ escapes. A
// backslash outside quotes is literal, so Windows paths need no quoting
// unless they contain spaces.
func SplitCommand(s string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		inArg   bool
		quote   rune
		escaped bool
	)
	for _, c := range s {
		switch {
		case escaped:
			if c != '"' && c != '\\' {
				cur.WriteRune('\\')
			}
			cur.WriteRune(c)
			escaped = false
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case quote == '"':
			switch c {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote = c
			inArg = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(c)
			inArg = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unterminated quote")
	}
	if inArg {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, errors.New("no command given")
	}
	return args, nil
}
