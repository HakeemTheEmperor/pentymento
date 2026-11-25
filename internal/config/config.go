// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Config is the pentym configuration. Keys are added by the chunks that use
// them; unknown keys are rejected.
type Config struct {
	// Include lists config files applied before this one.
	Include []string `yaml:"include"`
	// AllowPlaintextSecrets permits secret values written directly in the
	// config, provided the file is not readable by group or others.
	AllowPlaintextSecrets bool `yaml:"allow_plaintext_secrets"`
	// Targets are the configured databases, by name.
	Targets map[string]Target `yaml:"targets"`
}

// Target is one configured database.
type Target struct {
	// Engine is postgres or mongodb.
	Engine string `yaml:"engine"`
}

// Engines are the supported engine names.
var Engines = []string{"postgres", "mongodb"}

// targetNameRE is the form of a target name: it is typed in commands and
// mapped to environment-variable names.
var targetNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// EnvConfigPath names the environment variable that points to the config
// file when --config is not given.
const EnvConfigPath = "PENTYM_CONFIG"

// DefaultPath returns the config file location for an operating system
// (PRD 7.14): /etc/pentym/config.yaml on Linux and other Unix systems,
// /usr/local/etc/pentym/config.yaml on macOS, and
// %ProgramData%\pentym\config.yaml on Windows.
func DefaultPath(goos string, getenv func(string) string) string {
	switch goos {
	case "darwin":
		return "/usr/local/etc/pentym/config.yaml"
	case "windows":
		pd := getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return pd + `\pentym\config.yaml`
	}
	return "/etc/pentym/config.yaml"
}

// Path returns the config file to use: flag if set, else $PENTYM_CONFIG,
// else DefaultPath for this system.
func Path(flag string, getenv func(string) string) string {
	if flag != "" {
		return flag
	}
	if p := getenv(EnvConfigPath); p != "" {
		return p
	}
	return filepath.FromSlash(DefaultPath(runtime.GOOS, getenv))
}

// Load reads the config file at path with its includes and environment
// overrides, and validates it. Errors are *msg.Error values with exit code
// ExitConfig.
func Load(path string, environ []string) (*Config, error) {
	c, src, err := load[Config](path, environ)
	if err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, invalid(src.main, 0, err.Error())
	}
	return c, nil
}

func (c *Config) validate() error {
	names := make([]string, 0, len(c.Targets))
	for name := range c.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := map[string]string{}
	for _, name := range names {
		if !targetNameRE.MatchString(name) {
			return fmt.Errorf("target name %q must be lowercase letters, digits, - and _, starting with a letter or digit, at most 63 characters", name)
		}
		if other, ok := seen[normalize(name)]; ok {
			return fmt.Errorf("target names %q and %q differ only in - and _, so environment overrides cannot tell them apart", other, name)
		}
		seen[normalize(name)] = name
		t := c.Targets[name]
		if t.Engine == "" {
			return fmt.Errorf("target %s has no engine; set engine: postgres or engine: mongodb", name)
		}
		if !contains(Engines, t.Engine) {
			return fmt.Errorf("target %s: engine must be %s, not %q", name, strings.Join(Engines, " or "), t.Engine)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
