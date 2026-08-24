// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
)

// runRun implements `stalwart-migrate run`, which refuses.
//
// It refuses because §4.3 staging and the pipeline that would drive
// preflight -> backup -> stage -> recovery-mode -> cutover -> validate
// against real paths don't exist. The phases themselves mostly do:
// internal/preflight, internal/backup, internal/recovery and
// internal/cutover are all implemented, and preflight, backup, the settings
// dump, convert and the recovery-mode store migration have been exercised
// against a real Stalwart 0.15.5. Cutover has not - it has never run
// outside its own tests, and it is the phase that mutates production.
//
// What used to live here was `--dry-run`, which cloned the store into a
// sandbox and migrated the copy. That is now `stalwart-migrate rehearse`,
// minus the cloning: see ARCHITECTURE.md §4.9 for why the expensive half
// was dropped rather than fixed.
func runRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fs.String("binary", "/usr/local/bin/stalwart", "path to the currently-installed stalwart binary")
	fs.String("config", "/etc/stalwart/config.toml", "path to stalwart's current config file")
	fs.String("data-dir", "/var/lib/stalwart", "stalwart data directory")
	fs.String("target", "latest", `target Stalwart version, or "latest"`)
	fs.String("state-dir", checkpoint.DefaultBaseDir, "directory to store run checkpoints in")
	dryRun := fs.Bool("dry-run", false, "removed - see `stalwart-migrate rehearse`")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *dryRun {
		return fmt.Errorf("--dry-run has been replaced by `stalwart-migrate rehearse`, which converts this " +
			"instance's settings and reports what will and won't carry over. It no longer clones the data " +
			"directory: that only proved the store opens, and cost a full copy of it to find out " +
			"(ARCHITECTURE.md §4.9)")
	}

	fmt.Fprintln(os.Stderr,
		"real migrations aren't available yet: the staging phase (ARCHITECTURE.md §4.3) and the pipeline that\n"+
			"would drive preflight -> backup -> stage -> recovery-mode -> cutover -> validate don't exist, so this\n"+
			"command has no path that touches production.\n\n"+
			"Two things worth knowing while you wait:\n"+
			"  * `stalwart-migrate rehearse` converts your settings and reports what will NOT carry over. Measured\n"+
			"    against a production instance that was 98% of them, listeners included - so it decides your\n"+
			"    migration plan, and it's safe to run now.\n"+
			"  * Recovery from a failed migration is your own snapshot or backup. This tool does not undo a\n"+
			"    migration (§4.8).")
	return fmt.Errorf("`run` is not implemented")
}
