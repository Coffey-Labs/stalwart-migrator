// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package preflight

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// semver is a minimal major.minor.patch version - enough for this tool's
// comparisons since Stalwart's pre-1.0 release tags don't use pre-release
// or build metadata.
type semver struct {
	Major, Minor, Patch int
}

var versionPattern = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)

func parseSemver(s string) (semver, error) {
	m := versionPattern.FindStringSubmatch(s)
	if m == nil {
		return semver{}, fmt.Errorf("preflight: no version number found in %q", s)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return semver{major, minor, patch}, nil
}

func (v semver) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Compare returns -1, 0, or 1 as v is less than, equal to, or greater than o.
func (v semver) Compare(o semver) int {
	if v.Major != o.Major {
		return cmp(v.Major, o.Major)
	}
	if v.Minor != o.Minor {
		return cmp(v.Minor, o.Minor)
	}
	return cmp(v.Patch, o.Patch)
}

func cmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// minSupportedSource is the oldest version this tool will start a migration
// from. Stalwart's own upgrade guidance says older installs need to reach
// 0.15.x first before crossing the 0.15/0.16 schema boundary this tool
// automates - see ARCHITECTURE.md §1/§4.1.
var minSupportedSource = semver{0, 15, 0}

// VersionFromOutput extracts a semver from whatever a Stalwart build
// printed when asked for its version. Exported because the same question
// gets asked of a container image (internal/stage), where the command is
// `docker run <image> --version` rather than the binary directly - and the
// answer has to be parsed identically or the two paths could disagree
// about what they staged.
func VersionFromOutput(out string) (string, error) {
	v, err := parseSemver(out)
	if err != nil {
		return "", err
	}
	return v.String(), nil
}

// DetectContainerVersion reads the running version of a container
// deployment, which is a property of its image rather than of anything on
// this host.
//
// A container-only host has no Stalwart binary to ask, and a host that
// happens to have one is worse than a host that does not: a stray
// /usr/local/bin/stalwart left over from an older install answers
// confidently with a version nothing is running. So this asks the image
// the container is actually on, by ID rather than by the tag it was
// started from.
//
// The command mirrors internal/stage's, including the fallback, because
// the two have to agree about what a Stalwart image reports or the source
// and target versions could be read by different rules.
func DetectContainerVersion(ctx context.Context, containerName string) (string, error) {
	if containerName == "" {
		containerName = "stalwart"
	}
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.Image}}", containerName).Output()
	if err != nil {
		return "", fmt.Errorf("preflight: reading the image behind container %s: %w", containerName, err)
	}
	imageID := strings.TrimSpace(string(out))
	if imageID == "" {
		return "", fmt.Errorf("preflight: container %s reports no image", containerName)
	}

	raw, runErr := exec.CommandContext(ctx, "docker", "run", "--rm", imageID, "--version").Output()
	if runErr != nil {
		raw, runErr = exec.CommandContext(ctx, "docker", "run", "--rm", "--entrypoint", "stalwart", imageID, "--version").Output()
	}
	if runErr != nil {
		return "", fmt.Errorf("preflight: asking image %s for its version: %w", shortID(imageID), runErr)
	}
	v, err := VersionFromOutput(string(raw))
	if err != nil {
		return "", fmt.Errorf("preflight: image %s did not report a version this understands: %w", shortID(imageID), err)
	}
	return v, nil
}

// DetectVersion runs the installed binary's --version flag and extracts a
// semver from its output.
func DetectVersion(ctx context.Context, binaryPath string) (string, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "--version")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("preflight: run %s --version: %w (output: %s)", binaryPath, err, strings.TrimSpace(out.String()))
	}
	v, err := parseSemver(out.String())
	if err != nil {
		return "", fmt.Errorf("preflight: parse version from %s --version output %q: %w", binaryPath, strings.TrimSpace(out.String()), err)
	}
	return v.String(), nil
}
