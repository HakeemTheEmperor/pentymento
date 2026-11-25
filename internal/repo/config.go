// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/chunker"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// FormatVersion is the repository format this pentym reads and writes. A
// binary refuses a repository with a newer format (SDD 5.6); only
// `pentym repo upgrade` changes it.
const FormatVersion = 1

// Encryption modes.
const (
	EncryptionOn       = "encrypted"
	EncryptionInsecure = "insecure-test-mode"
)

// Message IDs for repositories (see docs/errors).
const (
	IDFormatNewer msg.ID = "PTM-3001"
	IDNotFound    msg.ID = "PTM-3002"
	IDExists      msg.ID = "PTM-3003"
)

// Config is the repository's config object: plaintext JSON, holding no
// secrets.
type Config struct {
	Version    int           `json:"version"`
	ID         string        `json:"id"`
	Encryption string        `json:"encryption"`
	Chunker    ChunkerConfig `json:"chunker"`
	Created    time.Time     `json:"created"`
}

// ChunkerConfig records the chunk sizes the repository was created with.
type ChunkerConfig struct {
	Min int `json:"min"`
	Avg int `json:"avg"`
	Max int `json:"max"`
}

// NewConfig returns the config for a new repository.
func NewConfig(insecure bool, now time.Time) Config {
	enc := EncryptionOn
	if insecure {
		enc = EncryptionInsecure
	}
	p := chunker.DefaultParams
	return Config{
		Version:    FormatVersion,
		ID:         randomHex(16),
		Encryption: enc,
		Chunker:    ChunkerConfig{Min: p.Min, Avg: p.Avg, Max: p.Max},
		Created:    now.UTC().Truncate(time.Second),
	}
}

// Insecure reports whether the repository is unencrypted.
func (c Config) Insecure() bool { return c.Encryption == EncryptionInsecure }

// ParseConfig decodes a config object and refuses formats this pentym does
// not understand.
func ParseConfig(data []byte) (Config, error) {
	// Read the version first, so a newer format is reported as such rather
	// than as unknown fields.
	var v struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return Config{}, fmt.Errorf("repository config is malformed: %w", err)
	}
	if v.Version > FormatVersion {
		return Config{}, &msg.Error{Code: msg.ExitFailure, Message: msg.Message{
			Level: msg.LevelFailed, ID: IDFormatNewer,
			Fact:   fmt.Sprintf("repository format %d is newer than this pentym understands (format %d)", v.Version, FormatVersion),
			Impact: "this pentym cannot read or write the repository; nothing was changed",
			Next:   "upgrade pentym to the version that created or upgraded this repository",
		}}
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("repository config is malformed: %w", err)
	}
	switch {
	case c.Version < 1:
		return Config{}, errors.New("repository config has no format version")
	case len(c.ID) != 32:
		return Config{}, errors.New("repository config has a malformed ID")
	case c.Encryption != EncryptionOn && c.Encryption != EncryptionInsecure:
		return Config{}, fmt.Errorf("repository encryption %q is not supported", c.Encryption)
	}
	p := chunker.Params{Min: c.Chunker.Min, Avg: c.Chunker.Avg, Max: c.Chunker.Max}
	if p != chunker.DefaultParams {
		return Config{}, fmt.Errorf("repository chunk sizes %+v are not supported", c.Chunker)
	}
	return c, nil
}
