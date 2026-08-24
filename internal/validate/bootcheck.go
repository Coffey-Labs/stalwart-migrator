// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package validate

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/recovery"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/stalwartapi"
)

// BootCheckOptions configures a normal (non-recovery-mode) boot of the
// migrated instance, to confirm it comes up cleanly outside recovery mode -
// not just that recovery mode itself could apply settings to it - and,
// optionally, a content-integrity comparison performed against that same
// boot before it's stopped again.
type BootCheckOptions struct {
	BinaryPath string
	ConfigPath string
	ListenURL  string
	ExtraEnv   []string
	Timeout    time.Duration
	StopGrace  time.Duration
	HTTPClient *http.Client

	// ContentIntegrityBefore, if non-nil, is the pre-migration snapshot
	// preflight captured (checkpoint.RunState.PreflightSnapshot). When set,
	// BootCheck captures a fresh snapshot from the instance it just booted
	// - authenticating with AdminUser/AdminPassword, which migrate over
	// unchanged with the account (they don't need to differ from the
	// pre-migration admin credentials) - and compares the two: this is the
	// actual no-data-loss guarantee from ARCHITECTURE.md §4.7, not just
	// "the migration mechanics ran". Left nil, only the boot-reachability
	// check runs, e.g. when preflight never captured a snapshot because
	// --admin-url wasn't set.
	ContentIntegrityBefore *checkpoint.PreflightSnapshot
	AdminUser              string
	AdminPassword          string
}

// BootCheck starts the target binary the way cutover eventually will (an
// ordinary boot, no STALWART_RECOVERY_MODE), waits for its HTTP listener to
// answer, optionally compares its content against ContentIntegrityBefore
// while it's up, then stops it. It reuses recovery.Process and
// recovery.WaitForHealthy rather than re-implementing process supervision,
// since "start the binary and confirm it's reachable" is exactly what those
// already do.
//
// Like recovery.Run, this is deliberately one atomic operation rather than
// separately checkpointed sub-steps: if this tool's own process crashes
// between the boot succeeding and the content check running, there's no
// safe way to reattach to whatever's left of the child process on resume,
// so a retry just redoes the whole cycle - see recovery.Run's doc comment
// for the full reasoning, which applies identically here.
func BootCheck(ctx context.Context, o BootCheckOptions) (detail string, result *ContentIntegrityResult, err error) {
	proc := &recovery.Process{}
	if startErr := proc.Start(ctx, recovery.ProcessOptions{
		BinaryPath: o.BinaryPath, ConfigPath: o.ConfigPath, RecoveryMode: false, ExtraEnv: o.ExtraEnv,
	}); startErr != nil {
		return "", nil, fmt.Errorf("validate: start normal boot: %w", startErr)
	}

	stopGrace := o.StopGrace
	if stopGrace <= 0 {
		stopGrace = 10 * time.Second
	}
	defer func() {
		if stopErr := proc.Stop(stopGrace); stopErr != nil && err == nil {
			err = stopErr
		}
	}()

	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if healthErr := recovery.WaitForHealthy(ctx, o.HTTPClient, o.ListenURL, timeout); healthErr != nil {
		out := strings.TrimSpace(proc.Output())
		if out == "" {
			out = "(the process produced no output)"
		}
		return "", nil, fmt.Errorf("migrated instance did not come up under a normal (non-recovery-mode) boot: %w\n"+
			"--- output from the supervised Stalwart process ---\n%s\n--- end of output ---", healthErr, out)
	}
	detail = fmt.Sprintf("migrated instance booted normally (not in recovery mode) and answered at %s", o.ListenURL)

	if o.ContentIntegrityBefore == nil {
		return detail, nil, nil
	}

	client := &stalwartapi.Client{BaseURL: o.ListenURL, Username: o.AdminUser, Password: o.AdminPassword, HTTPClient: o.HTTPClient}
	result, ciErr := compareContentIntegrity(ctx, client, o.ContentIntegrityBefore)
	if ciErr != nil {
		return detail, nil, fmt.Errorf("content-integrity comparison failed: %w", ciErr)
	}
	if !result.OK() {
		return detail, result, fmt.Errorf("content integrity check found problems: %s", result.String())
	}
	return detail, result, nil
}

// Run executes BootCheck as a single checkpointed step, mirroring
// preflight/backup/recovery's pattern.
func Run(ctx context.Context, store *checkpoint.Store, rs *checkpoint.RunState, opts BootCheckOptions) (Report, error) {
	var report Report
	outcome, err := store.RunStep(rs, checkpoint.PhaseValidate, "boot-check", func() (checkpoint.StepOutcome, error) {
		detail, result, err := BootCheck(ctx, opts)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		if result != nil {
			detail += " - " + result.String()
		}
		return checkpoint.StepOutcome{Detail: detail}, nil
	})
	if err != nil {
		report.Results = append(report.Results, CheckResult{Name: "boot-check", Status: StatusFail, Detail: err.Error()})
		return report, err
	}
	report.Results = append(report.Results, CheckResult{Name: "boot-check", Status: StatusOK, Detail: outcome.Detail})
	return report, nil
}
