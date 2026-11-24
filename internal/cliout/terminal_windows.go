// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build windows

package cliout

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableANSI turns on ANSI escape processing for a Windows console. It
// returns true for anything that is not a console, where the bytes pass
// through unchanged.
func enableANSI(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return true
	}
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return true // not a console
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}

// utf8CodePage is the Windows code page for UTF-8.
const utf8CodePage = 65001

// unicodeSupported reports whether symbols can be shown. On a console they
// can when the shell is Windows Terminal, the output code page is UTF-8, or
// the POSIX locale says UTF-8; pipes and files always get UTF-8.
func unicodeSupported(out any, getenv func(string) string) bool {
	if !isTerminal(out) {
		return true
	}
	if getenv("WT_SESSION") != "" {
		return true
	}
	if cp, err := windows.GetConsoleOutputCP(); err == nil && cp == utf8CodePage {
		return true
	}
	return localeIsUTF8(getenv)
}
