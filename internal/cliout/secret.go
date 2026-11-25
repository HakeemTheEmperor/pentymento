// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package cliout

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// ReadSecret asks for a secret, such as a passphrase, on stderr and reads
// it without echo. It returns ErrNotInteractive when there is no terminal.
// The answer is never written anywhere.
func (p *Printer) ReadSecret(prompt string) (secrets.Secret, error) {
	if !p.Interactive {
		return secrets.Secret{}, ErrNotInteractive
	}
	_, _ = fmt.Fprintf(p.Stderr, "%s: ", prompt)
	b, err := p.readSecret()
	_, _ = fmt.Fprintln(p.Stderr)
	if err != nil {
		return secrets.Secret{}, err
	}
	return secrets.New(b), nil
}

// readSecret reads one line without echo from a terminal, or a plain line
// from any other reader (tests).
func (p *Printer) readSecret() ([]byte, error) {
	if f, ok := p.rawStdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		return term.ReadPassword(int(f.Fd()))
	}
	if p.stdin == nil {
		return nil, nil
	}
	line, err := p.stdin.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return nil, err
	}
	out := bytes.TrimRight(line, "\r\n")
	clone := append([]byte(nil), out...)
	clear(line)
	return clone, nil
}
