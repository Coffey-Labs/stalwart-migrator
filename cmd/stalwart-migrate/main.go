// Command stalwart-migrate drives an in-place Stalwart Mail Server upgrade
// (0.15.5 -> latest) through preflight checks, a defense-in-depth backup,
// a checkpointed migration, and post-migration validation, with rollback
// available at every step. See ARCHITECTURE.md for the full design.
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
	case "run":
		err = runRun(os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	case "rollback":
		err = runRollback(os.Args[2:])
	case "confirm":
		err = fmt.Errorf("not implemented yet: see internal/checkpoint")
	case "report":
		err = fmt.Errorf("not implemented yet: see internal/validate")
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
  run         --dry-run: simulate and validate against a sandbox (real cutover isn't implemented yet)
  status      show the state of an in-progress or completed run
  rollback    restore the pre-migration backup for a given run (prints the plan; --yes to act)
  confirm     close the rollback window for a completed run
  report      print the validation report for a run`)
}
