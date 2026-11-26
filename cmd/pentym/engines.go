// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

// The engines register themselves with internal/engine; commands build
// them by the configured name and never name one.
import (
	_ "github.com/HakeemTheEmperor/pentymento/internal/engine/mongodb"
	_ "github.com/HakeemTheEmperor/pentymento/internal/engine/postgres"
)
