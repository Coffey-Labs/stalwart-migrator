// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package recovery

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DefaultDockerBinary is what shells out unless a caller names another.
// Named rather than inlined because podman answers the same CLI closely
// enough to be worth pointing this at one day - see issue #3, where that is
// explicitly not this pass's work.
const DefaultDockerBinary = "docker"

// ContainerMount is one mount to give the recovery container. These come
// from the live container's own `docker inspect`, not from a template: the
// store has to be the one Stalwart was already using, and anything this
// tool invented instead would migrate an empty directory and report
// success.
type ContainerMount struct {
	Source      string // volume name or host path
	Destination string // path inside the container
	ReadOnly    bool
}

func (m ContainerMount) arg() string {
	s := m.Source + ":" + m.Destination
	if m.ReadOnly {
		s += ":ro"
	}
	return s
}

// ContainerLauncher runs the target version as a throwaway container
// against the live data, which is what a container deployment's recovery
// cycle is. See ARCHITECTURE.md §4.4 and issue #3.
//
// The live container must already be stopped. Two Stalwart processes
// against one store is corruption, and nothing here can detect that the
// other one is running - `run` stops the service before this phase for
// exactly that reason.
type ContainerLauncher struct {
	// Image is the staged target image, by digest where possible: a tag
	// can move between staging and running, and then this would migrate
	// the store with something other than what was verified.
	Image string

	// Mounts is the live container's own mounts.
	Mounts []ContainerMount

	// Name for the recovery container. It must not be the live container's
	// name - that one still exists while stopped, and reusing it would
	// collide. Empty gets a generated one.
	Name string

	// Publish maps host to container ports, e.g. "127.0.0.1:8080:8080", so
	// the health check on this side can reach recovery mode's listener.
	Publish []string

	// DockerBinary defaults to DefaultDockerBinary.
	DockerBinary string
}

func (c ContainerLauncher) docker() string {
	if c.DockerBinary == "" {
		return DefaultDockerBinary
	}
	return c.DockerBinary
}

func (c ContainerLauncher) name() string {
	if c.Name != "" {
		return c.Name
	}
	return fmt.Sprintf("stalwart-migrate-recovery-%d", os.Getpid())
}

// Launch starts the container in the foreground and returns once the OS
// has started the client. Foreground rather than detached so the server's
// own output arrives on this process's pipes: a recovery boot that fails
// on a bind conflict or a rejected config says so immediately, and a
// detached container would leave that in `docker logs` for nobody.
func (c ContainerLauncher) Launch(ctx context.Context, o LaunchOptions) (Supervised, error) {
	if c.Image == "" {
		return nil, fmt.Errorf("recovery: no image to launch - stage the target image first")
	}
	if len(c.Mounts) == 0 {
		return nil, fmt.Errorf("recovery: no mounts for the recovery container - it would migrate an empty store and report success")
	}

	name := c.name()
	args := []string{"run", "--rm", "--name", name}
	if o.RecoveryMode {
		args = append(args,
			"-e", "STALWART_RECOVERY_MODE=1",
			"-e", fmt.Sprintf("STALWART_RECOVERY_ADMIN=%s:%s", o.AdminUser, o.AdminPassword),
		)
	}
	for _, e := range o.ExtraEnv {
		args = append(args, "-e", e)
	}
	for _, m := range c.Mounts {
		args = append(args, "-v", m.arg())
	}
	for _, p := range c.Publish {
		args = append(args, "-p", p)
	}
	args = append(args, c.Image)
	if o.ConfigPath != "" {
		args = append(args, "--config", o.ConfigPath)
	}

	proc := &Process{}
	if err := proc.start(exec.CommandContext(ctx, c.docker(), args...), "container "+name); err != nil {
		return nil, err
	}
	return &containerProcess{proc: proc, name: name, dockerBin: c.docker()}, nil
}

// containerProcess supervises the `docker run` client and, separately, the
// container it started.
//
// Those are two things, and conflating them is how a migration ends up
// with two processes on one store. Signals reach the container through an
// attached client, so the ordinary path works - but Process.Stop escalates
// to SIGKILL when the grace period expires, and killing the client does
// not kill the container. It would be left running, holding the store
// open, while the run moved on to the next phase believing it had stopped.
// So the container is stopped by name first, and its absence confirmed.
type containerProcess struct {
	proc      *Process
	name      string
	dockerBin string
}

func (c *containerProcess) Output() string { return c.proc.Output() }

func (c *containerProcess) Stop(gracePeriod time.Duration) error {
	if gracePeriod <= 0 {
		gracePeriod = 10 * time.Second
	}
	// docker's own timeout, so it SIGKILLs the container's PID 1 if the
	// server does not exit - the container's equivalent of Process.Stop's
	// escalation, done where it actually reaches the process.
	secs := strconv.Itoa(int(gracePeriod.Seconds()))
	stopOut, stopErr := exec.Command(c.docker(), "stop", "--time", secs, c.name).CombinedOutput()

	// Reap the client regardless: it owns the output buffer, and its
	// output is most valuable exactly when the stop went wrong.
	procErr := c.proc.Stop(gracePeriod)

	if stopErr != nil {
		// A container that has already exited is not an error - `--rm`
		// means a clean shutdown removes it before this runs.
		if gone, checkErr := c.absent(); checkErr == nil && gone {
			return procErr
		}
		return fmt.Errorf("recovery: stop container %s: %w (%s)", c.name, stopErr, strings.TrimSpace(string(stopOut)))
	}
	if gone, checkErr := c.absent(); checkErr == nil && !gone {
		return fmt.Errorf("recovery: container %s is still running after being stopped - it still holds the store open, "+
			"so nothing should touch that data until it is gone", c.name)
	}
	return procErr
}

func (c *containerProcess) docker() string {
	if c.dockerBin == "" {
		return DefaultDockerBinary
	}
	return c.dockerBin
}

// absent reports whether the container is gone or stopped.
func (c *containerProcess) absent() (bool, error) {
	out, err := exec.Command(c.docker(), "inspect", "-f", "{{.State.Running}}", c.name).Output()
	if err != nil {
		// inspect fails when there is no such container, which with --rm
		// is the successful outcome.
		return true, nil
	}
	return strings.TrimSpace(string(out)) != "true", nil
}

var _ Launcher = ContainerLauncher{}
var _ Supervised = (*containerProcess)(nil)
