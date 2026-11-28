// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package postgres

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

func TestRecoveryTarget(t *testing.T) {
	at := time.Date(2026, 11, 22, 14, 3, 12, 500000000, time.FixedZone("WAT", 3600))
	for p, want := range map[engine.Point]string{
		{Time: at}:               "recovery_target_time = '2026-11-22 13:03:12.5+00'\n",
		{XID: "8812"}:            "recovery_target_xid = '8812'\n",
		{Before: "0/3A00F128"}:   "recovery_target_lsn = '0/3A00F128'\n",
		{Before: "1/0/3A00F128"}: "recovery_target_lsn = '0/3A00F128'\n",
	} {
		if got := recoveryTarget(p); got != want {
			t.Errorf("recoveryTarget(%+v) = %q, want %q", p, got, want)
		}
	}
}

func TestWriteRecovery(t *testing.T) {
	dir := t.TempDir()
	if err := prepareConfig(dir); err != nil {
		t.Fatal(err)
	}
	if err := writeRecovery(dir, "recovery_target_xid = '8812'\n"); err != nil {
		t.Fatal(err)
	}
	conf, _ := readTestFile(filepath.Join(dir, "postgresql.conf"))
	for _, want := range []string{sideConf, "hot_standby = off\n", "recovery_target_xid = '8812'\n", "recovery_target_inclusive = off\n",
		"recovery_target_action = 'promote'\n", "recovery_target_timeline = 'latest'\n"} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("postgresql.conf lacks %q:\n%s", want, conf)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "recovery.signal")); err != nil {
		t.Errorf("recovery.signal: %v", err)
	}
	// Without a target, the settings are replaced, not added to.
	if err := writeRecovery(dir, ""); err != nil {
		t.Fatal(err)
	}
	conf, _ = readTestFile(filepath.Join(dir, "postgresql.conf"))
	if strings.Contains(string(conf), "recovery_target_xid") || strings.Contains(string(conf), "recovery_target_action") ||
		strings.Count(string(conf), "hot_standby") != 1 {
		t.Errorf("without a target:\n%s", conf)
	}
}

// fakeLogs serves named WAL files.
type fakeLogs struct {
	names []string
	data  map[string][]byte
}

func (f fakeLogs) Names(context.Context, engine.Position, engine.Position) ([]string, error) {
	return f.names, nil
}

func (f fakeLogs) Segment(_ context.Context, name string) ([]byte, error) {
	d, ok := f.data[name]
	if !ok {
		return nil, errors.New("no " + name)
	}
	return d, nil
}

func TestStageWAL(t *testing.T) {
	dir := t.TempDir()
	logs := fakeLogs{
		names: []string{"00000002.history", "000000010000000000000003", "000000020000000000000004.partial"},
		data: map[string][]byte{
			"00000002.history":                 []byte("1\t0/4000000\tno recovery target specified\n"),
			"000000010000000000000003":         bytes.Repeat([]byte{1}, 1<<20),
			"000000020000000000000004.partial": []byte("partial"),
		},
	}
	req := engine.RestoreRequest{Logs: logs, DataDir: dir, Backup: engine.BaseBackup{WALSegmentSize: 1 << 20}, Point: engine.Point{XID: "1"}}
	if err := (&Engine{}).stageWAL(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	wal := filepath.Join(dir, "pg_wal")
	if b, _ := readTestFile(filepath.Join(wal, "00000002.history")); !bytes.Equal(b, logs.data["00000002.history"]) {
		t.Errorf("history = %q", b)
	}
	if b, _ := readTestFile(filepath.Join(wal, "000000010000000000000003")); len(b) != 1<<20 {
		t.Errorf("segment is %d bytes", len(b))
	}
	b, _ := readTestFile(filepath.Join(wal, "000000020000000000000004"))
	if len(b) != 1<<20 || !bytes.HasPrefix(b, []byte("partial")) || b[len(b)-1] != 0 {
		t.Errorf("the partial was not padded to the segment size: %d bytes", len(b))
	}
	if _, err := os.Stat(filepath.Join(wal, "000000020000000000000004.partial")); !os.IsNotExist(err) {
		t.Error("the partial was staged under its .partial name")
	}
	for _, bad := range []string{"../postgresql.conf", "backup_label", "00000001000000000000000G"} {
		req.Logs = fakeLogs{names: []string{bad}, data: map[string][]byte{bad: nil}}
		if err := (&Engine{}).stageWAL(context.Background(), req); err == nil {
			t.Errorf("staged %q", bad)
		}
	}
}

func TestNotReached(t *testing.T) {
	var me *msg.Error
	if err := notReached(engine.Point{XID: "8812"}); !errors.As(err, &me) || me.Code != msg.ExitUnrecoverable ||
		!strings.Contains(me.Message.Fact, "transaction 8812") {
		t.Errorf("notReached = %v", err)
	}
}

func TestRestorePointNeedsLogs(t *testing.T) {
	_, _, err := (&Engine{}).Restore(context.Background(), engine.RestoreRequest{Point: engine.Point{XID: "1"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "needs the stored WAL") {
		t.Errorf("Restore without logs = %v", err)
	}
}

// readTestFile reads a file the test wrote.
func readTestFile(path string) ([]byte, error) {
	return os.ReadFile(path) // #nosec G304 -- a file in the test's own directory
}
