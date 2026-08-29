// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package cutover

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/preflight"
)

// ArtifactContainerDefinition is the preserved `docker inspect` of the
// container as it was before cutover replaced it - the container's
// equivalent of ArtifactServiceUnit, and for the same reason. Recovery is
// out of scope (ARCHITECTURE.md §4.8), so what this tool owes an operator
// putting a machine back by hand is the definition they would otherwise be
// reconstructing from memory.
const ArtifactContainerDefinition = "container-definition"

// ContainerOptions configures cutting over a container deployment.
type ContainerOptions struct {
	// ContainerName is the live container, which must already be stopped.
	ContainerName string

	// StagedImage is the image ID stage verified - an ID rather than a tag
	// deliberately, so what runs is what was checked.
	StagedImage string

	// PreserveDir is where the inspected definition is written.
	PreserveDir string

	DockerBinary string
}

func (o ContainerOptions) docker() string {
	if o.DockerBinary == "" {
		return "docker"
	}
	return o.DockerBinary
}

// runContainerCutover replaces the container with one running the staged
// image, carrying across the parts of its definition this tool understands
// and refusing outright when it finds parts it does not.
//
// The old container is renamed rather than removed, and the old image is
// never pruned. Together they are the manual restore path: an operator can
// start the previous container again with one command, which is as close to
// the preserved-binary guarantee (§4.2) as a container gets.
func runContainerCutover(ctx context.Context, rs *checkpoint.RunState, step stepFunc, opts ContainerOptions) error {
	if opts.ContainerName == "" {
		return fmt.Errorf("cutover: no container name")
	}
	if opts.StagedImage == "" {
		return fmt.Errorf("cutover: no staged image - stage the target image before cutting over to it")
	}

	var facts preflight.ContainerFacts

	if err := step("preserve-container-definition", func() (checkpoint.StepOutcome, error) {
		raw, err := dockerOut(ctx, opts.docker(), "inspect", opts.ContainerName)
		if err != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf("inspect %s: %w (%s)", opts.ContainerName, err, raw)
		}
		if opts.PreserveDir == "" {
			return checkpoint.StepOutcome{}, fmt.Errorf("no directory to preserve the container definition in")
		}
		if err := os.MkdirAll(opts.PreserveDir, 0o750); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		dest := filepath.Join(opts.PreserveDir, opts.ContainerName+".inspect.json")
		if err := os.WriteFile(dest, []byte(raw), 0o640); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		sum, size, err := hashFile(dest)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		// Recorded before anything is replaced, so a crash between
		// preserving and recreating still leaves the original findable.
		rs.RecordArtifact(ArtifactContainerDefinition, checkpoint.Artifact{Path: dest, SHA256: sum, SizeBytes: size})

		facts, err = preflight.InspectContainer(ctx, opts.ContainerName)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		return checkpoint.StepOutcome{Detail: "preserved the container definition at " + dest}, nil
	}); err != nil {
		return err
	}

	if err := step("container-is-recreatable", func() (checkpoint.StepOutcome, error) {
		if len(facts.Unsupported) > 0 {
			return checkpoint.StepOutcome{}, fmt.Errorf(
				"this container uses configuration cutting over would not carry across: %s. Recreating it without those would "+
					"start cleanly and quietly not be the server it was, so this tool will not do it. Migrate this one by hand: "+
					"the definition is preserved as %s, and the staged image is %s",
				strings.Join(facts.Unsupported, "; "), rs.Artifacts[ArtifactContainerDefinition].Path, opts.StagedImage)
		}
		return checkpoint.StepOutcome{Detail: "the container's definition is entirely within what a recreate carries across"}, nil
	}); err != nil {
		return err
	}

	retired := opts.ContainerName + "-premigration"
	if rs.SourceVersion != "" {
		retired += "-" + rs.SourceVersion
	}

	if err := step("retire-old-container", func() (checkpoint.StepOutcome, error) {
		// Renamed, not removed. The old container plus the old image - which
		// nothing here prunes - is what an operator restores by hand.
		if out, err := dockerOut(ctx, opts.docker(), "rename", opts.ContainerName, retired); err != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf("rename %s: %w (%s)", opts.ContainerName, err, out)
		}
		return checkpoint.StepOutcome{Detail: fmt.Sprintf("kept the previous container as %s, still on image %s", retired, facts.Image)}, nil
	}); err != nil {
		return err
	}

	return step("create-container", func() (checkpoint.StepOutcome, error) {
		args := []string{"run", "-d", "--name", opts.ContainerName}
		if facts.RestartPolicy != "" && facts.RestartPolicy != "no" {
			args = append(args, "--restart", facts.RestartPolicy)
		}
		for _, e := range facts.Env {
			// Recovery-mode variables must never survive into a normal
			// start: leaving STALWART_RECOVERY_MODE set would recovery-boot
			// on every restart, the same footgun §4.5 strips from a unit.
			if strings.HasPrefix(e, "STALWART_RECOVERY_") {
				continue
			}
			args = append(args, "-e", e)
		}
		for _, m := range facts.Mounts {
			src := m.Name
			if src == "" {
				src = m.Source
			}
			spec := src + ":" + m.Destination
			if !m.RW {
				spec += ":ro"
			}
			args = append(args, "-v", spec)
		}
		for port, bindings := range facts.Ports {
			for _, b := range bindings {
				spec := b.HostPort + ":" + strings.SplitN(port, "/", 2)[0]
				if b.HostIP != "" {
					spec = b.HostIP + ":" + spec
				}
				args = append(args, "-p", spec)
			}
		}
		for k, v := range facts.Labels {
			args = append(args, "--label", k+"="+v)
		}
		args = append(args, opts.StagedImage)

		if out, err := dockerOut(ctx, opts.docker(), args...); err != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf(
				"create %s from %s: %w (%s). The previous container is still here as %s",
				opts.ContainerName, opts.StagedImage, err, out, retired)
		}
		return checkpoint.StepOutcome{Detail: fmt.Sprintf("recreated %s on image %s", opts.ContainerName, opts.StagedImage)}, nil
	})
}

// stepFunc is cutover.Run's checkpointed step runner, passed in so the
// container path reports through the same Report the systemd path does.
type stepFunc func(name string, fn func() (checkpoint.StepOutcome, error)) error

func dockerOut(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}
