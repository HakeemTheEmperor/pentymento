// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build !windows

package cliout

// enableANSI is a no-op outside Windows: terminals interpret ANSI codes.
func enableANSI(any) bool { return true }

// unicodeSupported reports whether the locale selects UTF-8.
func unicodeSupported(_ any, getenv func(string) string) bool {
	return localeIsUTF8(getenv)
}
