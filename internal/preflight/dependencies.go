// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package preflight

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// minCLIVersion is the oldest stalwart-cli the migration can use.
// UPGRADING/v0_16.md is explicit: "install the new CLI (make sure to
// install v1.0.2 or later)". Older builds have no `apply` command at all.
var minCLIVersion = semver{1, 0, 2}

// CheckExternalTools verifies the programs the migration shells out to
// exist and are new enough, before anything is touched.
//
// This check exists because its absence took a production mail server down.
// A live migration stopped the service, converted the settings, and only
// then discovered that the host's stalwart-cli was 0.13.4 - present, but
// from the era when the CLI shipped with the server, and with no `apply`
// command. The migration needs v1.0.2+ from the separately-versioned
// stalwartlabs/cli repository, which is a different download most operators
// have never made. Recovery cost a restore from a day-old snapshot.
//
// Every one of those facts was knowable in under a second, from a stopped
// state, before any risk was taken. That is what preflight is for.
func CheckExternalTools(ctx context.Context, cliPath, pythonPath string, crossesMajorBoundary bool) []CheckResult {
	if cliPath == "" {
		cliPath = "stalwart-cli"
	}
	if pythonPath == "" {
		pythonPath = "python3"
	}
	if !crossesMajorBoundary {
		// A patch bump replays no settings, so neither tool is invoked.
		return []CheckResult{{
			Name:   "external-tools",
			Status: StatusOK,
			Detail: "not needed for a same-boundary patch upgrade: no settings conversion or apply happens",
		}}
	}

	var results []CheckResult

	version, err := toolVersion(ctx, cliPath, "--version")
	switch {
	case err != nil:
		results = append(results, CheckResult{
			Name:   "stalwart-cli",
			Status: StatusFail,
			Detail: fmt.Sprintf("%s is required to replay settings into the migrated store and could not be run: %v. "+
				"It is a separate download from the server - see https://stalw.art/docs/management/cli/overview - and "+
				"must be v%s or later. A stalwart-cli that shipped alongside a 0.15 server has no `apply` command and "+
				"will not do", cliPath, err, minCLIVersion),
		})
	default:
		parsed, parseErr := parseSemver(version)
		switch {
		case parseErr != nil:
			results = append(results, CheckResult{
				Name:   "stalwart-cli",
				Status: StatusWarn,
				Detail: fmt.Sprintf("%s reported a version this tool couldn't parse (%q); it must be v%s or later", cliPath, version, minCLIVersion),
			})
		case parsed.Compare(minCLIVersion) < 0:
			results = append(results, CheckResult{
				Name:   "stalwart-cli",
				Status: StatusFail,
				Detail: fmt.Sprintf("%s is v%s, but the migration needs v%s or later - older builds have no `apply` command. "+
					"Install it from https://stalw.art/docs/management/cli/overview (it is a separate download from the server)",
					cliPath, parsed, minCLIVersion),
			})
		default:
			results = append(results, CheckResult{
				Name:   "stalwart-cli",
				Status: StatusOK,
				Detail: fmt.Sprintf("%s is v%s (needs v%s or later)", cliPath, parsed, minCLIVersion),
			})
		}
	}

	if _, err := toolVersion(ctx, pythonPath, "--version"); err != nil {
		results = append(results, CheckResult{
			Name:   "python3",
			Status: StatusFail,
			Detail: fmt.Sprintf("%s is required to run Stalwart's migrate_v016.py and could not be run: %v", pythonPath, err),
		})
	} else {
		results = append(results, CheckResult{
			Name: "python3", Status: StatusOK,
			Detail: fmt.Sprintf("%s is available for migrate_v016.py", pythonPath),
		})
	}
	return results
}

// toolVersion runs a program's version flag and returns its output.
func toolVersion(ctx context.Context, path string, arg string) (string, error) {
	cmd := exec.CommandContext(ctx, path, arg)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w (output: %s)", err, strings.TrimSpace(out.String()))
	}
	return strings.TrimSpace(out.String()), nil
}
