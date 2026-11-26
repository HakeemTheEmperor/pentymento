// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package toolrun

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// ExeName adds the platform's executable suffix to a tool name.
func ExeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// Find finds a client tool in the configured directories (the config's
// tools.<section>), then on PATH. lookPath nil means exec.LookPath.
func Find(name, section string, dirs []string, lookPath func(string) (string, error)) (string, error) {
	for _, d := range dirs {
		p := filepath.Join(d, ExeName(name))
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	p, err := lookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s is not in tools.%s or on PATH", name, section)
	}
	return p, nil
}
