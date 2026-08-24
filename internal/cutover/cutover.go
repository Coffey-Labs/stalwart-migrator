// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package cutover

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/preflight"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/service"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/stalwartapi"
)

// Artifact names this phase records. ArtifactServiceUnit is the preserved
// copy of the service definition as it was before this phase rewrote it -
// recovery is out of scope for this tool (see ARCHITECTURE.md §4.8), but
// the operator putting a machine back by hand should not also have to
// reconstruct their unit file from memory.
const (
	ArtifactNewBinary   = "new-binary"
	ArtifactServiceUnit = "service-unit"
)

// Options configures the cutover phase - ARCHITECTURE.md §4.5.
type Options struct {
	// StagedBinaryPath is the already-downloaded target-version binary.
	// Cutover verifies its version before installing it and never
	// downloads anything itself.
	StagedBinaryPath string
	// BinaryPath is where the staged binary is installed - the path the
	// service definition runs. backup's preserve-binary step has already
	// moved the old binary aside, so this path is normally empty by now.
	BinaryPath string

	// ServiceUnitPath is the systemd unit to rewrite. ConfigPath, if set,
	// becomes the unit's --config argument.
	ServiceUnitPath string
	ConfigPath      string
	// ConfigSource, if set, is installed to ConfigPath before the unit is
	// repointed at it - the converted v0.16 config the migration produced.
	// Its ownership and mode are copied from ConfigOwnerReference (the old
	// config, normally), because the service does not run as root and a
	// root-owned config it cannot read fails the service at startup, not at
	// install time. That is not hypothetical: a full migration crash-looped
	// 28 times on "Failed to read data store settings: Permission denied"
	// for exactly this reason.
	ConfigSource         string
	ConfigOwnerReference string

	// RecoveryPointConfirmed is the operator asserting that a recovery
	// point exists for this machine. This tool does not take one, verify
	// one, or restore from one - see ARCHITECTURE.md §4.8 - so this is an
	// acknowledgement, not a check, and BuildPlan refuses without it. An
	// unverifiable assertion is weaker than a guarantee; making it explicit
	// at least means nobody migrates a production mail server having never
	// been asked the question.
	RecoveryPointConfirmed bool

	Deployment service.Options
	Controller service.Controller

	StartTimeout  time.Duration // waiting for the service to report running; default 60s
	HealthTimeout time.Duration // waiting for it to answer JMAP; default 120s

	AdminURL      string
	AdminUser     string
	AdminPassword string
	HTTPClient    *http.Client

	// RecalculateQuotas schedules the post-migration quota rebuild
	// (ARCHITECTURE.md §4.5's last step). It's needed when crossing the
	// 0.15/0.16 boundary, where Stalwart's own upgrade guide says quotas
	// were reset to zero and have to be rebuilt; a patch bump doesn't
	// touch them. TenantIDs additionally rebuilds tenant-level counters,
	// which only multi-tenant installs have.
	RecalculateQuotas bool
	TenantIDs         []string
	QuotaTimeout      time.Duration // default 30m; large installs legitimately take a while
}

// Plan is what a cutover would do, resolved before anything is touched.
type Plan struct {
	RunID         string
	TargetVersion string
	Target        string // what the service controller acts on

	StagedBinaryPath  string
	BinaryPath        string
	ServiceUnitPath   string
	ConfigPath        string
	ConfigSource      string
	RecalculateQuotas bool
}

func (p Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cutover plan for run %s:\n", p.RunID)
	fmt.Fprintf(&b, "  1. confirm %s really is %s, then install it as %s\n", p.StagedBinaryPath, p.TargetVersion, p.BinaryPath)
	if p.ConfigSource != "" {
		fmt.Fprintf(&b, "  2. install %s as %s, owned so the service user can read it\n", p.ConfigSource, p.ConfigPath)
	}
	fmt.Fprintf(&b, "  3. preserve %s, then point its ExecStart at the new binary and strip any recovery-mode env vars\n", p.ServiceUnitPath)
	fmt.Fprintf(&b, "  3. reload the service definition and start %s\n", p.Target)
	fmt.Fprint(&b, "  4. wait for it to answer an authenticated JMAP session request\n")
	if p.RecalculateQuotas {
		fmt.Fprint(&b, "  5. schedule per-account quota recalculation and wait for the task queue to drain\n")
	} else {
		fmt.Fprint(&b, "  5. skip quota recalculation - not needed for this upgrade path\n")
	}
	return b.String()
}

// BuildPlan resolves the cutover and refuses up front for anything it
// shouldn't attempt. The most important refusal is the first one: a cutover
// is only allowed to proceed if this run could still be rolled back, which
// is what makes "never commit to a change you can't undo" a property of the
// code rather than a claim in a design document.
func BuildPlan(rs *checkpoint.RunState, opts Options) (Plan, error) {
	p := Plan{
		RunID: rs.RunID, TargetVersion: rs.TargetVersion,
		StagedBinaryPath: opts.StagedBinaryPath, BinaryPath: opts.BinaryPath,
		ServiceUnitPath: opts.ServiceUnitPath, ConfigPath: opts.ConfigPath,
		ConfigSource:      opts.ConfigSource,
		RecalculateQuotas: opts.RecalculateQuotas,
	}

	if !opts.RecoveryPointConfirmed {
		return p, fmt.Errorf(
			"cutover: no recovery point has been confirmed for this run. This phase migrates a live mail server in place and this " +
				"tool has no way to undo that - recovery is the operator's own snapshot or backup (ARCHITECTURE.md §4.8). Take one, " +
				"verify you can actually restore from it, and re-run confirming that you have")
	}

	kind := opts.Deployment.Kind
	if kind == "" {
		kind = service.Kind(rs.Topology.DeploymentKind)
	}
	if kind == service.Docker {
		return p, fmt.Errorf(
			"cutover: this run's deployment is a Docker container, where cutting over means pulling a new image and recreating the " +
				"container rather than swapping a binary and rewriting a unit. This tool doesn't automate that - do it by hand")
	}
	deployment := opts.Deployment
	deployment.Kind = kind
	controller := opts.Controller
	if controller == nil {
		var err error
		controller, err = service.New(deployment)
		if err != nil {
			return p, err
		}
	}
	p.Target = controller.Target()

	if opts.StagedBinaryPath == "" {
		return p, fmt.Errorf("cutover: no staged target binary was given - cutover installs an already-downloaded binary, it doesn't fetch one")
	}
	if _, err := os.Stat(opts.StagedBinaryPath); err != nil {
		return p, fmt.Errorf("cutover: staged binary %s: %w", opts.StagedBinaryPath, err)
	}
	if opts.BinaryPath == "" {
		return p, fmt.Errorf("cutover: no path to install the new binary to was given")
	}
	if opts.ServiceUnitPath == "" {
		return p, fmt.Errorf("cutover: no service definition path was given - without one this phase can't point the service at the new binary")
	}
	if _, err := os.Stat(opts.ServiceUnitPath); err != nil {
		return p, fmt.Errorf("cutover: service definition %s: %w", opts.ServiceUnitPath, err)
	}
	return p, nil
}

// Run executes ARCHITECTURE.md §4.5. Every step is checkpointed, and the
// order is chosen so that the irreversible-looking parts happen only after
// the reversible checks pass: the staged binary's version is confirmed
// before it's installed, and the service definition is preserved before
// it's rewritten.
//
// Quota recalculation is the one step allowed to fail without failing the
// cutover, and that's deliberate. Stale quota counters are an accounting
// problem, while a failed cutover is one an operator has to respond to by
// restoring a machine that is otherwise migrated and serving mail
// correctly. Calling for that over a counter would be the worse outcome, so
// this step warns loudly and tells the operator how to finish it by hand.
func Run(ctx context.Context, store *checkpoint.Store, rs *checkpoint.RunState, opts Options) (Report, error) {
	var report Report

	plan, err := BuildPlan(rs, opts)
	if err != nil {
		report.Results = append(report.Results, CheckResult{Name: "plan", Status: StatusFail, Detail: err.Error()})
		return report, err
	}

	controller := opts.Controller
	if controller == nil {
		deployment := opts.Deployment
		if deployment.Kind == "" {
			deployment.Kind = service.Kind(rs.Topology.DeploymentKind)
		}
		controller, err = service.New(deployment)
		if err != nil {
			report.Results = append(report.Results, CheckResult{Name: "plan", Status: StatusFail, Detail: err.Error()})
			return report, err
		}
	}

	step := func(name string, fn func() (checkpoint.StepOutcome, error)) error {
		outcome, err := store.RunStep(rs, checkpoint.PhaseCutover, name, fn)
		if err != nil {
			report.Results = append(report.Results, CheckResult{Name: name, Status: StatusFail, Detail: err.Error()})
			return err
		}
		status := Status(outcome.Verdict)
		if status == "" {
			status = StatusOK
		}
		report.Results = append(report.Results, CheckResult{Name: name, Status: status, Detail: outcome.Detail})
		return nil
	}

	if err := step("verify-staged-binary", func() (checkpoint.StepOutcome, error) {
		got, err := preflight.DetectVersion(ctx, plan.StagedBinaryPath)
		if err != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf("couldn't read the staged binary's version: %w", err)
		}
		if rs.TargetVersion != "" && got != rs.TargetVersion {
			return checkpoint.StepOutcome{}, fmt.Errorf(
				"staged binary %s reports version %s, but this run targets %s - installing it would migrate to a version nobody planned for",
				plan.StagedBinaryPath, got, rs.TargetVersion)
		}
		return checkpoint.StepOutcome{Detail: fmt.Sprintf("staged binary %s reports %s, matching this run's target", plan.StagedBinaryPath, got), Extra: got}, nil
	}); err != nil {
		return report, err
	}

	if err := step("install-binary", func() (checkpoint.StepOutcome, error) {
		sum, size, err := installBinary(plan.StagedBinaryPath, plan.BinaryPath)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		rs.RecordArtifact(ArtifactNewBinary, checkpoint.Artifact{Path: plan.BinaryPath, SHA256: sum, SizeBytes: size})
		return checkpoint.StepOutcome{Detail: fmt.Sprintf("installed %s as %s (%d bytes)", plan.StagedBinaryPath, plan.BinaryPath, size)}, nil
	}); err != nil {
		return report, err
	}

	if err := step("install-config", func() (checkpoint.StepOutcome, error) {
		if plan.ConfigSource == "" {
			return checkpoint.StepOutcome{
				Verdict: string(StatusSkipped),
				Detail:  "no converted config to install - the unit is repointed at whatever is already at ConfigPath",
			}, nil
		}
		owner, err := installConfig(plan.ConfigSource, plan.ConfigPath, opts.ConfigOwnerReference)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		return checkpoint.StepOutcome{Detail: fmt.Sprintf("installed %s as %s (%s)", plan.ConfigSource, plan.ConfigPath, owner)}, nil
	}); err != nil {
		return report, err
	}

	if err := step("update-service-definition", func() (checkpoint.StepOutcome, error) {
		preserved, err := preserveUnit(plan.ServiceUnitPath, rs.RunID)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		sum, size, err := hashFile(preserved)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		// Recorded before the rewrite, so a crash between preserving and
		// rewriting still leaves the original findable.
		rs.RecordArtifact(ArtifactServiceUnit, checkpoint.Artifact{Path: preserved, SHA256: sum, SizeBytes: size})

		original, err := os.ReadFile(preserved)
		if err != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf("cutover: read preserved unit %s: %w", preserved, err)
		}
		rewritten, err := RewriteUnit(string(original), plan.BinaryPath, plan.ConfigPath)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		if err := writeFileAtomic(plan.ServiceUnitPath, []byte(rewritten), 0o644); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		return checkpoint.StepOutcome{
			Detail: fmt.Sprintf("pointed %s at %s; the original is preserved at %s", plan.ServiceUnitPath, plan.BinaryPath, preserved),
			Extra:  preserved,
		}, nil
	}); err != nil {
		return report, err
	}

	if err := step("reload-service-definition", func() (checkpoint.StepOutcome, error) {
		if err := controller.ReloadConfig(ctx); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		return checkpoint.StepOutcome{Detail: "service manager re-read the updated definition"}, nil
	}); err != nil {
		return report, err
	}

	startTimeout := opts.StartTimeout
	if startTimeout <= 0 {
		startTimeout = 60 * time.Second
	}
	if err := step("start-service", func() (checkpoint.StepOutcome, error) {
		if err := controller.Start(ctx); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		if err := service.WaitFor(ctx, controller, true, startTimeout); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		return checkpoint.StepOutcome{Detail: fmt.Sprintf("%s is running the migrated instance", controller.Target())}, nil
	}); err != nil {
		return report, err
	}

	healthTimeout := opts.HealthTimeout
	if healthTimeout <= 0 {
		healthTimeout = 120 * time.Second
	}
	if err := step("wait-healthy", func() (checkpoint.StepOutcome, error) {
		if opts.AdminURL == "" {
			return checkpoint.StepOutcome{
				Verdict: string(StatusSkipped),
				Detail:  "no admin URL configured - the service was started, but nothing confirmed it actually answers",
			}, nil
		}
		client := newClient(opts)
		// Liveness first, and on its own. Any response - 401 included -
		// proves the service is up and routing.
		if err := client.WaitForResponse(ctx, healthTimeout); err != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf("the migrated service started but never answered at %s within %s: %w", opts.AdminURL, healthTimeout, err)
		}
		// Credentials are a separate question, and failing them is not a
		// failed cutover. A config fallback-admin does not survive into
		// v0.16 - its config is just a store pointer, so the old
		// [authentication.fallback-admin] block is gone - so the
		// credentials that worked before the migration routinely stop
		// working after it, on an instance that is otherwise fine.
		if err := client.Ping(ctx); err != nil {
			return checkpoint.StepOutcome{
				Verdict: string(StatusWarn),
				Detail: fmt.Sprintf("migrated instance is up and answering at %s, but these admin credentials no longer work: %v. "+
					"If they were a config fallback-admin, that does not survive the migration - v0.16 keeps its config in the store. "+
					"Authenticate as an account that exists in the directory instead", opts.AdminURL, err),
			}, nil
		}
		return checkpoint.StepOutcome{Detail: fmt.Sprintf("migrated instance answered an authenticated JMAP session request at %s", opts.AdminURL)}, nil
	}); err != nil {
		return report, err
	}

	// From here on, failures are warnings: the service is up and serving
	// mail, and rolling that back over a quota counter would be worse than
	// leaving the counter stale.
	quotaOutcome, quotaErr := store.RunStep(rs, checkpoint.PhaseCutover, "recalculate-quotas", func() (checkpoint.StepOutcome, error) {
		return recalculateQuotas(ctx, opts)
	})
	switch {
	case quotaErr != nil:
		report.Results = append(report.Results, CheckResult{
			Name: "recalculate-quotas", Status: StatusWarn,
			Detail: fmt.Sprintf("%v - the migration is complete and serving mail; finish this from the WebUI's Tasks panel "+
				"(\"Recalculate disk quotas\"), and note that until it runs, per-account usage counters read low", quotaErr),
		})
	default:
		status := Status(quotaOutcome.Verdict)
		if status == "" {
			status = StatusOK
		}
		report.Results = append(report.Results, CheckResult{Name: "recalculate-quotas", Status: status, Detail: quotaOutcome.Detail})
	}

	return report, nil
}

func newClient(opts Options) *stalwartapi.Client {
	return &stalwartapi.Client{
		BaseURL: opts.AdminURL, Username: opts.AdminUser, Password: opts.AdminPassword, HTTPClient: opts.HTTPClient,
	}
}

func recalculateQuotas(ctx context.Context, opts Options) (checkpoint.StepOutcome, error) {
	if !opts.RecalculateQuotas {
		return checkpoint.StepOutcome{
			Verdict: string(StatusSkipped),
			Detail:  "not needed for this upgrade path - quotas are only reset by the 0.15/0.16 schema migration",
		}, nil
	}
	if opts.AdminURL == "" {
		return checkpoint.StepOutcome{
			Verdict: string(StatusSkipped),
			Detail:  "no admin URL configured - quota recalculation has to be triggered from the WebUI's Tasks panel by hand",
		}, nil
	}

	timeout := opts.QuotaTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	client := newClient(opts)

	accountIDs, err := client.AccountIDs(ctx)
	if err != nil {
		return checkpoint.StepOutcome{}, fmt.Errorf("couldn't enumerate accounts to recalculate: %w", err)
	}
	if len(accountIDs) == 0 {
		return checkpoint.StepOutcome{Verdict: string(StatusSkipped), Detail: "the instance reports no accounts, so there are no quotas to rebuild"}, nil
	}

	taskIDs, err := client.CreateQuotaRecalculationTasks(ctx, accountIDs)
	if err != nil {
		return checkpoint.StepOutcome{}, err
	}
	failures, err := client.WaitForTasks(ctx, taskIDs, timeout)
	if err != nil {
		return checkpoint.StepOutcome{}, err
	}
	if len(failures) > 0 {
		return checkpoint.StepOutcome{}, fmt.Errorf("%d of %d account quota task(s) failed: %v", len(failures), len(taskIDs), failures)
	}
	detail := fmt.Sprintf("rebuilt disk quotas for %d account(s)", len(accountIDs))

	// Tenant totals aggregate the per-account numbers, so they can only run
	// once every account task above has finished - which is why this is
	// sequenced after the wait rather than scheduled alongside it.
	if len(opts.TenantIDs) > 0 {
		tenantTasks, err := client.CreateTenantQuotaRecalculationTasks(ctx, opts.TenantIDs)
		if err != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf("%s, but scheduling tenant recalculation failed: %w", detail, err)
		}
		tenantFailures, err := client.WaitForTasks(ctx, tenantTasks, timeout)
		if err != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf("%s, but waiting on tenant recalculation failed: %w", detail, err)
		}
		if len(tenantFailures) > 0 {
			return checkpoint.StepOutcome{}, fmt.Errorf("%s, but %d tenant quota task(s) failed: %v", detail, len(tenantFailures), tenantFailures)
		}
		detail += fmt.Sprintf(" and %d tenant(s)", len(opts.TenantIDs))
	}
	return checkpoint.StepOutcome{Detail: detail}, nil
}

// installBinary copies the staged binary into place through a temp file in
// the same directory, so the service definition never points at a
// half-written executable. It's idempotent: a retry that finds the right
// bytes already installed reports them rather than copying again.
func installBinary(stagedPath, binaryPath string) (sha256Hex string, size int64, err error) {
	stagedSum, stagedSize, err := hashFile(stagedPath)
	if err != nil {
		return "", 0, err
	}
	if existingSum, existingSize, err := hashFile(binaryPath); err == nil && existingSum == stagedSum {
		return existingSum, existingSize, nil
	}

	data, err := os.ReadFile(stagedPath)
	if err != nil {
		return "", 0, fmt.Errorf("cutover: read staged binary %s: %w", stagedPath, err)
	}
	if err := writeFileAtomic(binaryPath, data, 0o755); err != nil {
		return "", 0, err
	}
	return stagedSum, stagedSize, nil
}

// preserveUnit copies the current service definition to
// "<path>.pre-<run-id>" so an operator restoring by hand has the original,
// and returns that path.
// Idempotent: a retry finds the copy already there and keeps it, since the
// file at path may by then be this phase's own rewrite.
func preserveUnit(unitPath, runID string) (preservedPath string, err error) {
	preservedPath = fmt.Sprintf("%s.pre-%s", unitPath, runID)
	if _, err := os.Stat(preservedPath); err == nil {
		return preservedPath, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("cutover: stat %s: %w", preservedPath, err)
	}
	data, err := os.ReadFile(unitPath)
	if err != nil {
		return "", fmt.Errorf("cutover: read service definition %s: %w", unitPath, err)
	}
	perm := os.FileMode(0o644)
	if info, err := os.Stat(unitPath); err == nil {
		perm = info.Mode().Perm()
	}
	if err := writeFileAtomic(preservedPath, data, perm); err != nil {
		return "", err
	}
	return preservedPath, nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("cutover: create temp file next to %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("cutover: write %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("cutover: chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("cutover: sync %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cutover: close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("cutover: move %s into place at %s: %w", tmpPath, path, err)
	}
	return nil
}

func hashFile(path string) (sha256Hex string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("cutover: hash %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("cutover: hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// installConfig places the converted config at dst, copying ownership and
// mode from reference (normally the config being replaced) so the service
// user can still read it.
//
// Ownership is the whole point of this function. Writing a config as root
// is the natural thing for a tool running as root to do, and it produces a
// service that starts, fails to read its own config, and restarts forever -
// a failure that shows up minutes later in the journal rather than at the
// moment of the mistake. Where no reference is available the file is left
// world-readable, since a config the service cannot read is worse than one
// other local users can.
func installConfig(src, dst, reference string) (ownership string, err error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("cutover: read converted config %s: %w", src, err)
	}

	perm := os.FileMode(0o644)
	uid, gid := -1, -1
	if reference == "" {
		reference = dst // fall back to whatever is already in place
	}
	if info, statErr := os.Stat(reference); statErr == nil {
		perm = info.Mode().Perm()
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(sys.Uid), int(sys.Gid)
		}
	}

	if err := writeFileAtomic(dst, data, perm); err != nil {
		return "", err
	}
	if uid >= 0 && gid >= 0 {
		if err := os.Chown(dst, uid, gid); err != nil {
			return "", fmt.Errorf("cutover: set ownership on %s to %d:%d - the service runs as that user and cannot read a config it does not own: %w", dst, uid, gid, err)
		}
		return fmt.Sprintf("uid %d, gid %d, mode %v", uid, gid, perm), nil
	}
	return fmt.Sprintf("mode %v, ownership unchanged (no reference file to copy it from)", perm), nil
}
