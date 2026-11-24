// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// version is the release version, set at build time with
// -ldflags "-X main.version=1.0.0". Local builds report "dev".
var version = "dev"

// versionInfo is the --version output.
type versionInfo struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Go       string `json:"go"`
	Platform string `json:"platform"`
}

func currentVersion() versionInfo {
	v := versionInfo{
		Version:  version,
		Commit:   "unknown",
		Go:       runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				v.Commit = s.Value
				if len(v.Commit) > 7 {
					v.Commit = v.Commit[:7]
				}
			}
		}
	}
	return v
}

// String renders the human form:
// "pentym 1.0.0 (commit abc1234, go1.26.11, linux/amd64)".
func (v versionInfo) String() string {
	return fmt.Sprintf("pentym %s (commit %s, %s, %s)", v.Version, v.Commit, v.Go, v.Platform)
}

func (a *app) printVersion() error {
	v := currentVersion()
	if a.printer.JSON {
		return a.printer.WriteJSON(v)
	}
	_, err := fmt.Fprintln(a.printer.Stdout, v.String())
	return err
}
