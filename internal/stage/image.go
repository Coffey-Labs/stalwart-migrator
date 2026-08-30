// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package stage

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Coffey-Labs/stalwart-migrator/internal/checkpoint"
	"github.com/Coffey-Labs/stalwart-migrator/internal/preflight"
)

// DefaultDockerBinary is what shells out unless a caller names another.
const DefaultDockerBinary = "docker"

// ImageOptions configures staging a container image - the container
// deployment's answer to downloading a binary. See issue #3.
type ImageOptions struct {
	// Image is the target image, named in full by the operator:
	// "stalwartlabs/stalwart:v0.16.14". It is deliberately never derived
	// from the running container's image by swapping the tag. That
	// derivation is wrong for a digest-pinned image, wrong for a mirror,
	// and wrong for a fork - and being wrong here means pulling the wrong
	// software into a mail server, which is not a mistake worth a
	// convenience.
	Image string

	// TargetVersion is what the image must report, e.g. "0.16.14".
	TargetVersion string

	// SkipPull uses an image already present locally. For an air-gapped
	// host that loaded it from a tarball, where a pull cannot work and its
	// failure would say nothing useful.
	SkipPull bool

	DockerBinary string
}

func (o ImageOptions) docker() string {
	if o.DockerBinary == "" {
		return DefaultDockerBinary
	}
	return o.DockerBinary
}

// StagedImage is a target image that is present locally and has been asked
// what it is.
type StagedImage struct {
	// Ref is what to actually run: the image's own ID, not the tag it
	// arrived under. A tag can move between staging and cutover - that is
	// the whole reason `latest` is a hazard - and running the tag later
	// would run something other than what was verified here.
	Ref string

	// Tag is what the operator named, kept for reports.
	Tag string

	// Version is what the image reported, which is the only thing that
	// settles what was pulled.
	Version string
}

// RunImage pulls the target image and confirms it is the version that was
// asked for, checkpointed as the stage phase's "stage-image" step.
//
// The version check is the point of the phase, exactly as it is for a
// binary: the tag, the registry and the repository name are all assumptions
// about someone else's publishing process, and the image's own answer is
// the only thing that settles what arrived.
//
// One caveat, recorded rather than hidden. Asking an image its version
// means running it with --version, which assumes its entrypoint is the
// server and passes flags through. That holds for an image built the
// obvious way and has not been confirmed against a published Stalwart
// image - there was none to hand when this was written. If it turns out
// not to hold, this fails loudly with the image's own output rather than
// staging something unverified, and the fix is a way to name the command
// rather than a reason to skip the check.
func RunImage(ctx context.Context, store *checkpoint.Store, rs *checkpoint.RunState, opts ImageOptions) (StagedImage, error) {
	if opts.Image == "" {
		return StagedImage{}, fmt.Errorf("stage: no target image named - pass the image to migrate to; it is never guessed from the running container")
	}
	if opts.TargetVersion == "" {
		return StagedImage{}, fmt.Errorf("stage: no target version to check %s against", opts.Image)
	}

	var staged StagedImage
	_, err := store.RunStep(rs, checkpoint.PhaseStage, "stage-image", func() (checkpoint.StepOutcome, error) {
		if !opts.SkipPull {
			if out, err := run(ctx, opts.docker(), "pull", opts.Image); err != nil {
				return checkpoint.StepOutcome{}, fmt.Errorf("stage: pull %s: %w (%s)", opts.Image, err, out)
			}
		}

		id, err := run(ctx, opts.docker(), "image", "inspect", "-f", "{{.Id}}", opts.Image)
		if err != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf("stage: %s is not present locally after pull: %w (%s)", opts.Image, err, id)
		}
		id = strings.TrimSpace(id)
		if id == "" {
			return checkpoint.StepOutcome{}, fmt.Errorf("stage: docker reported no image ID for %s", opts.Image)
		}

		// The image's own entrypoint first, which is what an image built
		// the obvious way answers to. Only if that fails is the entrypoint
		// overridden to call the binary by name - a second guess, tried
		// because failing the whole phase over an image that merely wraps
		// its server in a shim would be worse than one extra attempt.
		out, err := run(ctx, opts.docker(), "run", "--rm", id, "--version")
		if err != nil {
			out, err = run(ctx, opts.docker(), "run", "--rm", "--entrypoint", "stalwart", id, "--version")
		}
		if err != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf(
				"stage: %s would not report its version: %w (output: %s). This tool will not stage an image it cannot identify",
				opts.Image, err, strings.TrimSpace(out))
		}

		got, err := preflight.VersionFromOutput(out)
		if err != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf("stage: parse version from %s (%q): %w", opts.Image, strings.TrimSpace(out), err)
		}
		wanted := strings.TrimPrefix(opts.TargetVersion, "v")
		if got != wanted {
			return checkpoint.StepOutcome{}, fmt.Errorf(
				"stage: image %s reports %s but %s was asked for - the tag does not contain what it claims",
				opts.Image, got, wanted)
		}

		staged = StagedImage{Ref: id, Tag: opts.Image, Version: got}
		return checkpoint.StepOutcome{
			Detail: fmt.Sprintf("staged %s as %s, which reports %s", opts.Image, shortRef(id), got),
			Extra:  id,
		}, nil
	})
	if err != nil {
		return StagedImage{}, err
	}
	if staged.Ref == "" {
		// A resumed run skipped the step, so the values above were never
		// assigned. The recorded ID is what that run verified.
		staged = StagedImage{Ref: rs.Outcome(checkpoint.PhaseStage, "stage-image").Extra, Tag: opts.Image, Version: opts.TargetVersion}
	}
	return staged, nil
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func shortRef(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
