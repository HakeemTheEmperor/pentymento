// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPostgresTarget(t *testing.T) {
	ca := filepath.Join(t.TempDir(), "ca.pem")
	dir := files(t, map[string]string{"config.yaml": `
targets:
  prod-pg:
    engine: postgres
    host: db.internal
    port: 5433
    database: app
    sslmode: verify-full
    sslrootcert: '` + ca + `'
    credentials:
      backup:
        user: pentym_backup
        password: env:PG_BACKUP_PW
`})
	c, err := Load(filepath.Join(dir, "config.yaml"), []string{"PENTYM_TARGETS__PROD_PG__PORT=5434"})
	if err != nil {
		t.Fatal(err)
	}
	tg := c.Targets["prod-pg"]
	if tg.Host != "db.internal" || tg.Port != 5434 || tg.Database != "app" || tg.SSLMode != "verify-full" || tg.SSLRootCert != ca ||
		tg.Credentials.Backup.User != "pentym_backup" || tg.Credentials.Backup.Password.String() != "env:PG_BACKUP_PW" {
		t.Errorf("target = %+v", tg)
	}
}

func TestPostgresTargetInvalid(t *testing.T) {
	for _, tt := range []struct{ yaml, want string }{
		{"port: 70000", "target t: port 70000 is not a TCP port"},
		{"sslmode: strict", `target t: sslmode must be one of disable, allow, prefer, require, verify-ca, verify-full, not "strict"`},
		{"sslkey: key.pem", `target t: sslkey must be an absolute path, not "key.pem"`},
	} {
		dir := files(t, map[string]string{"config.yaml": "targets:\n  t:\n    engine: postgres\n    " + tt.yaml + "\n"})
		_, err := Load(filepath.Join(dir, "config.yaml"), nil)
		wantConfigError(t, err, IDInvalid, tt.want)
	}
	// A password written in the file is a plaintext secret.
	dir := files(t, map[string]string{"config.yaml": "targets:\n  t:\n    engine: postgres\n    credentials:\n      backup:\n        user: u\n        password: hunter2\n"})
	_, err := Load(filepath.Join(dir, "config.yaml"), nil)
	wantConfigError(t, err, IDPlaintextRejected, "targets.t.credentials.backup.password")
}

func TestMongoDBTarget(t *testing.T) {
	dir := files(t, map[string]string{"config.yaml": `
targets:
  prod-mg:
    engine: mongodb
    uri: mongodb://db1:27017,db2:27017/?replicaSet=rs0&tls=true
    credentials:
      backup:
        user: pentym_backup
        password: env:MG_BACKUP_PW
`})
	c, err := Load(filepath.Join(dir, "config.yaml"), []string{"PENTYM_TARGETS__PROD_MG__URI=mongodb+srv://cluster.internal/?authSource=admin"})
	if err != nil {
		t.Fatal(err)
	}
	tg := c.Targets["prod-mg"]
	if tg.URI != "mongodb+srv://cluster.internal/?authSource=admin" || tg.Credentials.Backup.User != "pentym_backup" ||
		tg.Credentials.Backup.Password.String() != "env:MG_BACKUP_PW" {
		t.Errorf("target = %+v", tg)
	}
}

func TestMongoDBTargetInvalid(t *testing.T) {
	for _, tt := range []struct{ engine, yaml, want string }{
		{"mongodb", "uri: postgres://db", "target t: uri must start with mongodb:// or mongodb+srv://"},
		{"mongodb", "uri: 'mongodb://'", "target t: uri names no host"},
		{"mongodb", "uri: 'mongodb://ada:hunter2@db/'", "target t: uri must not hold a user name or password; set credentials.backup.user, and its password as a secret reference"},
		{"mongodb", "uri: 'mongodb://ada@db'", "target t: uri must not hold a user name or password; set credentials.backup.user, and its password as a secret reference"},
		{"mongodb", "uri: 'mongodb://db/?tls=true&tlsCertificateKeyFilePassword=hunter2'", "target t: uri must not hold a password (tlsCertificateKeyFilePassword)"},
		{"mongodb", "host: db", "target t: host is for postgres targets; a mongodb target sets its connection in uri"},
		{"mongodb", "port: 27017", "target t: port is for postgres targets; a mongodb target sets its connection in uri"},
		{"postgres", "uri: mongodb://db", "target t: uri is for mongodb targets; a postgres target uses host, port and database"},
	} {
		dir := files(t, map[string]string{"config.yaml": "targets:\n  t:\n    engine: " + tt.engine + "\n    " + tt.yaml + "\n"})
		_, err := Load(filepath.Join(dir, "config.yaml"), nil)
		wantConfigError(t, err, IDInvalid, tt.want)
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the error quotes the password: %v", err)
		}
	}
}
