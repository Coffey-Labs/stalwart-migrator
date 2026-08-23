package main

import (
	"flag"
	"fmt"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
)

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	stateDir := fs.String("state-dir", checkpoint.DefaultBaseDir, "directory runs are checkpointed in")

	// Same treatment as `rollback`: without this, `status <run-id>
	// --state-dir X` would look up the run in the default directory and
	// report it missing, since flag parsing stops at the run-id.
	runID, rest := splitRunID(fs, args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: stalwart-migrate status [run-id] [flags]")
	}

	store := checkpoint.NewStore(*stateDir)

	if runID == "" {
		ids, err := store.List()
		if err != nil {
			return fmt.Errorf("list runs: %w", err)
		}
		if len(ids) == 0 {
			fmt.Println("no runs found in", *stateDir)
			return nil
		}
		fmt.Println("runs (newest first):")
		for _, id := range ids {
			fmt.Println(" ", id)
		}
		return nil
	}

	rs, err := store.Load(runID)
	if err != nil {
		return fmt.Errorf("load run %s: %w", runID, err)
	}

	fmt.Printf("run:    %s\n", rs.RunID)
	fmt.Printf("source: %s\n", rs.SourceVersion)
	fmt.Printf("target: %s\n", rs.TargetVersion)
	fmt.Printf("topology: deployment=%s store=%s\n", rs.Topology.DeploymentKind, rs.Topology.StoreBackend)
	fmt.Printf("rollback window closed: %v\n", rs.RollbackWindowClosed)
	fmt.Println("steps:")
	for _, step := range rs.Steps {
		tag := string(step.Status)
		if step.Verdict != "" {
			tag = step.Verdict
		}
		line := fmt.Sprintf("  [%-4s] %s/%s", tag, step.Phase, step.Name)
		if step.Detail != "" {
			line += " - " + step.Detail
		}
		if step.Error != "" {
			line += " (error: " + step.Error + ")"
		}
		fmt.Println(line)
	}
	return nil
}
