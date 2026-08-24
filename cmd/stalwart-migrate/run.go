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
	"time"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/applyplan"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/backup"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/cutover"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/plan"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/preflight"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/recovery"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/service"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/stage"
)

// runRun implements `stalwart-migrate run`: the real migration.
//
// The phase order is ARCHITECTURE.md §4's, and it was arrived at by
// performing this migration by hand against a clone of a production
// instance before it was ever written down as code:
//
//	preflight -> stage -> dump -> preserve binary -> STOP -> convert ->
//	generate supplement -> recovery-mode migration -> cutover -> START
//
// The dump happens before the service stops, because it reads settings
// over the admin API and a stopped server has no admin API. Everything
// between the stop and the end of cutover is downtime, and on a real 3.6 GB
// store that stretch was seconds of work - the window is dominated by
// verification and by however long an operator takes to answer, not by
// data volume.
//
// Two gates, deliberately separate. --yes says "do it"; it is about intent.
// --recovery-point-confirmed says "I have a snapshot or backup I have
// verified I can restore"; it is a claim about the world, which this tool
// cannot check and must not assume. Nothing here can undo a migration -
// recovery is the operator's (§4.8) - so a run that proceeded without that
// claim would be proceeding on a hope.
func runRun(args []string) (err error) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	binaryPath := fs.String("binary", "/usr/local/bin/stalwart", "path to the currently-installed stalwart binary")
	configPath := fs.String("config", "/etc/stalwart/config.toml", "path to stalwart's current config file")
	dataDir := fs.String("data-dir", "/var/lib/stalwart", "stalwart data directory")
	newConfigPath := fs.String("new-config", "", "where the converted v0.16 config is installed (default: config.json beside --config)")
	unitName := fs.String("unit", "stalwart", "systemd unit name")
	serviceUnitPath := fs.String("service-unit", "/etc/systemd/system/stalwart.service", "systemd unit file to repoint at the new binary")
	containerName := fs.String("container", "stalwart", "docker container name, if applicable")
	adminURL := fs.String("admin-url", "", "base URL for the live instance's admin/JMAP API (required)")
	adminUser := fs.String("admin-user", "", "admin username - must be a directory account, not a config fallback-admin (see README)")
	adminPassword := fs.String("admin-password", os.Getenv("STALWART_MIGRATE_ADMIN_PASSWORD"),
		"admin password (or set STALWART_MIGRATE_ADMIN_PASSWORD)")
	targetVersion := fs.String("target", "latest", `target Stalwart version, or "latest"`)
	targetBinary := fs.String("target-binary", "", "use an already-downloaded target binary instead of fetching one")
	stateDir := fs.String("state-dir", checkpoint.DefaultBaseDir, "directory to store run checkpoints in")
	workDir := fs.String("work-dir", "/var/lib/stalwart-migrator/work", "scratch directory")
	pythonPath := fs.String("python", "python3", "path to python3")
	stalwartCLI := fs.String("stalwart-cli", "stalwart-cli", "path to stalwart-cli (v1.0.2 or later; a separate download from the server)")
	scriptSHA := fs.String("migration-script-sha256", "", "pinned sha256 of migrate_v016.py")
	binarySHA := fs.String("target-binary-sha256", "", "pinned sha256 of the target release archive")
	minFree := fs.Float64("min-free-multiple", 2.0, "required free disk space as a multiple of the data directory size")
	recalcQuotas := fs.Bool("recalculate-quotas", true, "schedule the post-migration quota rebuild")
	keepArtifacts := fs.Bool("keep-artifacts", false, "don't delete work-dir/<run-id> afterward")
	resume := fs.String("resume", "", "resume an interrupted run by id instead of starting a new one (see `status` for ids)")
	yes := fs.Bool("yes", false, "actually perform the migration")
	recoveryConfirmed := fs.Bool("recovery-point-confirmed", false,
		"confirm you have a snapshot or backup you have verified you can restore - this tool cannot undo a migration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *adminURL == "" {
		return fmt.Errorf("--admin-url is required")
	}
	if *newConfigPath == "" {
		*newConfigPath = filepath.Join(filepath.Dir(*configPath), "config.json")
	}

	ctx := context.Background()
	httpClient := &http.Client{}

	fmt.Println("This migrates a live mail server in place. It will:")
	fmt.Printf("  1. check %s, then fetch and verify the %s binary\n", *binaryPath, *targetVersion)
	fmt.Printf("  2. dump settings from %s while it is still running\n", *adminURL)
	fmt.Printf("  3. STOP the service - mail is down from here\n")
	fmt.Printf("  4. convert the settings and migrate the store at %s IN PLACE\n", *dataDir)
	fmt.Printf("  5. install the new binary at %s and repoint %s\n", *binaryPath, *serviceUnitPath)
	fmt.Printf("  6. start the service and check it answers\n")
	fmt.Println("\nThis tool cannot undo any of it. Recovery is your snapshot or backup.")

	if !*recoveryConfirmed {
		return fmt.Errorf("\nrefusing to start: pass --recovery-point-confirmed once you have a snapshot or backup you have " +
			"actually verified you can restore. This tool does not take one and cannot check yours")
	}
	if !*yes {
		fmt.Println("\nnothing has been touched. Re-run with --yes to perform this migration.")
		return nil
	}

	store := checkpoint.NewStore(*stateDir)
	var rs *checkpoint.RunState
	if *resume != "" {
		// Resuming is not a convenience. A run that fails partway leaves
		// the service stopped and the store part-migrated, and starting
		// over is often impossible: preflight would re-run against a
		// binary that has already been moved aside, and the settings dump
		// needs a live pre-migration instance that no longer exists.
		// Completed steps are skipped from the checkpoint, so this picks
		// up where it stopped.
		if rs, err = store.Load(*resume); err != nil {
			return fmt.Errorf("resume run %s: %w", *resume, err)
		}
		fmt.Printf("\nresuming run: %s (completed steps will be skipped)\n", rs.RunID)
	} else {
		if rs, err = store.Create("", *targetVersion); err != nil {
			return fmt.Errorf("create run: %w", err)
		}
		fmt.Printf("\nrun id: %s\n", rs.RunID)
	}
	runWorkDir := filepath.Join(*workDir, rs.RunID)
	runStateDir := filepath.Join(*stateDir, rs.RunID)
	if err := os.MkdirAll(runWorkDir, 0o750); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	defer func() {
		if *keepArtifacts {
			fmt.Printf("\nartifacts kept at %s (--keep-artifacts)\n", runWorkDir)
			return
		}
		if err != nil {
			// Never clean up after a failure. These files - the settings
			// dump, the converted config and export plan - are the run's
			// inputs, and after the service has been stopped they cannot
			// be regenerated: the dump needs a live pre-migration instance.
			// Deleting them once turned a missing-dependency error into a
			// restore-from-snapshot, because there was no way forward and
			// no way back.
			fmt.Fprintf(os.Stderr, "\nthe run failed; its artifacts are kept at %s\n", runWorkDir)
			fmt.Fprintf(os.Stderr, "resume it once the cause is fixed:\n  stalwart-migrate run --resume %s [same flags]\n", rs.RunID)
			return
		}
		if rmErr := os.RemoveAll(runWorkDir); rmErr != nil {
			fmt.Fprintf(os.Stderr, "warning: couldn't clean up %s: %v\n", runWorkDir, rmErr)
		}
	}()

	fmt.Println("\n--- preflight ---")
	pfReport, err := preflight.New(preflight.Options{
		BinaryPath: *binaryPath, ConfigPath: *configPath, DataDir: *dataDir, ContainerName: *containerName,
		AdminURL: *adminURL, AdminUser: *adminUser, AdminPassword: *adminPassword,
		TargetVersion: *targetVersion, MinFreeMultiple: *minFree, HTTPClient: httpClient,
		CLIPath: *stalwartCLI, PythonPath: *pythonPath,
	}).Run(ctx, store, rs)
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

	fmt.Println("\n--- stage ---")
	staged := *targetBinary
	if staged == "" {
		staged = filepath.Join(runWorkDir, "stalwart-"+rs.TargetVersion)
		if staged, err = stage.Run(ctx, store, rs, stage.Options{
			TargetVersion: *targetVersion, DestPath: staged, SHA256: *binarySHA, HTTPClient: httpClient,
		}); err != nil {
			return fmt.Errorf("stage: %w", err)
		}
	}
	fmt.Println(rs.Outcome(checkpoint.PhaseStage, "stage-binary").Detail)

	script := filepath.Join(runWorkDir, "migrate_v016.py")
	settingsPath := filepath.Join(runWorkDir, "settings.json")
	principalsPath := filepath.Join(runWorkDir, "principals.json")
	convertedConfig := filepath.Join(runWorkDir, "config.json")
	convertedExport := filepath.Join(runWorkDir, "export.json")
	unmigratedPath := filepath.Join(runWorkDir, "unmigrated.txt")
	supplementPath := filepath.Join(runWorkDir, "supplement.json")

	if p.CrossesMajorBoundary {
		fmt.Println("\n--- dump (service still up) ---")
		if _, err := store.RunStep(rs, checkpoint.PhaseBackup, "settings-dump", func() (checkpoint.StepOutcome, error) {
			if _, err := backup.DownloadFile(ctx, httpClient, backup.DefaultMigrationScriptURL, script, *scriptSHA); err != nil {
				return checkpoint.StepOutcome{}, err
			}
			if err := backup.RunSettingsDump(ctx, backup.SettingsDumpOptions{
				PythonPath: *pythonPath, ScriptPath: script, URL: *adminURL,
				Username: *adminUser, Password: *adminPassword,
				SettingsPath: settingsPath, PrincipalsPath: principalsPath,
			}); err != nil {
				return checkpoint.StepOutcome{}, err
			}
			return checkpoint.StepOutcome{Detail: "dumped settings and principals"}, nil
		}); err != nil {
			return fmt.Errorf("settings dump: %w", err)
		}
		fmt.Println("dumped settings and principals")
	}

	fmt.Println("\n--- preserve the old binary ---")
	if _, err := store.RunStep(rs, checkpoint.PhaseBackup, "preserve-binary", func() (checkpoint.StepOutcome, error) {
		preserved, err := backup.PreserveBinary(*binaryPath, rs.SourceVersion)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		sum, size, err := backup.HashFile(preserved)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		rs.RecordArtifact("old-binary", checkpoint.Artifact{Path: preserved, SHA256: sum, SizeBytes: size})
		return checkpoint.StepOutcome{Detail: "preserved " + preserved}, nil
	}); err != nil {
		return fmt.Errorf("preserve binary: %w", err)
	}
	fmt.Println(rs.Outcome(checkpoint.PhaseBackup, "preserve-binary").Detail)

	controller, err := service.New(service.Options{
		Kind: service.Kind(rs.Topology.DeploymentKind), UnitName: *unitName, ContainerName: *containerName,
	})
	if err != nil {
		return err
	}

	fmt.Println("\n--- stopping the service: MAIL IS DOWN FROM HERE ---")
	windowStart := time.Now()
	if _, err := store.RunStep(rs, checkpoint.PhaseCutover, "stop-service", func() (checkpoint.StepOutcome, error) {
		if err := controller.Stop(ctx); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		if err := service.WaitFor(ctx, controller, false, 2*time.Minute); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		return checkpoint.StepOutcome{Detail: controller.Target() + " is stopped"}, nil
	}); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	fmt.Println(controller.Target(), "stopped")

	if p.CrossesMajorBoundary {
		fmt.Println("\n--- convert ---")
		if _, err := store.RunStep(rs, checkpoint.PhaseStage, "convert-settings", func() (checkpoint.StepOutcome, error) {
			if err := backup.RunSettingsConvert(ctx, backup.SettingsConvertOptions{
				PythonPath: *pythonPath, ScriptPath: script,
				SettingsPath: settingsPath, PrincipalsPath: principalsPath,
				ConfigPath: convertedConfig, OutputPath: convertedExport, WorkDir: runWorkDir,
			}); err != nil {
				return checkpoint.StepOutcome{}, err
			}
			return checkpoint.StepOutcome{Detail: "converted settings into a v0.16 apply plan"}, nil
		}); err != nil {
			return fmt.Errorf("convert settings: %w", err)
		}
		unmigrated, readErr := backup.ReadUnmigratedReport(unmigratedPath)
		if readErr == nil && unmigrated != nil && unmigrated.TotalKeys > 0 {
			keptWorklist := filepath.Join(runStateDir, "unmigrated.txt")
			if copyErr := copyFile(unmigratedPath, keptWorklist); copyErr == nil {
				if sum, size, hashErr := backup.HashFile(keptWorklist); hashErr == nil {
					rs.RecordArtifact("unmigrated-settings", checkpoint.Artifact{Path: keptWorklist, SHA256: sum, SizeBytes: size})
				}
			}
			fmt.Println(unmigrated.Classify().Summary(keptWorklist))
		}

		fmt.Println("\n--- supplemental plan ---")
		applyFiles := []string{convertedExport}
		if err := buildSupplement(settingsPath, principalsPath, unmigratedPath, supplementPath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: couldn't generate the supplemental plan: %v\n", err)
		} else {
			applyFiles = append(applyFiles, supplementPath)
		}

		fmt.Println("\n--- recovery-mode migration (the store is migrated IN PLACE) ---")
		recReport, err := recovery.Run(ctx, store, rs, recovery.Options{
			BinaryPath: staged, ConfigPath: convertedConfig,
			ListenURL: "http://127.0.0.1:8080/", AdminUser: "admin",
			ApplyFiles: applyFiles, CLIBinaryPath: *stalwartCLI,
			StartupTimeout: 20 * time.Minute, HTTPClient: httpClient,
		})
		fmt.Print(recReport.String())
		if err != nil {
			return fmt.Errorf("recovery-mode migration failed - the store may be part-migrated and the service is still "+
				"stopped; restore your recovery point rather than restarting the old version against it: %w", err)
		}
	}

	fmt.Println("\n--- cutover ---")
	configSource := convertedConfig
	if !p.CrossesMajorBoundary {
		configSource = "" // a patch bump keeps its existing config
	}
	cutReport, err := cutover.Run(ctx, store, rs, cutover.Options{
		StagedBinaryPath: staged, BinaryPath: *binaryPath,
		ServiceUnitPath: *serviceUnitPath, ConfigPath: *newConfigPath,
		ConfigSource: configSource, ConfigOwnerReference: *configPath,
		Deployment:             service.Options{Kind: service.Kind(rs.Topology.DeploymentKind), UnitName: *unitName, ContainerName: *containerName},
		RecoveryPointConfirmed: *recoveryConfirmed,
		AdminURL:               *adminURL, AdminUser: *adminUser, AdminPassword: *adminPassword,
		HTTPClient: httpClient, RecalculateQuotas: *recalcQuotas && p.CrossesMajorBoundary,
		StartTimeout: 3 * time.Minute, HealthTimeout: 5 * time.Minute, QuotaTimeout: 30 * time.Minute,
	})
	fmt.Print(cutReport.String())
	if err != nil {
		return fmt.Errorf("cutover failed: %w", err)
	}

	fmt.Printf("\nMIGRATION COMPLETE for run %s. Mail was down for %s.\n",
		rs.RunID, time.Since(windowStart).Round(time.Second))
	fmt.Printf("Now: confirm you can log in as %s, send and receive a test message, and work through\n", *adminUser)
	fmt.Printf("the settings that did not carry over: %s\n", filepath.Join(runStateDir, "unmigrated.txt"))
	return nil
}

// buildSupplement generates the apply plan for what migrate_v016.py leaves
// behind - listeners, without which the migrated server binds nothing, and
// administrator roles, without which nobody can administer it.
func buildSupplement(settingsPath, principalsPath, unmigratedPath, outPath string) error {
	settings, err := backup.ReadSettingsDump(settingsPath)
	if err != nil {
		return err
	}
	unmigrated, err := backup.ReadUnmigratedKeys(unmigratedPath, settings)
	if err != nil {
		return err
	}
	p, coverage, err := applyplan.Build(settings, unmigrated, applyplan.DefaultGenerators())
	if err != nil {
		return err
	}
	principals, err := backup.ReadPrincipalsDump(principalsPath)
	if err != nil {
		return err
	}
	roleOps, _, roleWarnings, err := applyplan.AccountRoleOperations(principals)
	if err != nil {
		return err
	}
	p.Operations = append(p.Operations, roleOps...)
	if len(p.Operations) == 0 {
		return fmt.Errorf("nothing to generate")
	}
	if err := p.WriteNDJSON(outPath); err != nil {
		return err
	}
	fmt.Println(coverage.Summary(4))
	fmt.Printf("  + %d account-role operation(s)\n", len(roleOps))
	for _, w := range append(coverage.Warnings, roleWarnings...) {
		fmt.Printf("  warning: %s\n", w)
	}
	return nil
}
