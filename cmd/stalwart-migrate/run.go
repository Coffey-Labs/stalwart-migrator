// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/backup"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/plan"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/preflight"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/recovery"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/validate"
)

// runRun implements `stalwart-migrate run`. Only --dry-run is available
// today. internal/cutover exists, so the gap is no longer a phase but the
// pipeline around it: §4.3 staging, and the wiring that would drive
// preflight -> backup -> stage -> recovery-mode -> cutover -> validate
// against real paths rather than a sandbox. See ARCHITECTURE.md §8. `run`
// without --dry-run refuses rather than doing a migration partway.
//
// --dry-run runs preflight and a real backup (see the caveat printed below
// about why backup still touches the live data directory), then - if the
// plan crosses the 0.15/0.16 boundary - clones the verified backup into a
// disposable sandbox, converts the settings snapshot to point at that
// sandbox (via migrate_v016.py's own documented --patch-paths mechanism,
// not by this tool guessing at config.json's schema), runs the real
// recovery-mode migration against the sandbox, and boots the result
// normally to confirm it comes up. Nothing at the real binary path or the
// real service is ever touched.
//
// Every byte a dry run writes - the fs-backup copy, the settings/principals
// dumps, the downloaded migrate_v016.py, the sandbox clone and its
// config/export files - lives under one per-run directory
// (work-dir/<run-id>) that a deferred cleanup at the bottom of this
// function removes on every exit path: success, a failed check partway
// through, or an early refusal. The only thing left behind afterward is the
// checkpoint's state.json under --state-dir, which is exactly the
// success/failure log a rerun's `status <run-id>` reads - not bulk data.
// --keep-artifacts opts out, for when a failure needs inspecting.
func runRun(args []string) (err error) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	binaryPath := fs.String("binary", "/usr/local/bin/stalwart", "path to the currently-installed stalwart binary")
	targetBinaryPath := fs.String("target-binary", "", "path to an already-downloaded target-version stalwart binary (required to simulate a major-boundary migration)")
	configPath := fs.String("config", "/etc/stalwart/config.toml", "path to stalwart's current config file")
	dataDir := fs.String("data-dir", "/var/lib/stalwart", "stalwart data directory")
	containerName := fs.String("container", "stalwart", "docker container name, if applicable")
	adminURL := fs.String("admin-url", "", "base URL for the live instance's admin/JMAP API (required)")
	adminUser := fs.String("admin-user", "", "admin username")
	adminPassword := fs.String("admin-password", os.Getenv("STALWART_MIGRATE_ADMIN_PASSWORD"),
		"admin password (or set STALWART_MIGRATE_ADMIN_PASSWORD)")
	targetVersion := fs.String("target", "latest", `target Stalwart version, or "latest"`)
	stateDir := fs.String("state-dir", checkpoint.DefaultBaseDir, "directory to store run checkpoints in")
	workDir := fs.String("work-dir", "/var/lib/stalwart-migrator/work", "scratch directory for backups, dumps, and the dry-run sandbox (cleaned up afterward - see --keep-artifacts)")
	stalwartCLI := fs.String("stalwart-cli", "stalwart-cli", "path to the stalwart-cli binary")
	pythonPath := fs.String("python", "python3", "path to python3")
	migrationScriptSHA256 := fs.String("migration-script-sha256", "", "pinned sha256 of migrate_v016.py (recommended; see preflight/backup output for the hash to pin after a first unpinned run)")
	recoveryPort := fs.Int("recovery-port", 8080, "port recovery mode's HTTP listener binds, per UPGRADING/v0_16.md's own examples")
	minFree := fs.Float64("min-free-multiple", 2.0, "required free disk space as a multiple of the data directory size")
	dryRun := fs.Bool("dry-run", false, "simulate and validate the migration against a disposable sandbox, without touching production")
	keepArtifacts := fs.Bool("keep-artifacts", false, "don't delete work-dir/<run-id> afterward (the fs-backup copy, dumps, and sandbox) - useful for inspecting a failure")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !*dryRun {
		return fmt.Errorf(
			"real (non-dry-run) migrations aren't available yet: cutover is implemented (ARCHITECTURE.md §4.5), but nothing wires it " +
				"into a production run - the staging phase (§4.3) and the real pipeline don't exist yet, so this command has no path " +
				"that touches production. Run with --dry-run to validate the migration mechanics against a disposable sandbox copy " +
				"of your data. Note that when a real run does land, recovery from a failed migration will be your own snapshot or " +
				"backup - this tool does not undo a migration (§4.8)",
		)
	}
	if *adminURL == "" {
		return fmt.Errorf("--admin-url is required")
	}

	ctx := context.Background()
	httpClient := &http.Client{}

	if err := os.MkdirAll(*workDir, 0o750); err != nil {
		return fmt.Errorf("create work dir %s: %w", *workDir, err)
	}

	store := checkpoint.NewStore(*stateDir)
	rs, err := store.Create("", *targetVersion)
	if err != nil {
		return fmt.Errorf("create run: %w", err)
	}
	fmt.Printf("run id: %s\n\n", rs.RunID)

	runWorkDir := filepath.Join(*workDir, rs.RunID)
	logPath := filepath.Join(*stateDir, rs.RunID, "state.json")
	defer func() {
		if _, statErr := os.Stat(runWorkDir); os.IsNotExist(statErr) {
			return // nothing was ever written (e.g. refused before backup ran)
		}
		if *keepArtifacts {
			fmt.Printf("\nartifacts kept at %s (--keep-artifacts) - remove manually when done inspecting\n", runWorkDir)
			return
		}
		outcome := "succeeded"
		if err != nil {
			outcome = "failed"
		}
		if rmErr := os.RemoveAll(runWorkDir); rmErr != nil {
			fmt.Fprintf(os.Stderr, "\nwarning: dry run %s, but failed to clean up %s: %v (remove it manually)\n", outcome, runWorkDir, rmErr)
			return
		}
		fmt.Printf("\ndry run %s - cleaned up %s; the run log is at %s\n", outcome, runWorkDir, logPath)
	}()

	fmt.Println("--- preflight ---")
	checker := preflight.New(preflight.Options{
		BinaryPath: *binaryPath, ConfigPath: *configPath, DataDir: *dataDir, ContainerName: *containerName,
		AdminURL: *adminURL, AdminUser: *adminUser, AdminPassword: *adminPassword,
		TargetVersion: *targetVersion, MinFreeMultiple: *minFree, HTTPClient: httpClient,
	})
	pfReport, err := checker.Run(ctx, store, rs)
	fmt.Print(pfReport.String())
	if err != nil {
		return fmt.Errorf("preflight failed to complete: %w", err)
	}
	if pfReport.Blocking() {
		return fmt.Errorf("preflight found blocking issues - see FAIL lines above")
	}

	p, err := plan.Decide(rs.SourceVersion, rs.TargetVersion)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	fmt.Printf("\nplan: %s\n", p.Reason)

	fmt.Println("\n--- backup ---")
	fmt.Println("(dry-run does not stop the live Stalwart service itself - internal/service can now do that, but dry-run " +
		"isn't wired to offer it. For a guaranteed-consistent snapshot, stop stalwart before running this; otherwise the " +
		"filesystem copy may reflect a live, in-use store. This is unrelated to whether production gets touched - it never does.)")

	backupDir := filepath.Join(runWorkDir, "backup")
	scriptDest := filepath.Join(runWorkDir, "migrate_v016.py")
	settingsPath := filepath.Join(runWorkDir, "settings.json")
	principalsPath := filepath.Join(runWorkDir, "principals.json")

	backupOpts := backup.Options{
		BinaryPath:             *binaryPath,
		SkipBinaryPreservation: true, // dry-run: never touch the production binary
		DataDir:                *dataDir,
		BackupDir:              backupDir,
		MigrationScriptSHA256:  *migrationScriptSHA256,
		ScriptDestPath:         scriptDest,
		AdminURL:               *adminURL,
		AdminUser:              *adminUser,
		AdminPassword:          *adminPassword,
		SettingsDumpPath:       settingsPath,
		PrincipalsDumpPath:     principalsPath,
		PythonPath:             *pythonPath,
		HTTPClient:             httpClient,
	}
	bkReport, err := backup.Run(ctx, store, rs, backupOpts)
	fmt.Print(bkReport.String())
	if err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}

	if !p.CrossesMajorBoundary {
		fmt.Println("\nthis is a same-boundary patch upgrade: there's no recovery-mode phase to simulate. " +
			"preflight and backup above are as far as a dry-run goes for this path - a real run would be a binary swap and restart.")
		return nil
	}

	if *targetBinaryPath == "" {
		return fmt.Errorf("--target-binary is required to simulate a major-boundary migration (0.15 -> 0.16 crosses one here)")
	}

	fmt.Println("\n--- convert (settings -> sandbox config) ---")
	sandboxDataDir := filepath.Join(runWorkDir, "sandbox-data")
	sandboxConfigPath := filepath.Join(runWorkDir, "sandbox-config.json")
	sandboxExportPath := filepath.Join(runWorkDir, "sandbox-export.json")

	if _, err := store.RunStep(rs, checkpoint.PhaseStage, "clone-sandbox-data", func() (checkpoint.StepOutcome, error) {
		manifest, err := backup.CopyDataDir(backupDir, sandboxDataDir)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		return checkpoint.StepOutcome{Detail: fmt.Sprintf("cloned the verified backup (%d files) into the sandbox at %s", len(manifest.Files), sandboxDataDir)}, nil
	}); err != nil {
		return fmt.Errorf("clone sandbox data: %w", err)
	}
	fmt.Printf("cloned verified backup into sandbox: %s\n", sandboxDataDir)

	unmigratedPath := filepath.Join(runWorkDir, "unmigrated.txt")
	if _, err := store.RunStep(rs, checkpoint.PhaseStage, "convert-settings", func() (checkpoint.StepOutcome, error) {
		if err := backup.RunSettingsConvert(ctx, backup.SettingsConvertOptions{
			PythonPath: *pythonPath, ScriptPath: scriptDest,
			SettingsPath: settingsPath, PrincipalsPath: principalsPath,
			ConfigPath: sandboxConfigPath, OutputPath: sandboxExportPath,
			PatchPaths: map[string]string{*dataDir: sandboxDataDir},
			// Without this the script writes unmigrated.txt into whatever
			// directory this command was launched from - or fails outright
			// if that isn't writable.
			WorkDir: runWorkDir,
		}); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		detail := fmt.Sprintf("generated %s and %s, patched to point at the sandbox", sandboxConfigPath, sandboxExportPath)
		if report, err := backup.ReadUnmigratedReport(unmigratedPath); err == nil && report != nil && report.TotalKeys > 0 {
			detail += fmt.Sprintf("; %d setting(s) were NOT migrated", report.TotalKeys)
		}
		return checkpoint.StepOutcome{Detail: detail, Extra: unmigratedPath}, nil
	}); err != nil {
		return fmt.Errorf("convert settings: %w", err)
	}
	fmt.Println("generated sandbox config.json and export.json")

	// What the converter could NOT carry over matters more than what it
	// could: against a real instance this is the overwhelming majority of
	// the configuration, including the listeners, and an operator who
	// doesn't read it will bring up a server that answers on no ports.
	unmigrated, err := backup.ReadUnmigratedReport(unmigratedPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: couldn't read the unmigrated-settings report: %v\n", err)
	} else if unmigrated != nil && unmigrated.TotalKeys > 0 {
		if sum, size, hashErr := backup.HashFile(unmigratedPath); hashErr == nil {
			rs.RecordArtifact("unmigrated-settings", checkpoint.Artifact{Path: unmigratedPath, SHA256: sum, SizeBytes: size})
			if saveErr := store.Save(rs); saveErr != nil {
				fmt.Fprintf(os.Stderr, "warning: couldn't record the unmigrated-settings artifact: %v\n", saveErr)
			}
		}
		fmt.Printf("\n  !! %s\n", unmigrated.Summary(10))
		fmt.Println("     These do not carry over. Recreate them on the migrated instance before it serves mail.")
	}

	fmt.Println("\n--- recovery-mode migration (against the sandbox) ---")
	listenURL := fmt.Sprintf("http://127.0.0.1:%d/", *recoveryPort)
	recReport, err := recovery.Run(ctx, store, rs, recovery.Options{
		BinaryPath: *targetBinaryPath, ConfigPath: sandboxConfigPath, ListenURL: listenURL,
		AdminUser: "admin", ApplyFiles: []string{sandboxExportPath}, CLIBinaryPath: *stalwartCLI,
		HTTPClient: httpClient,
	})
	fmt.Print(recReport.String())
	if err != nil {
		return fmt.Errorf("recovery-mode migration against the sandbox failed: %w", err)
	}

	fmt.Println("\n--- boot check (normal boot of the migrated sandbox) ---")
	if rs.PreflightSnapshot != nil {
		fmt.Println("(comparing against the pre-migration account/mailbox snapshot preflight captured - " +
			"this is the actual no-data-loss check, not just a reachability probe)")
	} else {
		fmt.Println("(no pre-migration snapshot to compare against - preflight couldn't capture one, most likely " +
			"because --admin-url wasn't set; only reachability is checked)")
	}
	valReport, err := validate.Run(ctx, store, rs, validate.BootCheckOptions{
		BinaryPath: *targetBinaryPath, ConfigPath: sandboxConfigPath, ListenURL: listenURL, HTTPClient: httpClient,
		ContentIntegrityBefore: rs.PreflightSnapshot,
		AdminUser:              *adminUser,
		AdminPassword:          *adminPassword,
	})
	fmt.Print(valReport.String())
	if err != nil {
		return fmt.Errorf("post-migration validation failed: %w", err)
	}

	verified := "the migration mechanics succeeded"
	if rs.PreflightSnapshot != nil {
		verified = "the migration mechanics succeeded AND every account/mailbox message count matched before vs. after"
	}
	fmt.Printf("\nDRY RUN COMPLETE for run %s: %s, against a disposable sandbox copy of your data. Nothing in production was touched.\n", rs.RunID, verified)
	return nil
}
