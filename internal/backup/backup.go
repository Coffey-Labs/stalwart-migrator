package backup

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
)

// Options configures a full backup pass. Which fields matter depends on
// rs.Topology.StoreBackend, as recorded by the preflight phase - Run
// branches on that rather than requiring the caller to pre-select a code
// path.
type Options struct {
	// Binary preservation.
	BinaryPath string
	// SkipBinaryPreservation, when true, never touches BinaryPath - used by
	// a dry run, which must not move the production binary aside just to
	// simulate a migration it hasn't committed to.
	SkipBinaryPreservation bool

	// Embedded backend (RocksDB/SQLite).
	DataDir   string
	BackupDir string // destination for the fs snapshot; required if the backend is embedded

	// External SQL backend (PostgreSQL/MySQL).
	SQL SQLOptions

	// FoundationDB backend.
	FDB FDBOptions

	// Settings/principals dump (always runs - every topology needs it).
	MigrationScriptURL    string // defaults to DefaultMigrationScriptURL
	MigrationScriptSHA256 string // pinned hash; empty accepts and reports whatever is fetched (see DownloadFile)
	ScriptDestPath        string
	AdminURL              string
	AdminUser             string
	AdminPassword         string
	SettingsDumpPath      string
	PrincipalsDumpPath    string
	PythonPath            string
	HTTPClient            *http.Client

	// Per-account content export (Vandelay) - optional defense-in-depth
	// layer. Empty Accounts skips it entirely; this is expected until
	// account enumeration is wired up (see stalwartapi.Client.AccountSnapshot).
	Vandelay VandelayOptions
	Accounts []string
}

// Run executes the full backup pass described in ARCHITECTURE.md §4.2,
// checkpointing each step. Unlike preflight, most steps here are hard
// failures: a backup that didn't actually happen must stop the pipeline,
// not just get reported and continued past. The one genuinely optional
// layer is the per-account Vandelay export, which is skipped (Status:
// StatusSkipped, not a failure) when Options.Accounts is empty - but if the
// operator populated Accounts, a failure there is hard too, since silently
// downgrading an explicitly requested backup layer would be worse than
// stopping.
func Run(ctx context.Context, store *checkpoint.Store, rs *checkpoint.RunState, opts Options) (Report, error) {
	var report Report

	step := func(name string, fn func() (checkpoint.StepOutcome, error)) (checkpoint.StepOutcome, error) {
		outcome, err := store.RunStep(rs, checkpoint.PhaseBackup, name, fn)
		if err != nil {
			report.Results = append(report.Results, CheckResult{Name: name, Status: StatusFail, Detail: err.Error()})
			return outcome, err
		}
		status := Status(outcome.Verdict)
		if status == "" {
			status = StatusOK
		}
		report.Results = append(report.Results, CheckResult{Name: name, Status: status, Detail: outcome.Detail})
		return outcome, nil
	}

	if opts.SkipBinaryPreservation {
		report.Results = append(report.Results, CheckResult{
			Name: "preserve-binary", Status: StatusSkipped,
			Detail: "skipped (SkipBinaryPreservation) - the production binary is never touched, e.g. for a dry run",
		})
	} else if _, err := step("preserve-binary", func() (checkpoint.StepOutcome, error) {
		preserved, err := PreserveBinary(opts.BinaryPath, rs.SourceVersion)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		sum, size, err := hashFile(preserved)
		if err != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf("backup: hash preserved binary %s: %w", preserved, err)
		}
		rs.RecordArtifact("old-binary", checkpoint.Artifact{Path: preserved, SHA256: sum, SizeBytes: size})
		return checkpoint.StepOutcome{Detail: fmt.Sprintf("preserved %s as %s", opts.BinaryPath, preserved), Extra: preserved}, nil
	}); err != nil {
		return report, err
	}

	backends := strings.ToLower(rs.Topology.StoreBackend)

	switch {
	case strings.Contains(backends, "rocksdb") || strings.Contains(backends, "sqlite"):
		if _, err := step("fs-snapshot", func() (checkpoint.StepOutcome, error) {
			manifest, err := CopyDataDir(opts.DataDir, opts.BackupDir)
			if err != nil {
				return checkpoint.StepOutcome{}, err
			}
			manifestPath := filepath.Join(opts.BackupDir, "..", filepath.Base(opts.BackupDir)+".manifest.json")
			if err := WriteManifest(manifestPath, manifest); err != nil {
				return checkpoint.StepOutcome{}, err
			}
			sum, err := manifest.Checksum()
			if err != nil {
				return checkpoint.StepOutcome{}, err
			}
			rs.RecordArtifact("fs-backup", checkpoint.Artifact{Path: opts.BackupDir, SHA256: sum, SizeBytes: manifest.TotalBytes})
			return checkpoint.StepOutcome{
				Detail: fmt.Sprintf("copied %d file(s), %d bytes, from %s to %s", len(manifest.Files), manifest.TotalBytes, opts.DataDir, opts.BackupDir),
				Extra:  manifestPath,
			}, nil
		}); err != nil {
			return report, err
		}

		if _, err := step("fs-verify", func() (checkpoint.StepOutcome, error) {
			manifestOutcome := rs.Outcome(checkpoint.PhaseBackup, "fs-snapshot")
			manifest, err := ReadManifest(manifestOutcome.Extra)
			if err != nil {
				return checkpoint.StepOutcome{}, err
			}
			if err := VerifyDataDirBackup(opts.BackupDir, manifest); err != nil {
				return checkpoint.StepOutcome{}, err
			}
			return checkpoint.StepOutcome{Detail: fmt.Sprintf("re-hashed %d file(s), all match the manifest recorded at copy time", len(manifest.Files))}, nil
		}); err != nil {
			return report, err
		}

	case strings.Contains(backends, "postgresql"):
		if _, err := step("sql-dump", func() (checkpoint.StepOutcome, error) {
			if err := RunPgDump(ctx, opts.SQL); err != nil {
				return checkpoint.StepOutcome{}, err
			}
			sum, size, err := hashFile(opts.SQL.OutPath)
			if err != nil {
				return checkpoint.StepOutcome{}, err
			}
			rs.RecordArtifact("sql-dump", checkpoint.Artifact{Path: opts.SQL.OutPath, SHA256: sum, SizeBytes: size})
			return checkpoint.StepOutcome{Detail: fmt.Sprintf("pg_dump of critical tables (%s) to %s, %d bytes", strings.Join(criticalTables, " "), opts.SQL.OutPath, size)}, nil
		}); err != nil {
			return report, err
		}

	case strings.Contains(backends, "mysql"):
		if _, err := step("sql-dump", func() (checkpoint.StepOutcome, error) {
			if err := RunMySQLDump(ctx, opts.SQL); err != nil {
				return checkpoint.StepOutcome{}, err
			}
			sum, size, err := hashFile(opts.SQL.OutPath)
			if err != nil {
				return checkpoint.StepOutcome{}, err
			}
			rs.RecordArtifact("sql-dump", checkpoint.Artifact{Path: opts.SQL.OutPath, SHA256: sum, SizeBytes: size})
			return checkpoint.StepOutcome{Detail: fmt.Sprintf("mysqldump of critical tables (%s) to %s, %d bytes", strings.Join(criticalTables, " "), opts.SQL.OutPath, size)}, nil
		}); err != nil {
			return report, err
		}

	case strings.Contains(backends, "foundationdb"):
		if _, err := step("fdb-backup", func() (checkpoint.StepOutcome, error) {
			if err := StartFDBBackup(ctx, opts.FDB); err != nil {
				return checkpoint.StepOutcome{}, err
			}
			return checkpoint.StepOutcome{
				Detail: fmt.Sprintf("fdbbackup start issued for destination %s (tag %s) - this only confirms the job was accepted, not that it finished; check `fdbbackup status` before relying on it", opts.FDB.Destination, opts.FDB.Tag),
			}, nil
		}); err != nil {
			return report, err
		}

	default:
		report.Results = append(report.Results, CheckResult{
			Name: "backend-backup", Status: StatusSkipped,
			Detail: fmt.Sprintf("no known store backend recorded for this run (topology.store_backend=%q) - preflight must run first, or the backend wasn't recognized; no filesystem/DB backup was taken", rs.Topology.StoreBackend),
		})
	}

	if _, err := step("settings-dump", func() (checkpoint.StepOutcome, error) {
		scriptURL := opts.MigrationScriptURL
		if scriptURL == "" {
			scriptURL = DefaultMigrationScriptURL
		}
		sum, err := DownloadFile(ctx, opts.HTTPClient, scriptURL, opts.ScriptDestPath, opts.MigrationScriptSHA256)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		pinNote := ""
		if opts.MigrationScriptSHA256 == "" {
			pinNote = fmt.Sprintf(" (no pin was configured - record sha256 %s as MigrationScriptSHA256 to pin it for future runs)", sum)
		}
		if err := RunSettingsDump(ctx, SettingsDumpOptions{
			PythonPath:     opts.PythonPath,
			ScriptPath:     opts.ScriptDestPath,
			URL:            opts.AdminURL,
			Username:       opts.AdminUser,
			Password:       opts.AdminPassword,
			SettingsPath:   opts.SettingsDumpPath,
			PrincipalsPath: opts.PrincipalsDumpPath,
		}); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		settingsSum, settingsSize, err := hashFile(opts.SettingsDumpPath)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		principalsSum, principalsSize, err := hashFile(opts.PrincipalsDumpPath)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		rs.RecordArtifact("settings-dump", checkpoint.Artifact{Path: opts.SettingsDumpPath, SHA256: settingsSum, SizeBytes: settingsSize})
		rs.RecordArtifact("principals-dump", checkpoint.Artifact{Path: opts.PrincipalsDumpPath, SHA256: principalsSum, SizeBytes: principalsSize})
		return checkpoint.StepOutcome{
			Detail: fmt.Sprintf("dumped settings (%d bytes) and principals (%d bytes) from %s%s", settingsSize, principalsSize, opts.AdminURL, pinNote),
			Extra:  sum,
		}, nil
	}); err != nil {
		return report, err
	}

	if len(opts.Accounts) == 0 {
		report.Results = append(report.Results, CheckResult{
			Name: "vandelay-export", Status: StatusSkipped,
			Detail: "no account list supplied - skipped; pass Options.Accounts (or --full-content-backup with an account source once account enumeration is wired up) to enable this belt-and-suspenders layer",
		})
	} else if _, err := step("vandelay-export", func() (checkpoint.StepOutcome, error) {
		if err := os.MkdirAll(opts.Vandelay.OutDir, 0o750); err != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf("backup: create vandelay output dir %s: %w", opts.Vandelay.OutDir, err)
		}
		files, err := ExportAccounts(ctx, opts.Vandelay, opts.Accounts)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		var totalSize int64
		for i, f := range files {
			sum, size, err := hashFile(f)
			if err != nil {
				return checkpoint.StepOutcome{}, err
			}
			rs.RecordArtifact(fmt.Sprintf("vandelay-%s", opts.Accounts[i]), checkpoint.Artifact{Path: f, SHA256: sum, SizeBytes: size})
			totalSize += size
		}
		return checkpoint.StepOutcome{Detail: fmt.Sprintf("exported %d account(s), %d bytes total, to %s", len(files), totalSize, opts.Vandelay.OutDir)}, nil
	}); err != nil {
		return report, err
	}

	return report, nil
}
