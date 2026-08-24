// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package recovery

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// ApplyOptions configures stalwart-cli apply invocations against a
// recovery-mode instance - see UPGRADING/v0_16.md's documented
// STALWART_URL/STALWART_USER/STALWART_PASSWORD + `stalwart-cli apply --file`
// sequence.
type ApplyOptions struct {
	CLIBinaryPath string // defaults to "stalwart-cli"
	URL           string
	User          string
	Password      string
}

// Apply runs `stalwart-cli apply --file <file>` once, with credentials
// passed as environment variables exactly as the upgrade guide's own
// example does, rather than on the command line where they'd be visible to
// anything reading this process's argv.
func Apply(ctx context.Context, o ApplyOptions, file string) error {
	binary := o.CLIBinaryPath
	if binary == "" {
		binary = "stalwart-cli"
	}
	cmd := exec.CommandContext(ctx, binary, "apply", "--file", file)
	cmd.Env = append(os.Environ(),
		"STALWART_URL="+o.URL,
		"STALWART_USER="+o.User,
		"STALWART_PASSWORD="+o.Password,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("recovery: stalwart-cli apply --file %s failed: %w (output: %s)", file, err, out)
	}
	return nil
}

// ApplyAll runs Apply for each file in order - export.json first, then any
// additional test-deployment snapshots (ARCHITECTURE.md §4.3/§4.4) -
// stopping at the first failure so a partial, silently-incomplete settings
// replay never gets reported as success.
func ApplyAll(ctx context.Context, o ApplyOptions, files []string) error {
	for i, f := range files {
		if err := Apply(ctx, o, f); err != nil {
			return fmt.Errorf("applied %d/%d file(s) before failing: %w", i, len(files), err)
		}
	}
	return nil
}
