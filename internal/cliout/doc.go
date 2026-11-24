// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Package cliout renders the CLI's human and JSON output in the house style
// (Brand 10 and 11): status words and symbols with an ASCII fallback, ANSI
// colour only on a terminal, aligned borderless tables, units, durations,
// times, messages with a next-step line, and confirmation prompts.
//
// Results and --json go to stdout; messages, prompts and everything else go
// to stderr.
package cliout
