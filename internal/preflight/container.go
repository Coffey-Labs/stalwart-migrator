// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
)

// checkFunc is the closure Checker.Run uses to run and checkpoint one
// check. Named here so the container checks can be a method rather than
// another hundred lines inside Run.
type checkFunc func(name string, fn func() (CheckResult, string)) (checkpoint.StepOutcome, error)

// ComposeProjectLabel is set by Docker Compose on every container it
// manages. Its presence is the difference between a container this tool
// could one day recreate and one it must not: recreating a compose-managed
// container out from under compose leaves the running container and the
// compose file disagreeing about what is deployed, and the next
// `compose up` silently reverts the migration.
const ComposeProjectLabel = "com.docker.compose.project"

// Mount is one bind or volume mount as the container sees it. Only the
// fields this tool reasons about are kept; docker inspect returns more.
type Mount struct {
	Type        string `json:"Type"`        // "volume" or "bind"
	Name        string `json:"Name"`        // volume name, empty for binds
	Source      string `json:"Source"`      // host path
	Destination string `json:"Destination"` // path inside the container
	RW          bool   `json:"RW"`
}

// ContainerFacts is what `docker inspect` says about a running Stalwart
// container, reduced to the things that decide whether it can be migrated.
type ContainerFacts struct {
	Name    string
	Image   string // the tag it was started from, e.g. "stalwartlabs/stalwart:v0.15.5"
	ImageID string // the digest actually running, which a tag can drift from
	Labels  map[string]string
	Mounts  []Mount
	Running bool

	// The rest is what cutover would have to carry across when it recreates
	// the container. Recreating is the container equivalent of rewriting a
	// unit file, except that a unit can be edited in place and a container
	// cannot - so anything not carried here is silently dropped, which is
	// the failure UnsupportedForRecreate exists to prevent.
	Env           []string
	Ports         map[string][]PortBinding
	RestartPolicy string
	NetworkMode   string
	Unsupported   []string // populated by UnsupportedForRecreate
}

// PortBinding is one published port.
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// ComposeProject returns the compose project managing this container, or
// "" if it is a plain `docker run`.
func (f ContainerFacts) ComposeProject() string { return f.Labels[ComposeProjectLabel] }

// WritableMounts are the mounts data could persist in. A container with
// none keeps everything in its own writable layer, which is discarded when
// the container is replaced - and replacing the container is exactly what
// migrating it means.
func (f ContainerFacts) WritableMounts() []Mount {
	var out []Mount
	for _, m := range f.Mounts {
		if m.RW {
			out = append(out, m)
		}
	}
	return out
}

// MountFor returns the mount whose Destination contains path, if any. A
// data directory not covered by one lives in the writable layer.
func (f ContainerFacts) MountFor(path string) (Mount, bool) {
	if path == "" {
		return Mount{}, false
	}
	var best Mount
	var found bool
	for _, m := range f.Mounts {
		if m.Destination == path || strings.HasPrefix(path, strings.TrimSuffix(m.Destination, "/")+"/") {
			// Longest destination wins: /var/lib/stalwart/data is more
			// specific than /var/lib, and it is the specific one that
			// actually holds the bytes.
			if !found || len(m.Destination) > len(best.Destination) {
				best, found = m, true
			}
		}
	}
	return best, found
}

// inspectOutput is the subset of `docker inspect` this parses. Named
// separately from ContainerFacts because docker's shape is docker's to
// change, and the rest of this package should not have to know it.
type inspectOutput struct {
	Name   string `json:"Name"`
	Image  string `json:"Image"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
		Env    []string          `json:"Env"`
		User   string            `json:"User"`
	} `json:"Config"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Mounts     []Mount `json:"Mounts"`
	HostConfig struct {
		PortBindings  map[string][]PortBinding `json:"PortBindings"`
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
		NetworkMode string            `json:"NetworkMode"`
		CapAdd      []string          `json:"CapAdd"`
		CapDrop     []string          `json:"CapDrop"`
		Devices     []any             `json:"Devices"`
		Sysctls     map[string]string `json:"Sysctls"`
		Ulimits     []any             `json:"Ulimits"`
		Privileged  bool              `json:"Privileged"`
		ExtraHosts  []string          `json:"ExtraHosts"`
		DNS         []string          `json:"Dns"`
		GroupAdd    []string          `json:"GroupAdd"`
		SecurityOpt []string          `json:"SecurityOpt"`
		Tmpfs       map[string]string `json:"Tmpfs"`
		LogConfig   struct {
			Type string `json:"Type"`
		} `json:"LogConfig"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]any `json:"Networks"`
	} `json:"NetworkSettings"`
}

// InspectContainer reads the facts about containerName. An error here is
// an error, not an absent container: callers reach this only after
// DetectDeploymentKind has already established that a container answers to
// this name, so a failure now means docker stopped answering, and guessing
// past that is how a tool ends up migrating something it cannot see.
func InspectContainer(ctx context.Context, containerName string) (ContainerFacts, error) {
	if containerName == "" {
		containerName = "stalwart"
	}
	out, err := exec.CommandContext(ctx, "docker", "inspect", containerName).Output()
	if err != nil {
		return ContainerFacts{}, fmt.Errorf("preflight: docker inspect %s: %w", containerName, err)
	}
	var got []inspectOutput
	if err := json.Unmarshal(out, &got); err != nil {
		return ContainerFacts{}, fmt.Errorf("preflight: parsing docker inspect %s: %w", containerName, err)
	}
	if len(got) == 0 {
		return ContainerFacts{}, fmt.Errorf("preflight: docker inspect %s returned no container", containerName)
	}
	c := got[0]
	f := ContainerFacts{
		Name:          strings.TrimPrefix(c.Name, "/"),
		Image:         c.Config.Image,
		ImageID:       c.Image,
		Labels:        c.Config.Labels,
		Mounts:        c.Mounts,
		Running:       c.State.Running,
		Env:           c.Config.Env,
		Ports:         c.HostConfig.PortBindings,
		RestartPolicy: c.HostConfig.RestartPolicy.Name,
		NetworkMode:   c.HostConfig.NetworkMode,
	}
	f.Unsupported = unsupportedForRecreate(c)
	return f, nil
}

// unsupportedForRecreate names every piece of this container's
// configuration that recreating it would not carry across.
//
// Cutover recreates rather than edits, because a container cannot be edited
// in place the way a unit file can. That makes silent loss the default
// failure: a container recreated without its capabilities, its custom
// network or its device mappings starts cleanly and is quietly not the
// server it was. §4.5 already refuses to edit a unit line it only partly
// understands; this is the same rule, applied where the whole definition
// has to be rebuilt.
//
// The list is deliberately conservative and deliberately not exhaustive -
// docker's HostConfig has far more fields than these. It names the ones a
// mail server plausibly uses, and anything it does not know about is a
// reason this tool should not be recreating that container at all.
func unsupportedForRecreate(c inspectOutput) []string {
	var out []string
	add := func(cond bool, what string) {
		if cond {
			out = append(out, what)
		}
	}
	h := c.HostConfig
	add(len(h.CapAdd) > 0, "added capabilities (--cap-add)")
	add(len(h.CapDrop) > 0, "dropped capabilities (--cap-drop)")
	add(len(h.Devices) > 0, "device mappings (--device)")
	add(len(h.Sysctls) > 0, "sysctls (--sysctl)")
	add(len(h.Ulimits) > 0, "ulimits (--ulimit)")
	add(h.Privileged, "privileged mode (--privileged)")
	add(len(h.ExtraHosts) > 0, "extra hosts (--add-host)")
	add(len(h.DNS) > 0, "custom DNS (--dns)")
	add(len(h.GroupAdd) > 0, "supplementary groups (--group-add)")
	add(len(h.SecurityOpt) > 0, "security options (--security-opt)")
	add(len(h.Tmpfs) > 0, "tmpfs mounts (--tmpfs)")
	add(h.LogConfig.Type != "" && h.LogConfig.Type != "json-file", "a non-default log driver (--log-driver "+h.LogConfig.Type+")")
	add(c.Config.User != "", "a container user (--user "+c.Config.User+")")

	// A user-defined network is a name in NetworkSettings.Networks that is
	// not one of docker's built-ins. Recreating without it puts the server
	// somewhere nothing else can reach it.
	for name := range c.NetworkSettings.Networks {
		switch name {
		case "bridge", "host", "none":
		default:
			out = append(out, "a user-defined network ("+name+")")
		}
	}
	return out
}

// runContainerChecks adds the checks that only apply to a container. They
// run after deployment-kind has already established there is one.
//
// Both are blocking for `run` and advisory for `rehearse`, on the same
// reasoning as the deployment-kind check itself: rehearse never stops or
// recreates anything, and an operator doing the migration by hand needs
// these facts more than an automated run does.
func (c *Checker) runContainerChecks(ctx context.Context, runCheck checkFunc) error {
	facts, factsErr := InspectContainer(ctx, c.opts.ContainerName)

	if _, err := runCheck("container-inspect", func() (CheckResult, string) {
		if factsErr != nil {
			return CheckResult{Status: StatusFail, Detail: factsErr.Error()}, ""
		}
		return CheckResult{Status: StatusOK, Detail: fmt.Sprintf(
			"container %s runs image %s (%s)", facts.Name, facts.Image, shortID(facts.ImageID))}, facts.Image
	}); err != nil {
		return err
	}
	if factsErr != nil {
		// The two checks below read facts we do not have.
		return nil
	}

	if _, err := runCheck("container-runtime", func() (CheckResult, string) {
		project := facts.ComposeProject()
		if project == "" {
			return CheckResult{Status: StatusOK, Detail: "plain docker container, not compose-managed"}, ""
		}
		status := StatusFail
		if c.opts.DeploymentCheckAdvisory {
			status = StatusWarn
		}
		return CheckResult{Status: status, Detail: fmt.Sprintf(
			"container is managed by docker compose (project %q). Recreating it out from under compose would leave the "+
				"running container and the compose file disagreeing about what is deployed, and the next `compose up` would "+
				"revert the migration. Migrate it by editing the image tag in the compose file and running `compose up -d`",
			project)}, project
	}); err != nil {
		return err
	}

	_, err := runCheck("container-data-volume", func() (CheckResult, string) {
		writable := facts.WritableMounts()
		if len(writable) == 0 {
			status := StatusFail
			if c.opts.DeploymentCheckAdvisory {
				status = StatusWarn
			}
			return CheckResult{Status: status, Detail: "container has no writable volume or bind mount, so its data lives in " +
				"the container's own writable layer - which is discarded when the container is replaced, and replacing it is " +
				"what migrating it means. Move the data onto a volume before migrating"}, ""
		}
		// A data directory named but not covered by a mount is the same
		// problem wearing a disguise, and worth saying separately: the
		// mounts exist, they just are not where the data is.
		if c.opts.DataDir != "" {
			if m, ok := facts.MountFor(c.opts.DataDir); ok {
				return CheckResult{Status: StatusOK, Detail: fmt.Sprintf(
					"data dir %s is on a %s mount (%s)", c.opts.DataDir, m.Type, mountSource(m))}, m.Destination
			}
			status := StatusFail
			if c.opts.DeploymentCheckAdvisory {
				status = StatusWarn
			}
			return CheckResult{Status: status, Detail: fmt.Sprintf(
				"data dir %s is not covered by any of the container's mounts (%s), so it lives in the writable layer and would "+
					"not survive the container being replaced. Check whether --data-dir names the path inside the container",
				c.opts.DataDir, describeMounts(facts.Mounts))}, ""
		}
		return CheckResult{Status: StatusOK, Detail: "container has writable mounts: " + describeMounts(writable)}, ""
	})
	return err
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func mountSource(m Mount) string {
	if m.Name != "" {
		return m.Name
	}
	return m.Source
}

func describeMounts(mounts []Mount) string {
	if len(mounts) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(mounts))
	for _, m := range mounts {
		parts = append(parts, m.Destination+" <- "+mountSource(m))
	}
	return strings.Join(parts, ", ")
}
