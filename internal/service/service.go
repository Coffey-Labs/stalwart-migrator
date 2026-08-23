package service

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Kind is how a Stalwart instance is run, and therefore how it has to be
// stopped and started. Preflight detects it (preflight.DetectDeploymentKind
// is an alias for this type's detector-side constants) and records it in
// the checkpoint's Topology, so a rollback days later controls the same
// thing the original run observed rather than re-guessing.
type Kind string

const (
	Systemd Kind = "systemd"
	Docker  Kind = "docker"
	Unknown Kind = "unknown"
)

// Options names the thing to control. Only the field matching Kind is used.
type Options struct {
	Kind          Kind
	UnitName      string // systemd; defaults to "stalwart"
	ContainerName string // docker; defaults to "stalwart"
}

// Controller stops and starts one Stalwart deployment. It is deliberately
// the only thing in this tool that shells out to systemctl or docker, for
// the same reason stalwartapi is the only thing that speaks JMAP: the
// commands that can take mail delivery down belong in one auditable place,
// not scattered across the phases that happen to need them.
type Controller interface {
	// Stop stops the service and returns once the command reports success.
	// It does not wait for the process to actually be gone - use WaitFor
	// for that, since both systemd and docker can report success while the
	// unit is still shutting down.
	Stop(ctx context.Context) error
	// Start starts the service.
	Start(ctx context.Context) error
	// Active reports whether the service is currently running (or still
	// transitioning into or out of running). An error means the state
	// couldn't be determined at all - which is different from, and must
	// never be silently collapsed into, "not running".
	Active(ctx context.Context) (bool, error)
	// ReloadConfig re-reads unit/service definitions after one has been
	// rewritten on disk. It's a no-op for deployments that don't have such
	// a step, so callers never need to branch on Kind.
	ReloadConfig(ctx context.Context) error
	// Target describes what this controller acts on, for operator-facing
	// messages ("stopped systemd unit stalwart").
	Target() string
}

// New returns a Controller for the given deployment. It refuses an Unknown
// (or unrecognized) kind rather than guessing: picking the wrong mechanism
// here means a rollback that reports "service stopped" while the old
// instance is still running and holding the data directory open, which is
// exactly the kind of quiet wrongness this tool exists to avoid.
func New(o Options) (Controller, error) {
	switch o.Kind {
	case Systemd:
		unit := o.UnitName
		if unit == "" {
			unit = "stalwart"
		}
		return &systemdController{unit: unit}, nil
	case Docker:
		name := o.ContainerName
		if name == "" {
			name = "stalwart"
		}
		return &dockerController{container: name}, nil
	case Unknown, "":
		return nil, fmt.Errorf("service: deployment kind is unknown - this tool won't guess how to stop Stalwart; re-run preflight, or name the systemd unit or docker container explicitly")
	default:
		return nil, fmt.Errorf("service: unsupported deployment kind %q", o.Kind)
	}
}

// WaitFor polls c.Active until it reports want, or timeout elapses. Both
// systemctl and docker return as soon as the *request* to stop succeeded,
// so without this a caller would move on to overwriting the data directory
// while the old process still had it open.
func WaitFor(ctx context.Context, c Controller, want bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		active, err := c.Active(ctx)
		if err != nil {
			lastErr = err
		} else if active == want {
			return nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	state := "stopped"
	if want {
		state = "running"
	}
	if lastErr != nil {
		return fmt.Errorf("service: %s was still not %s after %s, and its state couldn't be read: %w", c.Target(), state, timeout, lastErr)
	}
	return fmt.Errorf("service: %s was still not %s after %s", c.Target(), state, timeout)
}

type systemdController struct{ unit string }

func (s *systemdController) Target() string { return "systemd unit " + s.unit }

func (s *systemdController) Stop(ctx context.Context) error  { return s.run(ctx, "stop") }
func (s *systemdController) Start(ctx context.Context) error { return s.run(ctx, "start") }

func (s *systemdController) ReloadConfig(ctx context.Context) error {
	return runCommand(ctx, "systemctl", "daemon-reload")
}

func (s *systemdController) run(ctx context.Context, verb string) error {
	return runCommand(ctx, "systemctl", verb, s.unit)
}

// Active maps `systemctl is-active` output rather than its exit status:
// the command exits non-zero for every not-active state, so treating a
// non-zero exit as a failure to read the state would make "inactive" -
// the answer we most want - look like an error.
func (s *systemdController) Active(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "is-active", s.unit).Output()
	state := strings.TrimSpace(string(out))
	switch state {
	case "active", "activating", "reloading", "deactivating":
		// deactivating counts as active on purpose: it means the old
		// process is still there, which is precisely what a caller waiting
		// for a clean stop must not mistake for "gone".
		return true, nil
	case "inactive", "failed":
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("service: systemctl is-active %s: %w (output: %q)", s.unit, err, state)
	}
	return false, fmt.Errorf("service: systemctl is-active %s returned unrecognized state %q", s.unit, state)
}

type dockerController struct{ container string }

func (d *dockerController) Target() string { return "docker container " + d.container }

func (d *dockerController) Stop(ctx context.Context) error {
	return runCommand(ctx, "docker", "stop", d.container)
}

func (d *dockerController) Start(ctx context.Context) error {
	return runCommand(ctx, "docker", "start", d.container)
}

// ReloadConfig is a no-op: a container has no equivalent of daemon-reload -
// a changed Compose file takes effect when the container is recreated, and
// recreating containers is beyond what this controller does.
func (d *dockerController) ReloadConfig(context.Context) error { return nil }

func (d *dockerController) Active(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", d.container).Output()
	state := strings.TrimSpace(string(out))
	switch state {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("service: docker inspect %s: %w (output: %q)", d.container, err, state)
	}
	return false, fmt.Errorf("service: docker inspect %s returned unrecognized state %q", d.container, state)
}

func runCommand(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("service: %s %s: %w (output: %s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
