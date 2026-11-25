// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package instance

import (
	"context"

	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// Spec asks for a side instance (SDD 4.3).
type Spec struct {
	// Engine is "postgres" or "mongodb".
	Engine string
	// Version is the major version to match: "17" for Postgres, "8.0" for
	// MongoDB.
	Version string
	// DataDir is the restore directory the instance runs on.
	DataDir string
	// RunID tags the instance (container labels, logs) so leftovers of a
	// crashed run can be found and removed.
	RunID string
}

// Conn is how to reach a running side instance. It listens on 127.0.0.1
// only, and its superuser password is random and never shown.
type Conn struct {
	Host     string
	Port     int
	User     string
	Password secrets.Secret
}

// Instance is a running side instance.
type Instance interface {
	// Conn returns the connection details.
	Conn() Conn
	// Stop stops the instance and removes everything it created except
	// the data directory, which belongs to the caller. It is safe to call
	// more than once.
	Stop(ctx context.Context) error
}

// Provider starts side instances: local binaries or Docker (C12b).
type Provider interface {
	// Name is "local" or "docker", as in the instance_provider setting.
	Name() string
	// Start starts an instance for spec, or explains why it cannot.
	Start(ctx context.Context, spec Spec) (Instance, error)
}
