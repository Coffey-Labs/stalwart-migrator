// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

// Command stalwart-migrate drives an in-place Stalwart Mail Server upgrade
// (0.15.5 -> latest) through preflight checks, a defense-in-depth backup,
// a checkpointed migration, and post-migration validation. Recovery from a
// failed migration is the operator's own snapshot or backup and is out of
// scope for this tool - see ARCHITECTURE.md §4.8.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "preflight":
		err = runPreflight(os.Args[2:])
	case "rehearse":
		err = runRehearse(os.Args[2:])
	case "run":
		err = runRun(os.Args[2:])
	case "tenants":
		err = runTenants(os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	case "report":
		err = runReport(os.Args[2:])
	default:
		usage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "stalwart-migrate:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: stalwart-migrate <command> [flags]

commands:
  preflight   run read-only checks and print the migration plan
  rehearse    convert this instance's settings and report what will NOT carry over (read-only)
  run         perform the migration (needs --yes and --recovery-point-confirmed)
  tenants     show which tenant owns which domain, and what blocks a migration (read-only)
  status      show the state of an in-progress or completed run
  report      print the validation report for a run`)
}
