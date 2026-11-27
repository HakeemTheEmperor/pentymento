// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// Config is the pentym configuration. Keys are added by the chunks that use
// them; unknown keys are rejected.
type Config struct {
	// Include lists config files applied before this one.
	Include []string `yaml:"include"`
	// AllowPlaintextSecrets permits secret values written directly in the
	// config, provided the file is not readable by group or others.
	AllowPlaintextSecrets bool `yaml:"allow_plaintext_secrets"`
	// Repository is where backups are stored.
	Repository Repository `yaml:"repository"`
	// InstanceProvider lists the side-instance providers to try, in order:
	// "docker" and "local". Empty means DefaultInstanceProviders.
	InstanceProvider []string `yaml:"instance_provider"`
	// WorkDir holds side-instance files and temporary credential files.
	// Empty means DefaultWorkDir.
	WorkDir string `yaml:"work_dir"`
	// Tools are directories searched for database binaries before PATH.
	Tools Tools `yaml:"tools"`
	// Targets are the configured databases, by name.
	Targets map[string]Target `yaml:"targets"`
	// Agent configures pentym agent.
	Agent Agent `yaml:"agent"`
	// Retention configures what the repository keeps.
	Retention Retention `yaml:"retention"`
	// CatchUp configures catch-up.
	CatchUp CatchUp `yaml:"catch_up"`
}

// Agent configures the long-running agent (SDD 9).
type Agent struct {
	// WriteOnly makes the agent refuse to start when its key slot can
	// unlock the private key (SDD 8.4); nil means true.
	WriteOnly *bool `yaml:"write_only"`
	// Socket is the control socket: a Unix socket path, or a Windows
	// named pipe (\\.\pipe\name). Empty means the default from
	// AgentSocket.
	Socket string `yaml:"socket"`
	// HealthListen is an address (host:port) for the opt-in TCP health
	// port, which serves only /health, /ready and /metrics; empty means
	// off.
	HealthListen string `yaml:"health_listen"`
	// Debug holds debugging switches.
	Debug AgentDebug `yaml:"debug"`
}

// AgentDebug holds the agent's debugging switches.
type AgentDebug struct {
	// PProf serves Go's profiling endpoints on the control socket.
	PProf bool `yaml:"pprof"`
}

// IsWriteOnly reports whether the agent must refuse a key slot that can
// read backups; the default is true.
func (a Agent) IsWriteOnly() bool { return a.WriteOnly == nil || *a.WriteOnly }

// WindowsPipe is the agent's default control socket on Windows.
const WindowsPipe = `\\.\pipe\pentym-agent`

// AgentSocket returns the control socket's address: agent.socket, or by
// default <work_dir>/agent.sock (the named pipe WindowsPipe on Windows).
func (c *Config) AgentSocket() (string, error) {
	if c.Agent.Socket != "" {
		return c.Agent.Socket, nil
	}
	if runtime.GOOS == "windows" {
		return WindowsPipe, nil
	}
	dir, err := c.WorkDirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent.sock"), nil
}

// Repository is the repository location and how to unlock it.
type Repository struct {
	// Path is the directory of a repository on local storage. It must be
	// absolute. A repository is on local storage (Path) or in a bucket
	// (S3), never both.
	Path string `yaml:"path"`
	// S3 is a repository in an S3-compatible bucket.
	S3 S3 `yaml:"s3"`
	// Key is a reference to the passphrase, recovery code or age identity
	// that unlocks a key slot when there is no terminal to ask in.
	Key secrets.Ref `yaml:"key"`
}

// S3 is a repository in an S3-compatible bucket (PRD 7.8).
type S3 struct {
	Bucket string `yaml:"bucket"`
	// Prefix puts the repository under a "directory" of the bucket.
	Prefix string `yaml:"prefix"`
	Region string `yaml:"region"`
	// Endpoint is an S3-compatible service's URL (MinIO, R2, B2, Wasabi,
	// Spaces); empty means AWS. It must be https, except on the loopback
	// interface.
	Endpoint string `yaml:"endpoint"`
	// PathStyle puts the bucket in the URL path rather than the host name,
	// as many S3-compatible services need.
	PathStyle bool `yaml:"path_style"`
	// Credentials are static keys; empty means the AWS default credential
	// chain (environment, shared files, instance roles, IRSA, OIDC).
	Credentials S3Credentials `yaml:"credentials"`
}

// S3Credentials are static S3 keys, as secret references.
type S3Credentials struct {
	AccessKeyID     secrets.Ref `yaml:"access_key_id"`
	SecretAccessKey secrets.Ref `yaml:"secret_access_key"`
}

// IsSet reports whether any S3 setting is present.
func (s S3) IsSet() bool {
	return s.Bucket != "" || s.Prefix != "" || s.Region != "" || s.Endpoint != "" || s.PathStyle ||
		!s.Credentials.AccessKeyID.IsZero() || !s.Credentials.SecretAccessKey.IsZero()
}

// Location describes the repository's location for messages: its path,
// or s3://bucket/prefix.
func (r Repository) Location() string {
	if r.S3.IsSet() {
		loc := "s3://" + r.S3.Bucket
		if p := strings.Trim(r.S3.Prefix, "/"); p != "" {
			loc += "/" + p
		}
		return loc
	}
	return r.Path
}

// Configured reports whether the config names a repository.
func (r Repository) Configured() bool { return r.Path != "" || r.S3.IsSet() }

// Tools lists, per engine, directories holding its binaries (postgres,
// pg_ctl, pg_dump …; mongod, mongodump …). Several can be listed, such as
// one per Postgres major version; the version check picks the match.
type Tools struct {
	Postgres []string `yaml:"postgres"`
	MongoDB  []string `yaml:"mongodb"`
}

// Instance providers.
const (
	ProviderDocker = "docker"
	ProviderLocal  = "local"
)

// DefaultInstanceProviders is the provider order when none is configured
// (PRD 7.6).
var DefaultInstanceProviders = []string{ProviderDocker, ProviderLocal}

// Providers returns the configured provider order, or the default.
func (c *Config) Providers() []string {
	if len(c.InstanceProvider) == 0 {
		return append([]string(nil), DefaultInstanceProviders...)
	}
	return append([]string(nil), c.InstanceProvider...)
}

// DefaultWorkDir returns the work directory used when work_dir is not
// set: pentym/work in the user's cache directory.
func DefaultWorkDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("no work_dir configured and no cache directory: %w", err)
	}
	return filepath.Join(dir, "pentym", "work"), nil
}

// WorkDirPath returns work_dir, or the default.
func (c *Config) WorkDirPath() (string, error) {
	if c.WorkDir != "" {
		return c.WorkDir, nil
	}
	return DefaultWorkDir()
}

// Target is one configured database.
type Target struct {
	// Engine is postgres or mongodb.
	Engine string `yaml:"engine"`

	// Connection (Postgres). Host is a host name or a Unix socket
	// directory; empty means the libpq default.
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Database string `yaml:"database"`
	// SSLMode is a libpq sslmode: disable, allow, prefer (the default),
	// require, verify-ca or verify-full.
	SSLMode     string `yaml:"sslmode"`
	SSLRootCert string `yaml:"sslrootcert"`
	SSLCert     string `yaml:"sslcert"`
	SSLKey      string `yaml:"sslkey"`

	// Slot is a Postgres target's physical replication slot for WAL
	// capture; empty means DefaultSlot.
	Slot string `yaml:"slot"`

	// URI is a MongoDB target's connection string, such as
	// mongodb://db1:27017,db2:27017/?replicaSet=rs0&tls=true. It holds no
	// user name or password: those come from Credentials. Empty means
	// mongodb://localhost:27017.
	URI string `yaml:"uri"`

	// Credentials are the roles pentym connects as, one per purpose
	// (PRD 8.3).
	Credentials Credentials `yaml:"credentials"`
}

// Credentials are a target's roles by purpose. The restore credential is
// added by the chunk that uses it.
type Credentials struct {
	// Backup takes base backups.
	Backup Credential `yaml:"backup"`
	// Replication captures the log (Postgres WAL through the replication
	// slot); when its user is empty, capture uses Backup.
	Replication Credential `yaml:"replication"`
}

// CaptureCredential is the credential log capture connects with: the
// replication credential, or the backup credential when none is set.
func (c Credentials) CaptureCredential() Credential {
	if c.Replication.User != "" {
		return c.Replication
	}
	return c.Backup
}

// Credential is a role and, optionally, a reference to its password;
// certificates, peer authentication and .pgpass need none.
type Credential struct {
	User     string      `yaml:"user"`
	Password secrets.Ref `yaml:"password"`
}

// SSLModes are the accepted sslmode values.
var SSLModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

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
	if p := c.Repository.Path; p != "" && !filepath.IsAbs(p) {
		return fmt.Errorf("repository.path must be an absolute path, not %q", p)
	}
	if c.Repository.S3.IsSet() {
		if c.Repository.Path != "" {
			return errors.New("set repository.path or repository.s3, not both")
		}
		if err := c.Repository.S3.validate(); err != nil {
			return err
		}
	}
	if c.WorkDir != "" && !filepath.IsAbs(c.WorkDir) {
		return fmt.Errorf("work_dir must be an absolute path, not %q", c.WorkDir)
	}
	if err := c.Agent.validate(); err != nil {
		return err
	}
	seenProvider := map[string]bool{}
	for _, p := range c.InstanceProvider {
		if p != ProviderDocker && p != ProviderLocal {
			return fmt.Errorf("instance_provider: %q is not a provider; use docker and/or local", p)
		}
		if seenProvider[p] {
			return fmt.Errorf("instance_provider lists %s twice", p)
		}
		seenProvider[p] = true
	}
	for _, t := range []struct {
		engine string
		dirs   []string
	}{{"postgres", c.Tools.Postgres}, {"mongodb", c.Tools.MongoDB}} {
		for _, d := range t.dirs {
			if !filepath.IsAbs(d) {
				return fmt.Errorf("tools.%s: %q must be an absolute path", t.engine, d)
			}
		}
	}
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
		if err := t.validateConnection(name); err != nil {
			return err
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

// validateConnection checks a target's connection settings: Postgres
// targets use host, port, database and the ssl settings; MongoDB targets
// use uri.
func (t Target) validateConnection(name string) error {
	if t.Engine == "mongodb" {
		for _, f := range [][2]string{{"host", t.Host}, {"database", t.Database}, {"sslmode", t.SSLMode}, {"slot", t.Slot},
			{"sslrootcert", t.SSLRootCert}, {"sslcert", t.SSLCert}, {"sslkey", t.SSLKey}} {
			if f[1] != "" {
				return fmt.Errorf("target %s: %s is for postgres targets; a mongodb target sets its connection in uri", name, f[0])
			}
		}
		if t.Port != 0 {
			return fmt.Errorf("target %s: port is for postgres targets; a mongodb target sets its connection in uri", name)
		}
		return validateMongoURI(name, t.URI)
	}
	if t.URI != "" {
		return fmt.Errorf("target %s: uri is for mongodb targets; a postgres target uses host, port and database", name)
	}
	if t.Port < 0 || t.Port > 65535 {
		return fmt.Errorf("target %s: port %d is not a TCP port", name, t.Port)
	}
	if t.Slot != "" && !slotRE.MatchString(t.Slot) {
		return fmt.Errorf("target %s: slot must be 1-63 lowercase letters, digits and underscores, not %q", name, t.Slot)
	}
	if t.SSLMode != "" && !contains(SSLModes, t.SSLMode) {
		return fmt.Errorf("target %s: sslmode must be one of %s, not %q", name, strings.Join(SSLModes, ", "), t.SSLMode)
	}
	for _, f := range [][2]string{{"sslrootcert", t.SSLRootCert}, {"sslcert", t.SSLCert}, {"sslkey", t.SSLKey}} {
		if f[1] != "" && !filepath.IsAbs(f[1]) {
			return fmt.Errorf("target %s: %s must be an absolute path, not %q", name, f[0], f[1])
		}
	}
	return nil
}

// validateMongoURI checks the form of a MongoDB connection string without
// resolving it (a mongodb+srv URI needs DNS, which waits for connecting),
// and refuses one that holds credentials: the config holds secrets only
// as references. Errors never quote the URI.
func validateMongoURI(name, uri string) error {
	if uri == "" {
		return nil
	}
	rest, ok := strings.CutPrefix(uri, "mongodb://")
	if !ok {
		if rest, ok = strings.CutPrefix(uri, "mongodb+srv://"); !ok {
			return fmt.Errorf("target %s: uri must start with mongodb:// or mongodb+srv://", name)
		}
	}
	authority, query := rest, ""
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		authority = rest[:i]
		if _, q, found := strings.Cut(rest[i:], "?"); found {
			query = q
		}
	}
	if strings.Contains(authority, "@") {
		return fmt.Errorf("target %s: uri must not hold a user name or password; set credentials.backup.user, and its password as a secret reference", name)
	}
	if authority == "" {
		return fmt.Errorf("target %s: uri names no host", name)
	}
	for _, kv := range strings.Split(query, "&") {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToLower(k) {
		case "tlscertificatekeyfilepassword", "sslpemkeypassword":
			return fmt.Errorf("target %s: uri must not hold a password (%s)", name, k)
		}
	}
	return nil
}

// bucketRE is the form of an S3 bucket name.
var bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// prefixRE is the form of a repository prefix: key segments of the
// characters pentym uses in keys.
var prefixRE = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$`)

func (s S3) validate() error {
	switch {
	case s.Bucket == "":
		return errors.New("repository.s3.bucket is required")
	case !bucketRE.MatchString(s.Bucket):
		return fmt.Errorf("repository.s3.bucket %q is not a valid bucket name (3-63 lowercase letters, digits, . and -)", s.Bucket)
	case s.Region == "":
		return errors.New("repository.s3.region is required (for S3-compatible services, the region they expect, such as us-east-1 or auto)")
	}
	if p := strings.Trim(s.Prefix, "/"); p != "" && !prefixRE.MatchString(p) {
		return fmt.Errorf("repository.s3.prefix %q may hold only letters, digits, ., _, - and /, and no segment may start with a dot", s.Prefix)
	}
	if s.Endpoint != "" {
		u, err := url.Parse(s.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("repository.s3.endpoint %q must be a URL such as https://s3.example.com", s.Endpoint)
		}
		if u.Scheme == "http" && !loopback(u.Hostname()) {
			return fmt.Errorf("repository.s3.endpoint must use https; plain http is allowed only on the loopback interface, not %s", u.Host)
		}
	}
	if s.Credentials.AccessKeyID.IsZero() != s.Credentials.SecretAccessKey.IsZero() {
		return errors.New("repository.s3.credentials needs both access_key_id and secret_access_key, or neither for the default credential chain")
	}
	return nil
}

// loopback reports whether host is the local machine.
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a Agent) validate() error {
	if s := a.Socket; s != "" && !strings.HasPrefix(s, `\\.\pipe\`) && !filepath.IsAbs(s) {
		return fmt.Errorf(`agent.socket must be an absolute path or a named pipe (\\.\pipe\name), not %q`, s)
	}
	if h := a.HealthListen; h != "" {
		host, port, err := net.SplitHostPort(h)
		if err != nil {
			return fmt.Errorf("agent.health_listen must be host:port, such as 127.0.0.1:9187, not %q", h)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("agent.health_listen: %q is not a TCP port", port)
		}
		if host == "" {
			return errors.New("agent.health_listen needs an address to bind, such as 127.0.0.1 or the pod IP; it is never bound to every interface")
		}
	}
	return nil
}

// slotRE is the form of a Postgres replication slot name.
var slotRE = regexp.MustCompile(`^[a-z0-9_]{1,63}$`)

// DefaultSlot is a Postgres target's replication slot when none is set:
// pentym_ and the target name with - turned into _.
func DefaultSlot(target string) string {
	s := "pentym_" + strings.ReplaceAll(target, "-", "_")
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}

// SlotName returns the target's replication slot.
func (t Target) SlotName(name string) string {
	if t.Slot != "" {
		return t.Slot
	}
	return DefaultSlot(name)
}
