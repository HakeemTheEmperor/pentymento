// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package instance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
	"github.com/HakeemTheEmperor/pentymento/internal/toolrun"
)

// Container labels that identify pentym's side instances, so leftovers of
// a crashed run can be found and removed.
const (
	labelRunID     = "pentym.run-id"
	labelOwnerPID  = "pentym.owner-pid"
	labelOwnerHost = "pentym.owner-host"
	labelEngine    = "pentym.engine"
)

// Docker runs side instances in containers from the official images,
// postgres:<major> and mongo:<version>, with the data directory mounted
// (SDD 4.3). It drives the docker CLI through the tool runner.
type Docker struct{ env *Env }

// NewDocker returns the Docker provider.
func NewDocker(env *Env) *Docker { return &Docker{env: env} }

// Name is "docker".
func (*Docker) Name() string { return "docker" }

// docker runs a docker command and returns its trimmed stdout.
func (d *Docker) docker(ctx context.Context, runID string, files []*toolrun.SecretFile, args ...string) (string, error) {
	bin, err := d.env.lookPath(exe("docker"))
	if err != nil {
		return "", errors.New("the docker command is not on PATH")
	}
	var out bytes.Buffer
	err = d.env.Runner.Run(ctx, &toolrun.Cmd{Tool: "docker", Path: bin, Args: args, Stdout: &out, RunID: runID, Files: files})
	return strings.TrimSpace(out.String()), err
}

// ServerVersion returns the Docker daemon's version, or why it cannot be
// reached.
func (d *Docker) ServerVersion(ctx context.Context) (string, error) {
	v, err := d.docker(ctx, "", nil, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		var ee *toolrun.ExitError
		if errors.As(err, &ee) {
			return "", errors.New("the Docker daemon is not reachable; start Docker or check that this user may use it")
		}
		return "", err
	}
	return v, nil
}

// images and container paths per engine.
var dockerEngines = map[string]struct {
	image, dataPath string
	port            int
}{
	Postgres: {"postgres", "/var/lib/postgresql/data", 5432},
	MongoDB:  {"mongo", "/data/db", 27017},
}

type dockerInstance struct {
	d       *Docker
	spec    Spec
	name    string
	conn    Conn
	once    sync.Once
	stopErr error
}

func (i *dockerInstance) Conn() Conn { return i.conn }

// Start runs a container over spec.DataDir. An empty data directory is
// initialized by the image with a random superuser password, passed in an
// owner-only --env-file so it never appears on a command line. The port is
// published on 127.0.0.1 only. On Linux the container runs as pentym's
// user, so the data files stay pentym's.
func (d *Docker) Start(ctx context.Context, spec Spec) (Instance, error) {
	eng, ok := dockerEngines[spec.Engine]
	if !ok {
		return nil, fmt.Errorf("unknown engine %q", spec.Engine)
	}
	if _, err := d.ServerVersion(ctx); err != nil {
		return nil, err
	}
	empty, err := emptyDir(spec.DataDir)
	if err != nil {
		return nil, err
	}
	inst := &dockerInstance{d: d, spec: spec, name: "pentym-" + spec.RunID + "-" + spec.Engine,
		conn: Conn{Host: "127.0.0.1", User: superuser}}
	args := []string{"run", "--detach", "--name", inst.name,
		"--label", labelRunID + "=" + spec.RunID,
		"--label", labelOwnerPID + "=" + strconv.Itoa(os.Getpid()),
		"--label", labelOwnerHost + "=" + d.env.Host,
		"--label", labelEngine + "=" + spec.Engine,
		"--publish", "127.0.0.1::" + strconv.Itoa(eng.port),
		"--volume", spec.DataDir + ":" + eng.dataPath,
	}
	if runtime.GOOS == "linux" {
		args = append(args, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	}
	var files []*toolrun.SecretFile
	if empty {
		inst.conn.Password = newPassword()
		envFile, err := toolrun.WriteSecretFile(d.env.Runner.SecretDir, ".env", initEnv(spec.Engine, inst.conn.Password))
		if err != nil {
			return nil, err
		}
		files = append(files, envFile)
		args = append(args, "--env-file", envFile.Path)
	}
	if spec.Engine == Postgres {
		args = append(args, "--env", "PGDATA="+eng.dataPath)
	}
	args = append(args, eng.image+":"+spec.Version)
	fail := func(err error) (Instance, error) {
		_ = inst.Stop(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("docker %s %s: %w", spec.Engine, spec.Version, err)
	}
	if _, err := d.docker(ctx, spec.RunID, files, args...); err != nil {
		return fail(err)
	}
	addr, err := d.docker(ctx, spec.RunID, nil, "port", inst.name, strconv.Itoa(eng.port)+"/tcp")
	if err != nil {
		return fail(err)
	}
	first, _, _ := strings.Cut(addr, "\n")
	_, portText, err := net.SplitHostPort(first)
	if err != nil {
		return fail(fmt.Errorf("docker port %s: %q", inst.name, first))
	}
	if inst.conn.Port, err = strconv.Atoi(portText); err != nil {
		return fail(err)
	}
	if !empty {
		return inst, nil // access is the engine's to arrange
	}
	if err := awaitReady(ctx, spec, inst.conn); err != nil {
		return fail(fmt.Errorf("%w; see docker logs %s", err, inst.name))
	}
	return inst, nil
}

// initEnv is the --env-file content that makes the image create the
// superuser.
func initEnv(engine string, pw secrets.Secret) secrets.Secret {
	var b []byte
	if engine == Postgres {
		b = append(b, "POSTGRES_USER="+superuser+"\nPOSTGRES_PASSWORD="...)
	} else {
		b = append(b, "MONGO_INITDB_ROOT_USERNAME="+superuser+"\nMONGO_INITDB_ROOT_PASSWORD="...)
	}
	b = append(b, pw.Expose()...)
	b = append(b, '\n')
	s := secrets.New(b)
	clear(b)
	return s
}

// Stop stops the container gently, then removes it. The data directory is
// left to the caller.
func (i *dockerInstance) Stop(ctx context.Context) error {
	i.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout+10*time.Second)
		defer cancel()
		_, _ = i.d.docker(ctx, i.spec.RunID, nil, "stop", "--time", strconv.Itoa(int(stopTimeout.Seconds())), i.name)
		_, err := i.d.docker(ctx, i.spec.RunID, nil, "rm", "--force", "--volumes", i.name)
		var ee *toolrun.ExitError
		if errors.As(err, &ee) && containerGone(ee) {
			err = nil
		}
		i.stopErr = err
	})
	return i.stopErr
}

// containerGone reports whether docker failed because the container does
// not exist, which for removal is success.
func containerGone(ee *toolrun.ExitError) bool {
	for _, l := range ee.Stderr {
		if strings.Contains(strings.ToLower(l), "no such container") {
			return true
		}
	}
	return false
}

// leftovers removes containers this host started for owners that are no
// longer running. It returns how many it removed.
func (d *Docker) leftovers(ctx context.Context) (int, error) {
	out, err := d.docker(ctx, "", nil, "ps", "--all", "--filter", "label="+labelOwnerHost+"="+d.env.Host,
		"--format", `{{.ID}} {{.Label "`+labelOwnerPID+`"}}`)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		id, pidText, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err == nil && (pid == os.Getpid() || toolrun.PIDAlive(pid)) {
			continue
		}
		if _, err := d.docker(ctx, "", nil, "rm", "--force", "--volumes", id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
