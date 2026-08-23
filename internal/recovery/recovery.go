package recovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/johnellis/stalwart-migrator/internal/checkpoint"
)

// Options configures one full recovery-mode migration cycle: starting the
// target binary in recovery mode, waiting for it to come up, applying the
// settings snapshot(s), and stopping it again. See ARCHITECTURE.md §4.4.
type Options struct {
	BinaryPath     string
	ConfigPath     string
	ListenURL      string // recovery mode's own HTTP listener, e.g. "http://127.0.0.1:8080"
	AdminUser      string
	ApplyFiles     []string
	CLIBinaryPath  string
	ExtraEnv       []string // lets a dry-run point ports/paths at a sandbox without touching production config
	StartupTimeout time.Duration
	StopGrace      time.Duration
	HTTPClient     *http.Client
}

// GenerateRecoveryPassword returns a fresh random one-time password for
// STALWART_RECOVERY_ADMIN - never the operator's real admin password, never
// logged, never reused across runs or persisted to the checkpoint.
func GenerateRecoveryPassword() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("recovery: generate password: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Run executes one recovery-mode cycle as a single checkpointed step. It is
// deliberately not decomposed into per-sub-step checkpoints the way
// preflight and backup are: if this tool's own process crashes mid-cycle,
// the child Stalwart process it started may or may not still be running
// independently, and blindly "resuming" by reattaching to a guessed PID or
// killing an unrelated process on the recovery port would be more dangerous
// than just retrying cleanly. A retry that hits "address already in use"
// surfaces the real problem (an orphaned process from the failed attempt)
// for a human to clear, rather than this tool guessing at cleanup.
//
// Whatever happens after Start succeeds, Stop is always attempted on the
// way out (via a deferred call), so a failure partway through this cycle
// doesn't leak the child process within a single invocation.
func Run(ctx context.Context, store *checkpoint.Store, rs *checkpoint.RunState, opts Options) (Report, error) {
	var report Report

	outcome, err := store.RunStep(rs, checkpoint.PhaseRecovery, "recovery-cycle", func() (out checkpoint.StepOutcome, err error) {
		password, err := GenerateRecoveryPassword()
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}

		proc := &Process{}
		if startErr := proc.Start(ctx, ProcessOptions{
			BinaryPath: opts.BinaryPath, ConfigPath: opts.ConfigPath,
			RecoveryMode: true, AdminUser: opts.AdminUser, AdminPassword: password,
			ExtraEnv: opts.ExtraEnv,
		}); startErr != nil {
			return checkpoint.StepOutcome{}, startErr
		}

		stopGrace := opts.StopGrace
		if stopGrace <= 0 {
			stopGrace = 10 * time.Second
		}
		defer func() {
			if stopErr := proc.Stop(stopGrace); stopErr != nil && err == nil {
				err = stopErr
			}
		}()

		startupTimeout := opts.StartupTimeout
		if startupTimeout <= 0 {
			startupTimeout = 60 * time.Second
		}
		if healthErr := WaitForHealthy(ctx, opts.HTTPClient, opts.ListenURL, startupTimeout); healthErr != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf("recovery mode did not come up: %w", healthErr)
		}

		if applyErr := ApplyAll(ctx, ApplyOptions{
			CLIBinaryPath: opts.CLIBinaryPath, URL: opts.ListenURL, User: opts.AdminUser, Password: password,
		}, opts.ApplyFiles); applyErr != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf("settings apply failed: %w", applyErr)
		}

		return checkpoint.StepOutcome{
			Detail: fmt.Sprintf("recovery mode came up at %s, applied %d settings file(s), stopped cleanly", opts.ListenURL, len(opts.ApplyFiles)),
		}, nil
	})

	if err != nil {
		report.Results = append(report.Results, CheckResult{Name: "recovery-cycle", Status: StatusFail, Detail: err.Error()})
		return report, err
	}
	report.Results = append(report.Results, CheckResult{Name: "recovery-cycle", Status: StatusOK, Detail: outcome.Detail})
	return report, nil
}
