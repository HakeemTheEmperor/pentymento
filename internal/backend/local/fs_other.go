// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

//go:build !linux && !darwin && !windows

package local

func filesystem(string) (FSInfo, error) { return FSInfo{Unknown: true}, nil }
