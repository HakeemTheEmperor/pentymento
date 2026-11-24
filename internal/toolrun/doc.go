// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Package toolrun runs external database tools as child processes safely:
// no secrets in arguments, streamed stdout, captured stderr, process groups
// or job objects, cancellation, exit-code and version checks.
package toolrun
