// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/Coffey-Labs/stalwart-migrator/internal/checkpoint"
)

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	stateDir := fs.String("state-dir", checkpoint.DefaultBaseDir, "directory runs are checkpointed in")

	// Go's flag package stops parsing at the first positional argument, so
	// without this `status <run-id> --state-dir X` would look the run up in
	// the default directory and report it missing.
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

// splitRunID pulls the run-id out of args wherever it appears, so
// `status <run-id> --state-dir X` works as naturally as
// `status --state-dir X <run-id>`. Go's flag package stops parsing at the
// first positional argument, which would otherwise make the obvious
// invocation order silently drop every flag after the run-id. Tokens
// consumed as a flag's value are skipped by asking the FlagSet itself which
// flags take one, rather than by maintaining a second list of them here.
func splitRunID(fs *flag.FlagSet, args []string) (runID string, rest []string) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if strings.HasPrefix(arg, "-") {
			rest = append(rest, arg)
			name := strings.TrimLeft(arg, "-")
			if !strings.Contains(arg, "=") && takesValue(fs, name) && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if runID == "" {
			runID = arg
			continue
		}
		rest = append(rest, arg) // a second positional: let Parse report it
	}
	return runID, rest
}

func takesValue(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !boolFlag.IsBoolFlag()
}
