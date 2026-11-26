// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestAgentSection(t *testing.T) {
	dir := files(t, map[string]string{"config.yaml": `
work_dir: '` + filepath.Join(t.TempDir(), "work") + `'
agent:
  health_listen: 127.0.0.1:9187
  debug:
    pprof: true
`})
	c, err := Load(filepath.Join(dir, "config.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Agent.IsWriteOnly() || c.Agent.HealthListen != "127.0.0.1:9187" || !c.Agent.Debug.PProf {
		t.Errorf("agent = %+v", c.Agent)
	}
	sock, err := c.AgentSocket()
	want := filepath.Join(c.WorkDir, "agent.sock")
	if runtime.GOOS == "windows" {
		want = WindowsPipe
	}
	if err != nil || sock != want {
		t.Errorf("default socket = %q, %v; want %q", sock, err, want)
	}

	// write_only can be turned off, and the socket set, from the
	// environment too.
	abs := filepath.Join(t.TempDir(), "pentym.sock")
	c, err = Load(filepath.Join(dir, "config.yaml"), []string{"PENTYM_AGENT__WRITE_ONLY=false", "PENTYM_AGENT__SOCKET=" + abs})
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent.IsWriteOnly() {
		t.Error("write_only: false was ignored")
	}
	if sock, _ := c.AgentSocket(); sock != abs {
		t.Errorf("socket = %q", sock)
	}
	if WindowsPipe != `\\.\pipe\pentym-agent` {
		t.Errorf("the default pipe is %q", WindowsPipe)
	}
}

func TestAgentSectionInvalid(t *testing.T) {
	for _, tt := range []struct{ yaml, want string }{
		{"socket: agent.sock", `agent.socket must be an absolute path or a named pipe (\\.\pipe\name), not "agent.sock"`},
		{"health_listen: 9187", `agent.health_listen must be host:port, such as 127.0.0.1:9187, not "9187"`},
		{"health_listen: 127.0.0.1:http", `agent.health_listen: "http" is not a TCP port`},
		{"health_listen: ':9187'", "agent.health_listen needs an address to bind, such as 127.0.0.1 or the pod IP; it is never bound to every interface"},
	} {
		dir := files(t, map[string]string{"config.yaml": "agent:\n  " + tt.yaml + "\n"})
		_, err := Load(filepath.Join(dir, "config.yaml"), nil)
		wantConfigError(t, err, IDInvalid, tt.want)
	}
	// A named pipe is accepted as it is.
	dir := files(t, map[string]string{"config.yaml": "agent:\n  socket: '\\\\.\\pipe\\pentym-test'\n"})
	if _, err := Load(filepath.Join(dir, "config.yaml"), nil); err != nil {
		t.Errorf("named pipe: %v", err)
	}
}
