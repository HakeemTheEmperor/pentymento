// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package mongodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// roleRef is a granted role.
type roleRef struct {
	Role string `bson:"role"`
	DB   string `bson:"db"`
}

func (r roleRef) String() string { return r.Role + "@" + r.DB }

// privilege is one granted privilege. A nil DB or Collection is absent
// from the resource; an empty one means every database or collection.
type privilege struct {
	Resource struct {
		DB          *string `bson:"db"`
		Collection  *string `bson:"collection"`
		AnyResource bool    `bson:"anyResource"`
	} `bson:"resource"`
	Actions []string `bson:"actions"`
}

// connStatus is the reply to connectionStatus with privileges.
type connStatus struct {
	AuthInfo struct {
		Users []struct {
			User string `bson:"user"`
			DB   string `bson:"db"`
		} `bson:"authenticatedUsers"`
		Roles      []roleRef   `bson:"authenticatedUserRoles"`
		Privileges []privilege `bson:"authenticatedUserPrivileges"`
	} `bson:"authInfo"`
}

// dumpRoles are built-in roles that let mongodump read every database.
var dumpRoles = []roleRef{{"backup", "admin"}, {"root", "admin"}, {"__system", "admin"}}

// writeRoles are built-in roles that can change data, users or the
// cluster: more privilege than backups need (SDD 8.5).
var writeRoles = map[string]bool{
	"root": true, "__system": true, "restore": true, "readWriteAnyDatabase": true, "dbAdminAnyDatabase": true,
	"userAdminAnyDatabase": true, "clusterAdmin": true, "clusterManager": true, "hostManager": true,
	"dbOwner": true, "readWrite": true, "dbAdmin": true, "userAdmin": true,
}

// canFindAll reports whether privileges allow find on every collection,
// as a custom role might grant instead of backup.
func canFindAll(privs []privilege) bool {
	for _, p := range privs {
		r := p.Resource
		all := r.AnyResource || (r.DB != nil && *r.DB == "" && r.Collection != nil && *r.Collection == "")
		if all {
			for _, a := range p.Actions {
				if a == "find" {
					return true
				}
			}
		}
	}
	return false
}

// privilegeFindings judges the backup user's roles: mongodump needs the
// backup role (or find on every collection) and, on a replica set, read
// on the oplog; roles that can write are more than it needs.
func privilegeFindings(user string, roles []roleRef, privs []privilege, oplogDenied bool) []engine.Finding {
	var out []engine.Finding
	has := func(r roleRef) bool {
		for _, g := range roles {
			if g == r {
				return true
			}
		}
		return false
	}
	dump := canFindAll(privs)
	for _, r := range dumpRoles {
		dump = dump || has(r)
	}
	if !dump {
		out = append(out, engine.Finding{Level: msg.LevelFailed, ID: IDPrivilege,
			Fact: user + " lacks the backup role, which mongodump needs",
			Next: fmt.Sprintf(`db.getSiblingDB("admin").grantRolesToUser(%q, [{role: "backup", db: "admin"}])`, user)})
	}
	if oplogDenied {
		out = append(out, engine.Finding{Level: msg.LevelFailed, ID: IDPrivilege,
			Fact: user + " cannot read the oplog (local.oplog.rs), which snapshots of a replica set need",
			Next: fmt.Sprintf(`db.getSiblingDB("admin").grantRolesToUser(%q, [{role: "read", db: "local"}])`, user)})
	}
	var extra []string
	for _, r := range roles {
		if writeRoles[r.Role] {
			extra = append(extra, r.String())
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		out = append(out, engine.Finding{Level: msg.LevelWarning, ID: IDTooMuchPrivilege,
			Fact: fmt.Sprintf("%s has %s, which is more privilege than backups need", user, strings.Join(extra, ", ")),
			Next: "use a user with only backup and read on local: pentym init --print-mongo prints one"})
	}
	return out
}

// oplogDenied reports whether the user is refused reading the oplog.
func (s *session) oplogDenied(ctx context.Context) (bool, error) {
	err := s.client.Database("local").Collection("oplog.rs").
		FindOne(ctx, bson.D{}, options.FindOne().SetProjection(bson.D{{Key: "ts", Value: 1}})).Err()
	var ce mongo.CommandError
	switch {
	case err == nil, errors.Is(err, mongo.ErrNoDocuments):
		return false, nil
	case errors.As(err, &ce) && ce.Code == 13: // Unauthorized
		return true, nil
	}
	return false, fmt.Errorf("read the oplog: %w", err)
}

// Check connects as the backup user and checks what snapshots need: a
// supported version, a replica set or standalone server (a standalone
// gets snapshots only), the backup role and read on the oplog, and the
// mongodump tool. Roles that can write are reported as more privilege
// than needed (SDD 8.5).
func (e *Engine) Check(ctx context.Context, t engine.Target) (*engine.CheckResult, error) {
	s, err := e.connect(ctx, t)
	if err != nil {
		return nil, err
	}
	defer s.close(ctx)
	version, err := s.version(ctx)
	if err != nil {
		return nil, err
	}
	res := &engine.CheckResult{ServerVersion: version, Role: s.user}
	add := func(level msg.Level, id msg.ID, fact, next string) {
		res.Findings = append(res.Findings, engine.Finding{Level: level, ID: id, Fact: fact, Next: next})
	}
	if !supported(version) {
		add(msg.LevelFailed, IDVersion, "MongoDB "+version+" is not supported; pentym supports "+strings.Join(Versions, ", "),
			"upgrade the server to a supported version")
	}
	switch {
	case s.hello.sharded():
		add(msg.LevelFailed, IDSharded, "the uri points at a mongos router; sharded clusters are not supported",
			"point uri at a replica set or a standalone server")
		return res, nil
	case !s.hello.replicaSet():
		add(msg.LevelWarning, IDStandalone,
			"this is a standalone server with no oplog, so pentym takes snapshots only: no point-in-time recovery",
			"run it as a single-node replica set to get point-in-time recovery")
	}
	var st connStatus
	if err := s.client.Database("admin").RunCommand(ctx,
		bson.D{{Key: "connectionStatus", Value: 1}, {Key: "showPrivileges", Value: true}}).Decode(&st); err != nil {
		return nil, fmt.Errorf("read the user's roles: %w", err)
	}
	if len(st.AuthInfo.Users) > 0 {
		if res.Role == "" {
			res.Role = st.AuthInfo.Users[0].User
		}
		denied := false
		if s.hello.replicaSet() {
			if denied, err = s.oplogDenied(ctx); err != nil {
				return nil, err
			}
		}
		res.Findings = append(res.Findings, privilegeFindings(res.Role, st.AuthInfo.Roles, st.AuthInfo.Privileges, denied)...)
	}
	res.Findings = append(res.Findings, e.toolFindings(ctx)...)
	return res, nil
}

// minToolsMajor is the first release of the MongoDB Database Tools,
// versioned apart from the server; older mongodump came with the server.
const minToolsMajor = 100

// toolFindings checks mongodump is present and is the Database Tools'.
func (e *Engine) toolFindings(ctx context.Context) []engine.Finding {
	failed := func(fact, next string) []engine.Finding {
		return []engine.Finding{{Level: msg.LevelFailed, Fact: fact, Next: next}}
	}
	bin, err := e.findTool("mongodump")
	if err != nil {
		return failed(err.Error(), "install the MongoDB Database Tools, or list their directory in tools.mongodb")
	}
	v, err := e.Runner.ToolVersion(ctx, "mongodump", bin)
	if err != nil {
		return failed("mongodump --version failed: "+err.Error(), "check the MongoDB Database Tools installation")
	}
	if v.Major < minToolsMajor {
		return failed("mongodump "+v.String()+" is from the legacy server tools; pentym needs the MongoDB Database Tools (100 or later)",
			"install the MongoDB Database Tools, or list their directory in tools.mongodb")
	}
	return nil
}

// Inventory is a MongoDB snapshot's collection summary: what the server
// held, for verification to compare with later.
type Inventory struct {
	ServerVersion string `json:"server_version"`
	// ReplicaSet is the set's name; empty for a standalone.
	ReplicaSet string            `json:"replica_set,omitempty"`
	Databases  []DatabaseSummary `json:"databases"`
}

// DatabaseSummary describes one database.
type DatabaseSummary struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	// Collections counts collections (including time series) and Views
	// counts views, leaving out system collections. EstimatedDocuments
	// sums the collections' metadata counts, so it is approximate.
	Collections        int    `json:"collections"`
	Views              int    `json:"views"`
	EstimatedDocuments int64  `json:"estimated_documents"`
	Error              string `json:"error,omitempty"`
}

// internalDBs hold the server's own state, which mongodump leaves out.
var internalDBs = map[string]bool{"local": true, "config": true}

// Inventory summarizes the server: every database but local and config,
// with its size, collections and estimated documents. A database the
// backup user cannot read is listed with the reason.
func (e *Engine) Inventory(ctx context.Context, t engine.Target) (json.RawMessage, error) {
	s, err := e.connect(ctx, t)
	if err != nil {
		return nil, err
	}
	defer s.close(ctx)
	version, err := s.version(ctx)
	if err != nil {
		return nil, err
	}
	inv := Inventory{ServerVersion: version, ReplicaSet: s.hello.SetName, Databases: []DatabaseSummary{}}
	dbs, err := s.client.ListDatabases(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	for _, d := range dbs.Databases {
		if internalDBs[d.Name] {
			continue
		}
		sum := DatabaseSummary{Name: d.Name, SizeBytes: d.SizeOnDisk}
		if err := summarize(ctx, s.client.Database(d.Name), &sum); err != nil {
			sum.Error = err.Error()
		}
		inv.Databases = append(inv.Databases, sum)
	}
	sort.Slice(inv.Databases, func(i, j int) bool { return inv.Databases[i].Name < inv.Databases[j].Name })
	return json.Marshal(inv)
}

// summarize counts a database's collections, views and estimated
// documents.
func summarize(ctx context.Context, db *mongo.Database, s *DatabaseSummary) error {
	specs, err := db.ListCollectionSpecifications(ctx, bson.D{})
	if err != nil {
		return err
	}
	for _, c := range specs {
		if strings.HasPrefix(c.Name, "system.") {
			continue
		}
		if c.Type == "view" {
			s.Views++
			continue
		}
		s.Collections++
		n, err := db.Collection(c.Name).EstimatedDocumentCount(ctx)
		if err != nil {
			return fmt.Errorf("count %s: %w", c.Name, err)
		}
		s.EstimatedDocuments += n
	}
	return nil
}
