// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build windows

package main

// onReloadSignal does nothing on Windows, which has no SIGHUP; reload
// through the control socket (pentym agent reload) instead.
func onReloadSignal(func()) func() { return func() {} }
