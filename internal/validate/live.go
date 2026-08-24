// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package validate

import (
	"context"
	"fmt"
	"net/http"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/stalwartapi"
)

// LiveOptions describes the migrated instance cutover has just started.
type LiveOptions struct {
	AdminURL      string
	AdminUser     string
	AdminPassword string
	HTTPClient    *http.Client

	// Before is what preflight captured before anything was touched
	// (checkpoint.RunState.PreflightSnapshot). Nil when preflight had no
	// admin URL to capture it from, in which case there is nothing to
	// compare against and the check reports that rather than passing.
	Before *checkpoint.PreflightSnapshot
}

// CheckLive compares a running instance against the pre-migration snapshot.
//
// The same comparison BootCheck performs against an instance it booted
// itself, aimed instead at the service cutover has already started. That is
// the instance people will actually use — its real config, its real ports,
// under its real service manager — and checking it costs no extra downtime,
// where booting a second copy inside the maintenance window would.
func CheckLive(ctx context.Context, client *stalwartapi.Client, before *checkpoint.PreflightSnapshot) (*ContentIntegrityResult, error) {
	return compareContentIntegrity(ctx, client, before)
}

// RunLive executes the post-cutover content comparison as a checkpointed
// step, mirroring how every other phase records itself.
//
// A missing snapshot or admin URL is reported as skipped, never as a pass:
// "every account survived" and "we were unable to look" are different
// answers, and ARCHITECTURE.md §4.7 is explicit that this suite must not
// imply a guarantee it did not measure.
func RunLive(ctx context.Context, store *checkpoint.Store, rs *checkpoint.RunState, opts LiveOptions) (Report, error) {
	var report Report

	switch {
	case opts.AdminURL == "":
		report.Results = append(report.Results, CheckResult{
			Name: "content-integrity", Status: StatusSkip,
			Detail: "no admin URL configured - nothing could be compared against the migrated instance",
		})
		return report, nil
	case opts.Before == nil:
		report.Results = append(report.Results, CheckResult{
			Name: "content-integrity", Status: StatusSkip,
			Detail: "preflight captured no pre-migration snapshot - there is nothing to compare the migrated instance against",
		})
		return report, nil
	}

	client := &stalwartapi.Client{
		BaseURL: opts.AdminURL, Username: opts.AdminUser, Password: opts.AdminPassword, HTTPClient: opts.HTTPClient,
	}

	outcome, err := store.RunStep(rs, checkpoint.PhaseValidate, "content-integrity", func() (checkpoint.StepOutcome, error) {
		r, err := CheckLive(ctx, client, opts.Before)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		if !r.OK() {
			// Recorded as a completed step with a failing verdict rather
			// than an error: the comparison ran, and its answer is the
			// finding. An error here would read as "we could not look".
			return checkpoint.StepOutcome{Verdict: string(StatusFail), Detail: r.String()}, nil
		}
		return checkpoint.StepOutcome{Detail: r.String()}, nil
	})
	if err != nil {
		report.Results = append(report.Results, CheckResult{
			Name: "content-integrity", Status: StatusFail,
			Detail: fmt.Sprintf("could not compare the migrated instance against the pre-migration snapshot: %v", err),
		})
		return report, err
	}

	status := StatusOK
	if outcome.Verdict == string(StatusFail) {
		status = StatusFail
	}
	report.Results = append(report.Results, CheckResult{Name: "content-integrity", Status: status, Detail: outcome.Detail})
	return report, nil
}
