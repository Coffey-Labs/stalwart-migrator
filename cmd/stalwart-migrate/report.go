// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"flag"
	"fmt"

	"github.com/Coffey-Labs/stalwart-migrator/internal/checkpoint"
	"github.com/Coffey-Labs/stalwart-migrator/internal/validate"
)

// runReport prints what validation found for a run, from the checkpoint the
// run already wrote. It re-reads rather than re-checks: the comparison is
// against a pre-migration snapshot, so running it again later would answer a
// different question — how the instance looks now, not how it looked when it
// was migrated.
func runReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	stateDir := fs.String("state-dir", checkpoint.DefaultBaseDir, "directory runs are checkpointed in")

	runID, rest := splitRunID(fs, args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() != 0 || runID == "" {
		return fmt.Errorf("usage: stalwart-migrate report <run-id> [flags]")
	}

	store := checkpoint.NewStore(*stateDir)
	rs, err := store.Load(runID)
	if err != nil {
		return fmt.Errorf("load run %s: %w", runID, err)
	}

	report := reportFromSteps(rs)
	fmt.Printf("run:    %s\n", rs.RunID)
	fmt.Printf("source: %s\n", rs.SourceVersion)
	fmt.Printf("target: %s\n", rs.TargetVersion)

	if len(report.Results) == 0 {
		fmt.Println("\nno validation has been recorded for this run.")
		if rs.PreflightSnapshot == nil {
			fmt.Println("preflight captured no pre-migration snapshot, so there was nothing to compare against;")
			fmt.Println("pass --admin-url to preflight next time and the comparison becomes possible.")
		} else {
			fmt.Println("the run did not reach the validate phase - see `stalwart-migrate status " + runID + "`.")
		}
		return nil
	}

	fmt.Println("\nvalidation:")
	fmt.Print(report.String())
	if report.Blocking() {
		// Non-zero so this is usable in a script that gates on it, and so a
		// failed migration cannot look successful to anything watching.
		return fmt.Errorf("validation recorded a failure for run %s", runID)
	}
	return nil
}

// reportFromSteps rebuilds the validation report from the checkpointed
// steps, so the report survives the process that produced it.
func reportFromSteps(rs *checkpoint.RunState) validate.Report {
	var report validate.Report
	for _, step := range rs.Steps {
		if step.Phase != checkpoint.PhaseValidate {
			continue
		}
		status := validate.StatusOK
		switch {
		case step.Error != "":
			status = validate.StatusFail
		case step.Verdict == string(validate.StatusFail):
			status = validate.StatusFail
		case step.Verdict == string(validate.StatusSkip):
			status = validate.StatusSkip
		case step.Verdict == string(validate.StatusWarn):
			status = validate.StatusWarn
		case step.Status != checkpoint.StepDone:
			// Recorded but never completed: the run stopped partway.
			status = validate.StatusFail
		}
		detail := step.Detail
		if step.Error != "" {
			if detail != "" {
				detail += " "
			}
			detail += "(error: " + step.Error + ")"
		}
		report.Results = append(report.Results, validate.CheckResult{Name: step.Name, Status: status, Detail: detail})
	}
	return report
}
