package rollback

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/backup"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/service"
)

// Artifact names this phase reads out of the checkpoint. backup.Run writes
// the first three as it captures them; ArtifactServiceUnit is the contract
// a future cutover phase has to honour when it rewrites a systemd unit or
// Compose file, so this phase can put the original back.
const (
	ArtifactOldBinary   = "old-binary"
	ArtifactFSBackup    = "fs-backup"
	ArtifactSQLDump     = "sql-dump"
	ArtifactServiceUnit = "service-unit"
)

// Options configures one rollback. Most paths default to what the run's own
// checkpoint recorded, so an operator rolling back days later doesn't have
// to remember - and can't mistype - where the backup went.
type Options struct {
	// Deployment names the service to stop and start. Controller overrides
	// it outright when a caller already has one (cutover will, and the
	// tests do).
	Deployment service.Options
	Controller service.Controller

	StopTimeout  time.Duration // how long to wait for the service to actually stop; default 60s
	StartTimeout time.Duration // how long to wait for it to come back; default 60s

	// DataDir is where an embedded-backend store is restored to. Required
	// for rocksdb/sqlite runs.
	DataDir string
	// BackupDir and ManifestPath default to the fs-snapshot step's recorded
	// artifact and manifest.
	BackupDir    string
	ManifestPath string

	// SQL configures an external-database restore. OutPath defaults to the
	// recorded sql-dump artifact; the connection fields must be supplied,
	// since the checkpoint deliberately never stores database credentials.
	SQL backup.SQLOptions

	// BinaryPath is where the preserved old binary goes back to.
	BinaryPath string
	// ServiceUnitPath is where a preserved systemd unit or Compose file
	// goes back to. Both this and an ArtifactServiceUnit record are needed
	// for that step to do anything.
	ServiceUnitPath string

	AdminURL      string
	AdminUser     string
	AdminPassword string
	HTTPClient    *http.Client
	VerifyTimeout time.Duration
}

// Plan is what a rollback would do, resolved from the run's checkpoint
// before anything is touched. Building it can fail; executing it is what
// takes mail delivery down, so every reason to refuse is found here first.
type Plan struct {
	RunID         string
	SourceVersion string // the version this rollback restores the instance to
	Method        string // "filesystem", "postgresql", or "mysql"

	Target string // what the service controller acts on

	BackupDir    string
	ManifestPath string
	DataDir      string
	SQLDumpPath  string
	SQLDatabase  string

	PreservedBinary string // preserved old binary to reinstall, "" if none was preserved
	BinaryPath      string

	ServiceUnitSource string // preserved unit/compose file, "" if none
	ServiceUnitDest   string
}

// String renders the plan as the confirmation an operator should read
// before agreeing to it - this phase overwrites a live data directory, so
// "what exactly is about to happen" has to be answerable without reading
// the source.
func (p Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "rollback plan for run %s:\n", p.RunID)
	fmt.Fprintf(&b, "  1. stop %s\n", p.Target)
	switch p.Method {
	case "filesystem":
		fmt.Fprintf(&b, "  2. move %s aside to %s.failed-%s (nothing is deleted)\n", p.DataDir, p.DataDir, p.RunID)
		fmt.Fprintf(&b, "  3. restore %s from the verified backup at %s\n", p.DataDir, p.BackupDir)
	default:
		fmt.Fprintf(&b, "  2. replay the %s critical-table dump at %s into database %q\n", p.Method, p.SQLDumpPath, p.SQLDatabase)
		fmt.Fprintf(&b, "     (this overwrites those tables in place - unlike the filesystem path, the current contents are NOT preserved)\n")
	}
	if p.PreservedBinary != "" {
		fmt.Fprintf(&b, "  4. reinstall %s as %s (the current binary is moved aside, not deleted)\n", p.PreservedBinary, p.BinaryPath)
	} else {
		fmt.Fprintf(&b, "  4. leave the binary alone - this run never preserved one\n")
	}
	if p.ServiceUnitSource != "" {
		fmt.Fprintf(&b, "  5. restore the service definition %s to %s\n", p.ServiceUnitSource, p.ServiceUnitDest)
	} else {
		fmt.Fprintf(&b, "  5. leave the service definition alone - this run never preserved one\n")
	}
	version := p.SourceVersion
	if version == "" {
		version = "the version this run started from"
	}
	fmt.Fprintf(&b, "  6. start %s and verify it came back on %s\n", p.Target, version)
	return b.String()
}

// BuildPlan resolves what a rollback of this run would do, refusing up
// front for anything it can't undo. Every refusal here happens before the
// service is stopped, which is the whole point of separating this from Run:
// discovering "there's no backup to restore" after taking mail delivery
// down would be the worst possible time to find out.
func BuildPlan(rs *checkpoint.RunState, opts Options) (Plan, error) {
	p := Plan{
		RunID: rs.RunID, SourceVersion: rs.SourceVersion,
		BinaryPath: opts.BinaryPath, ServiceUnitDest: opts.ServiceUnitPath,
		SQLDatabase: opts.SQL.Database,
	}

	if rs.RollbackWindowClosed {
		return p, fmt.Errorf(
			"rollback: run %s had its rollback window closed by `confirm` - the operator declared the migration good, "+
				"and the backups this would restore from may since have been pruned. Restore manually if you're sure", rs.RunID)
	}

	controller := opts.Controller
	if controller == nil {
		var err error
		controller, err = service.New(deploymentFor(rs, opts))
		if err != nil {
			return p, err
		}
	}
	p.Target = controller.Target()

	backends := strings.ToLower(rs.Topology.StoreBackend)
	switch {
	case strings.Contains(backends, "rocksdb") || strings.Contains(backends, "sqlite"):
		p.Method = "filesystem"
		art, found := rs.Artifacts[ArtifactFSBackup]
		if !found && opts.BackupDir == "" {
			return p, fmt.Errorf("rollback: run %s recorded no %s artifact - there is no filesystem backup to restore, so this run cannot be rolled back by this tool", rs.RunID, ArtifactFSBackup)
		}
		p.BackupDir = opts.BackupDir
		if p.BackupDir == "" {
			p.BackupDir = art.Path
		}
		p.ManifestPath = opts.ManifestPath
		if p.ManifestPath == "" {
			p.ManifestPath = rs.Outcome(checkpoint.PhaseBackup, "fs-snapshot").Extra
		}
		if p.ManifestPath == "" {
			return p, fmt.Errorf("rollback: run %s recorded no backup manifest path - without it the backup can't be verified before it's restored; pass one explicitly if you have it", rs.RunID)
		}
		if opts.DataDir == "" {
			return p, fmt.Errorf("rollback: a data directory to restore into is required for a %s backend", rs.Topology.StoreBackend)
		}
		p.DataDir = opts.DataDir

	case strings.Contains(backends, "postgresql"), strings.Contains(backends, "mysql"):
		p.Method = "postgresql"
		if strings.Contains(backends, "mysql") {
			p.Method = "mysql"
		}
		art, found := rs.Artifacts[ArtifactSQLDump]
		if !found && opts.SQL.OutPath == "" {
			return p, fmt.Errorf("rollback: run %s recorded no %s artifact - there is no database dump to restore", rs.RunID, ArtifactSQLDump)
		}
		p.SQLDumpPath = opts.SQL.OutPath
		if p.SQLDumpPath == "" {
			p.SQLDumpPath = art.Path
		}
		if opts.SQL.Database == "" || opts.SQL.User == "" {
			return p, fmt.Errorf("rollback: database name and user are required to restore a %s backend - the checkpoint deliberately doesn't store database credentials", p.Method)
		}

	case strings.Contains(backends, "foundationdb"):
		return p, fmt.Errorf(
			"rollback: run %s uses a FoundationDB backend, whose backup step only *starts* an fdbbackup job - restoring it means "+
				"`fdbrestore` against a quiesced cluster, which this tool doesn't automate. Roll back manually and don't rely on this command", rs.RunID)

	default:
		return p, fmt.Errorf(
			"rollback: run %s recorded no recognized store backend (topology.store_backend=%q), so there's no way to know what to restore",
			rs.RunID, rs.Topology.StoreBackend)
	}

	if art, found := rs.Artifacts[ArtifactOldBinary]; found {
		if opts.BinaryPath == "" {
			return p, fmt.Errorf("rollback: run %s preserved the old binary at %s, but no path to reinstall it to was given", rs.RunID, art.Path)
		}
		p.PreservedBinary = art.Path
	}
	if art, found := rs.Artifacts[ArtifactServiceUnit]; found && opts.ServiceUnitPath != "" {
		p.ServiceUnitSource = art.Path
	}
	return p, nil
}

func deploymentFor(rs *checkpoint.RunState, opts Options) service.Options {
	d := opts.Deployment
	if d.Kind == "" {
		d.Kind = service.Kind(rs.Topology.DeploymentKind)
	}
	return d
}

// Run executes ARCHITECTURE.md §4.8: stop the service, put the verified
// pre-migration state back, restart the old binary, and confirm the
// restored instance actually works rather than assuming it does. Every step
// is checkpointed under PhaseRollback, so a rollback interrupted partway -
// which is exactly when a machine is most likely to be rebooted out from
// under it - resumes where it stopped instead of restarting a destructive
// sequence from the top.
//
// Nothing from the failed attempt is deleted: the half-migrated data
// directory and the new binary are moved aside under ".failed-<run-id>"
// names, so a retry after the underlying issue is fixed still has both the
// evidence and the artifacts it needs.
func Run(ctx context.Context, store *checkpoint.Store, rs *checkpoint.RunState, opts Options) (Report, error) {
	var report Report

	plan, err := BuildPlan(rs, opts)
	if err != nil {
		report.Results = append(report.Results, CheckResult{Name: "plan", Status: StatusFail, Detail: err.Error()})
		return report, err
	}

	controller := opts.Controller
	if controller == nil {
		controller, err = service.New(deploymentFor(rs, opts))
		if err != nil {
			report.Results = append(report.Results, CheckResult{Name: "plan", Status: StatusFail, Detail: err.Error()})
			return report, err
		}
	}

	step := func(name string, fn func() (checkpoint.StepOutcome, error)) error {
		outcome, err := store.RunStep(rs, checkpoint.PhaseRollback, name, fn)
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

	var manifest *backup.Manifest

	// Verify the backup before stopping anything. Discovering that the
	// backup is corrupt is survivable while the failed-but-running instance
	// is still up; discovering it after the data directory has been moved
	// aside is not.
	if err := step("verify-backup", func() (checkpoint.StepOutcome, error) {
		if plan.Method != "filesystem" {
			art, found := rs.Artifacts[ArtifactSQLDump]
			if !found {
				return checkpoint.StepOutcome{Verdict: string(StatusSkipped), Detail: fmt.Sprintf("dump at %s was supplied by hand, with no recorded checksum to check it against", plan.SQLDumpPath)}, nil
			}
			sum, size, err := hashFile(plan.SQLDumpPath)
			if err != nil {
				return checkpoint.StepOutcome{}, err
			}
			if sum != art.SHA256 {
				return checkpoint.StepOutcome{}, fmt.Errorf(
					"the dump at %s has sha256 %s but the checkpoint recorded %s when it was taken - refusing to restore a dump that changed since the backup",
					plan.SQLDumpPath, sum, art.SHA256)
			}
			return checkpoint.StepOutcome{Detail: fmt.Sprintf("dump at %s (%d bytes) still matches the checksum recorded at backup time", plan.SQLDumpPath, size)}, nil
		}
		m, err := backup.ReadManifest(plan.ManifestPath)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		if err := backup.VerifyDataDirBackup(plan.BackupDir, m); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		return checkpoint.StepOutcome{
			Detail: fmt.Sprintf("re-hashed %d file(s) in %s, all still match the manifest recorded at backup time", len(m.Files), plan.BackupDir),
			Extra:  plan.ManifestPath,
		}, nil
	}); err != nil {
		return report, err
	}

	// A resumed run skips the step above, so the manifest is loaded here
	// rather than inside it - the restore below needs it either way.
	if plan.Method == "filesystem" {
		manifest, err = backup.ReadManifest(plan.ManifestPath)
		if err != nil {
			report.Results = append(report.Results, CheckResult{Name: "restore-data", Status: StatusFail, Detail: err.Error()})
			return report, err
		}
	}

	stopTimeout := opts.StopTimeout
	if stopTimeout <= 0 {
		stopTimeout = 60 * time.Second
	}
	if err := step("stop-service", func() (checkpoint.StepOutcome, error) {
		if err := controller.Stop(ctx); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		if err := service.WaitFor(ctx, controller, false, stopTimeout); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		return checkpoint.StepOutcome{Detail: fmt.Sprintf("%s is stopped", controller.Target())}, nil
	}); err != nil {
		return report, err
	}

	if err := step("preserve-failed-state", func() (checkpoint.StepOutcome, error) {
		if plan.Method != "filesystem" {
			return checkpoint.StepOutcome{
				Verdict: string(StatusSkipped),
				Detail: "an external SQL store's current contents can't be moved aside the way a data directory can - the restore below " +
					"overwrites those tables in place. Take your own dump first if the failed attempt's state matters",
			}, nil
		}
		preserved, moved, err := PreserveFailedState(plan.DataDir, rs.RunID)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		if !moved {
			return checkpoint.StepOutcome{Detail: fmt.Sprintf("nothing to move aside (%s was already preserved, or %s doesn't exist)", preserved, plan.DataDir), Extra: preserved}, nil
		}
		return checkpoint.StepOutcome{Detail: fmt.Sprintf("moved the failed attempt's data directory to %s - it is not deleted", preserved), Extra: preserved}, nil
	}); err != nil {
		return report, err
	}

	if err := step("restore-data", func() (checkpoint.StepOutcome, error) {
		if plan.Method != "filesystem" {
			sqlOpts := opts.SQL
			sqlOpts.OutPath = plan.SQLDumpPath
			restore := RunPsqlRestore
			if plan.Method == "mysql" {
				restore = RunMySQLRestore
			}
			if err := restore(ctx, sqlOpts); err != nil {
				return checkpoint.StepOutcome{}, err
			}
			return checkpoint.StepOutcome{Detail: fmt.Sprintf("replayed %s into database %s", plan.SQLDumpPath, sqlOpts.Database)}, nil
		}
		if err := RestoreDataDir(plan.BackupDir, plan.DataDir, manifest); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		return checkpoint.StepOutcome{
			Detail: fmt.Sprintf("restored %d file(s) to %s and re-verified every one against the backup manifest", len(manifest.Files), plan.DataDir),
		}, nil
	}); err != nil {
		return report, err
	}

	if err := step("restore-binary", func() (checkpoint.StepOutcome, error) {
		if plan.PreservedBinary == "" {
			return checkpoint.StepOutcome{
				Verdict: string(StatusSkipped),
				Detail:  "this run never preserved an old binary (a dry run, or one that failed before the backup phase) - nothing to reinstall",
			}, nil
		}
		art := rs.Artifacts[ArtifactOldBinary]
		displaced, err := RestoreBinary(plan.PreservedBinary, plan.BinaryPath, art.SHA256, rs.RunID)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		detail := fmt.Sprintf("reinstalled the %s binary at %s", rs.SourceVersion, plan.BinaryPath)
		if displaced != "" {
			detail += fmt.Sprintf("; the binary that was there is preserved at %s for a retry", displaced)
		}
		return checkpoint.StepOutcome{Detail: detail, Extra: displaced}, nil
	}); err != nil {
		return report, err
	}

	if err := step("restore-service-config", func() (checkpoint.StepOutcome, error) {
		if plan.ServiceUnitSource == "" {
			return checkpoint.StepOutcome{
				Verdict: string(StatusSkipped),
				Detail: fmt.Sprintf("no %q artifact recorded for this run - nothing rewrote the service definition, so there's nothing to put back "+
					"(cutover, once it exists, is what will record one)", ArtifactServiceUnit),
			}, nil
		}
		if err := RestoreFile(plan.ServiceUnitSource, plan.ServiceUnitDest); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		if err := controller.ReloadConfig(ctx); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		return checkpoint.StepOutcome{Detail: fmt.Sprintf("restored %s from %s and reloaded the service definition", plan.ServiceUnitDest, plan.ServiceUnitSource)}, nil
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
		return checkpoint.StepOutcome{Detail: fmt.Sprintf("%s is running again", controller.Target())}, nil
	}); err != nil {
		return report, err
	}

	// The verification results are reported individually rather than
	// collapsed into the single step outcome, so an operator sees which
	// check failed, not just that one did.
	var verifyResults []CheckResult
	if err := step("verify-rollback", func() (checkpoint.StepOutcome, error) {
		results, err := Verify(ctx, VerifyOptions{
			BinaryPath: plan.BinaryPath, ExpectVersion: rs.SourceVersion,
			AdminURL: opts.AdminURL, AdminUser: opts.AdminUser, AdminPassword: opts.AdminPassword,
			HTTPClient: opts.HTTPClient, Snapshot: rs.PreflightSnapshot, Timeout: opts.VerifyTimeout,
		})
		verifyResults = results
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		return checkpoint.StepOutcome{Detail: summarize(results)}, nil
	}); err != nil {
		report.Results = append(report.Results, verifyResults...)
		return report, err
	}
	report.Results = append(report.Results, verifyResults...)

	return report, nil
}

func summarize(results []CheckResult) string {
	parts := make([]string, 0, len(results))
	for _, r := range results {
		parts = append(parts, fmt.Sprintf("%s=%s", r.Name, r.Status))
	}
	return strings.Join(parts, " ")
}
