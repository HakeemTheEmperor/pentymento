// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Command pentym backs up and restores self-hosted Postgres and MongoDB, with
// point-in-time recovery. It runs one-shot commands and the long-running
// agent from one binary.
package main

import (
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes pentym with the given arguments and returns the process exit
// code. The command tree is added in a later step; for now run does nothing.
func run(_ []string, _, _ io.Writer) int {
	return 0
}
