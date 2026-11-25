// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build race

package repo

// raceEnabled reports whether the race detector is on; the slowest
// randomized tests run fewer cases under it, where everything is several
// times slower. Runs without it keep the full counts.
const raceEnabled = true
