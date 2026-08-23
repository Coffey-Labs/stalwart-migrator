package main

import (
	"flag"
	"fmt"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
)

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	stateDir := fs.String("state-dir", checkpoint.DefaultBaseDir, "directory runs are checkpointed in")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store := checkpoint.NewStore(*stateDir)

	if fs.NArg() == 0 {
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

	runID := fs.Arg(0)
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
