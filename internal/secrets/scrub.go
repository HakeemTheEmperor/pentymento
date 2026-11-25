// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package secrets

import (
	"regexp"
	"strings"
)

var (
	// userinfoRE matches the password in a URL's userinfo, such as
	// postgres://user:pass@host.
	userinfoRE = regexp.MustCompile(`(://[^/:@\s]*:)[^@\s]+@`)
	// keyValueRE matches password-like key=value and key: value pairs, as
	// in libpq connection strings and driver messages.
	keyValueRE = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key)(\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;&]+)`)
)

// Scrub returns text with every known secret value and every password-like
// pattern (URL userinfo passwords, password=… pairs) replaced by Redacted.
func Scrub(text string, known ...Secret) string {
	for _, s := range known {
		if v := s.Expose(); len(v) > 0 {
			text = strings.ReplaceAll(text, string(v), Redacted)
		}
	}
	text = userinfoRE.ReplaceAllString(text, "${1}"+Redacted+"@")
	return keyValueRE.ReplaceAllString(text, "${1}${2}"+Redacted)
}

// ScrubError wraps err so that its message is scrubbed with Scrub. The
// original error stays reachable through errors.Is and errors.As; callers
// must show or log only the wrapper's Error text.
func ScrubError(err error, known ...Secret) error {
	if err == nil {
		return nil
	}
	return &scrubbedError{text: Scrub(err.Error(), known...), err: err}
}

type scrubbedError struct {
	text string
	err  error
}

func (e *scrubbedError) Error() string { return e.text }
func (e *scrubbedError) Unwrap() error { return e.err }
