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
