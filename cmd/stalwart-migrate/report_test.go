// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"testing"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/validate"
)

// `report` re-reads what the run recorded rather than re-checking, so the
// mapping from checkpointed step back to verdict is the whole command. The
// case that matters is a failure staying a failure: a migration that lost an
// account must not read as clean the next morning.

func TestReportFromSteps(t *testing.T) {
	for _, tc := range []struct {
		name  string
		step  checkpoint.StepRecord
		want  validate.Status
		block bool
	}{
		{
			name: "a completed comparison is a pass",
			step: checkpoint.StepRecord{Phase: checkpoint.PhaseValidate, Name: "content-integrity", Status: checkpoint.StepDone, StepOutcome: checkpoint.StepOutcome{Detail: "12 accounts checked"}},
			want: validate.StatusOK,
		},
		{
			name:  "a failing verdict survives the round trip",
			step:  checkpoint.StepRecord{Phase: checkpoint.PhaseValidate, Name: "content-integrity", Status: checkpoint.StepDone, StepOutcome: checkpoint.StepOutcome{Verdict: string(validate.StatusFail), Detail: "missing accounts: bob@example.org"}},
			want:  validate.StatusFail,
			block: true,
		},
		{
			name: "a skipped check stays skipped, not a pass",
			step: checkpoint.StepRecord{Phase: checkpoint.PhaseValidate, Name: "content-integrity", Status: checkpoint.StepDone, StepOutcome: checkpoint.StepOutcome{Verdict: string(validate.StatusSkip), Detail: "no snapshot"}},
			want: validate.StatusSkip,
		},
		{
			name:  "a step that errored is a failure",
			step:  checkpoint.StepRecord{Phase: checkpoint.PhaseValidate, Name: "content-integrity", Status: checkpoint.StepFailed, Error: "connection refused"},
			want:  validate.StatusFail,
			block: true,
		},
		{
			name:  "a step that never finished is a failure, not a pass",
			step:  checkpoint.StepRecord{Phase: checkpoint.PhaseValidate, Name: "content-integrity", Status: checkpoint.StepRunning},
			want:  validate.StatusFail,
			block: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := reportFromSteps(&checkpoint.RunState{Steps: []checkpoint.StepRecord{tc.step}})
			if len(got.Results) != 1 {
				t.Fatalf("got %d results, want 1", len(got.Results))
			}
			if got.Results[0].Status != tc.want {
				t.Fatalf("status = %q, want %q", got.Results[0].Status, tc.want)
			}
			if got.Blocking() != tc.block {
				t.Fatalf("Blocking() = %v, want %v", got.Blocking(), tc.block)
			}
		})
	}
}

func TestReportFromStepsIgnoresOtherPhases(t *testing.T) {
	rs := &checkpoint.RunState{Steps: []checkpoint.StepRecord{
		{Phase: checkpoint.PhaseCutover, Name: "start-service", Status: checkpoint.StepDone},
		{Phase: checkpoint.PhasePreflight, Name: "disk-space", Status: checkpoint.StepFailed, Error: "nope"},
		{Phase: checkpoint.PhaseValidate, Name: "content-integrity", Status: checkpoint.StepDone, StepOutcome: checkpoint.StepOutcome{Detail: "ok"}},
	}}
	got := reportFromSteps(rs)
	if len(got.Results) != 1 || got.Results[0].Name != "content-integrity" {
		t.Fatalf("expected only the validate phase, got %+v", got.Results)
	}
	// A failed preflight step must not leak into the validation verdict.
	if got.Blocking() {
		t.Fatal("a failure in another phase must not make validation look failed")
	}
}

func TestReportFromStepsOnARunThatNeverValidated(t *testing.T) {
	rs := &checkpoint.RunState{Steps: []checkpoint.StepRecord{
		{Phase: checkpoint.PhaseCutover, Name: "start-service", Status: checkpoint.StepDone},
	}}
	if got := reportFromSteps(rs); len(got.Results) != 0 {
		t.Fatalf("expected no results, got %+v", got.Results)
	}
}

func TestReportFromStepsKeepsTheError(t *testing.T) {
	rs := &checkpoint.RunState{Steps: []checkpoint.StepRecord{
		{Phase: checkpoint.PhaseValidate, Name: "content-integrity", Status: checkpoint.StepFailed, StepOutcome: checkpoint.StepOutcome{Detail: "reached the instance"}, Error: "401 unauthorized"},
	}}
	got := reportFromSteps(rs)
	if d := got.Results[0].Detail; d != "reached the instance (error: 401 unauthorized)" {
		t.Fatalf("detail = %q, want it to carry both the detail and the error", d)
	}
}
