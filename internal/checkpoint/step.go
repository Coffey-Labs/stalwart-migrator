// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package checkpoint

import (
	"fmt"
	"time"
)

func (rs *RunState) stepIndex(phase Phase, name string) int {
	for i := range rs.Steps {
		if rs.Steps[i].Phase == phase && rs.Steps[i].Name == name {
			return i
		}
	}
	return -1
}

// Status returns the current status of a step, or StepPending if it has
// never been started.
func (rs *RunState) Status(phase Phase, name string) StepStatus {
	if i := rs.stepIndex(phase, name); i >= 0 {
		return rs.Steps[i].Status
	}
	return StepPending
}

// Done reports whether a step already completed successfully. Callers use
// this to decide whether to skip work on resume.
func (rs *RunState) Done(phase Phase, name string) bool {
	return rs.Status(phase, name) == StepDone
}

// Outcome returns the StepOutcome recorded for a step, zero-valued if none.
// This is what lets a resumed run reconstruct a skipped step's result
// without re-executing it.
func (rs *RunState) Outcome(phase Phase, name string) StepOutcome {
	if i := rs.stepIndex(phase, name); i >= 0 {
		return rs.Steps[i].StepOutcome
	}
	return StepOutcome{}
}

// Begin marks a step as running, creating its record on first attempt or
// resetting it on a retry after a prior failure.
func (rs *RunState) Begin(phase Phase, name string) {
	now := time.Now().UTC()
	if i := rs.stepIndex(phase, name); i >= 0 {
		rs.Steps[i].Status = StepRunning
		rs.Steps[i].StartedAt = &now
		rs.Steps[i].CompletedAt = nil
		rs.Steps[i].StepOutcome = StepOutcome{}
		rs.Steps[i].Error = ""
	} else {
		rs.Steps = append(rs.Steps, StepRecord{
			Phase: phase, Name: name, Status: StepRunning, StartedAt: &now,
		})
	}
	rs.UpdatedAt = now
}

// Complete marks a step done with its outcome. It panics if Begin was never
// called for this step - that's a bug in the calling phase, not a runtime
// condition callers should need to handle.
func (rs *RunState) Complete(phase Phase, name string, outcome StepOutcome) {
	i := rs.stepIndex(phase, name)
	if i < 0 {
		panic(fmt.Sprintf("checkpoint: Complete(%s/%s) called without Begin", phase, name))
	}
	now := time.Now().UTC()
	rs.Steps[i].Status = StepDone
	rs.Steps[i].CompletedAt = &now
	rs.Steps[i].StepOutcome = outcome
	rs.Steps[i].Error = ""
	rs.UpdatedAt = now
}

// Fail marks a step failed, so a later Begin for the same (phase, name)
// knows to retry it rather than treat it as done.
func (rs *RunState) Fail(phase Phase, name string, stepErr error) {
	i := rs.stepIndex(phase, name)
	if i < 0 {
		panic(fmt.Sprintf("checkpoint: Fail(%s/%s) called without Begin", phase, name))
	}
	now := time.Now().UTC()
	rs.Steps[i].Status = StepFailed
	rs.Steps[i].CompletedAt = &now
	rs.Steps[i].Error = stepErr.Error()
	rs.UpdatedAt = now
}
