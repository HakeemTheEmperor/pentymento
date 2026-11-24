// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

// Package ops is the operations layer: one implementation per operation
// (init, backup, catch-up, restore, events, list, status, verify, check,
// prune, pin, dump, history, key management, doctor, notify test, debug
// bundle), each labelled read, write or destructive. The CLI and the agent
// are thin callers of this package.
package ops
