// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/applyplan"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/backup"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/plan"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/preflight"
)

// runRehearse implements `stalwart-migrate rehearse` (ARCHITECTURE.md §4.9):
// preflight, dump this instance's settings and principals, convert them with
// migrate_v016.py, and report both halves of the result - what will carry
// over, and what will not.
//
// It copies no data, clones nothing, starts no server, and never writes to
// the store, so it is safe to run against production repeatedly and without
// a maintenance window. That is a deliberate narrowing from the sandbox-
// cloning dry run this replaces: cloning the store proved only that the
// store migrates and opens, at the cost of copying the data twice and
// reading a live store to do it, while the half that found every real
// problem - an empty defaultHostname v0.16 rejects, passwords v0.16 refuses,
// and a 12,182-key reconstruction worklist - needs no copy at all.
//
// The worklist is the point. Measured against a real production instance,
// migrate_v016.py carried 219 of 12,401 settings; server.listener was not
// among them, so a migrated instance answers on no ports until an operator
// rebuilds them. Anything that reported such a migration as a success
// without saying so would be actively misleading.
func runRehearse(args []string) (err error) {
	fs := flag.NewFlagSet("rehearse", flag.ExitOnError)
	binaryPath := fs.String("binary", "/usr/local/bin/stalwart", "path to the currently-installed stalwart binary")
	configPath := fs.String("config", "/etc/stalwart/config.toml", "path to stalwart's current config file")
	dataDir := fs.String("data-dir", "/var/lib/stalwart", "stalwart data directory (read-only here; used by preflight's checks)")
	containerName := fs.String("container", "stalwart", "docker container name, if applicable")
	adminURL := fs.String("admin-url", "", "base URL for the live instance's admin/JMAP API (required)")
	adminUser := fs.String("admin-user", "", "admin username")
	adminPassword := fs.String("admin-password", os.Getenv("STALWART_MIGRATE_ADMIN_PASSWORD"),
		"admin password (or set STALWART_MIGRATE_ADMIN_PASSWORD)")
	targetVersion := fs.String("target", "latest", `target Stalwart version, or "latest"`)
	stateDir := fs.String("state-dir", checkpoint.DefaultBaseDir, "directory to store run checkpoints in")
	workDir := fs.String("work-dir", "/var/lib/stalwart-migrator/work", "scratch directory for the dumps and converted plan (cleaned up afterward - see --keep-artifacts)")
	pythonPath := fs.String("python", "python3", "path to python3")
	stalwartCLI := fs.String("stalwart-cli", "stalwart-cli",
		"path to stalwart-cli (v1.0.2 or later; a separate download from the server) - rehearsal doesn't invoke it, but checks it so `run` doesn't fail after stopping the service")
	migrationScriptSHA256 := fs.String("migration-script-sha256", "", "pinned sha256 of migrate_v016.py (recommended; the first run prints the hash to pin)")
	migrationScriptPath := fs.String("migration-script", "", "use a local copy of migrate_v016.py instead of fetching it (for a host with no route to the internet)")
	minFree := fs.Float64("min-free-multiple", 2.0, "free-space multiple preflight checks for; rehearsal itself copies nothing")
	keepArtifacts := fs.Bool("keep-artifacts", false, "don't delete work-dir/<run-id> afterward")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *adminURL == "" {
		return fmt.Errorf("--admin-url is required: rehearsal converts the settings this instance actually has, which means reading them from it")
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
	runStateDir := filepath.Join(*stateDir, rs.RunID)
	// The rehearsal's two conclusions survive cleanup: the worklist of what
	// won't carry over, and the plan of what will. Everything else in the
	// work directory is scratch. Recording an artifact that points into a
	// directory about to be deleted would leave the checkpoint referring to
	// files that aren't there.
	keptWorklist := filepath.Join(runStateDir, "unmigrated.txt")
	keptPlan := filepath.Join(runStateDir, "export.json")
	keptSupplement := filepath.Join(runStateDir, "supplement.json")
	defer func() {
		if _, statErr := os.Stat(runWorkDir); os.IsNotExist(statErr) {
			return
		}
		if *keepArtifacts {
			fmt.Printf("\nartifacts kept at %s (--keep-artifacts)\n", runWorkDir)
			return
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nthe rehearsal failed; its artifacts are kept at %s for inspection\n", runWorkDir)
			return
		}
		if rmErr := os.RemoveAll(runWorkDir); rmErr != nil {
			fmt.Fprintf(os.Stderr, "\nwarning: failed to clean up %s: %v (remove it manually)\n", runWorkDir, rmErr)
			return
		}
		fmt.Printf("\ncleaned up %s; the run log is at %s\n", runWorkDir, filepath.Join(runStateDir, "state.json"))
	}()

	fmt.Println("--- preflight ---")
	checker := preflight.New(preflight.Options{
		BinaryPath: *binaryPath, ConfigPath: *configPath, DataDir: *dataDir, ContainerName: *containerName,
		AdminURL: *adminURL, AdminUser: *adminUser, AdminPassword: *adminPassword,
		TargetVersion: *targetVersion, MinFreeMultiple: *minFree, HTTPClient: httpClient,
		CLIPath: *stalwartCLI, PythonPath: *pythonPath, ToolCheckAdvisory: true, DeploymentCheckAdvisory: true,
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
	if !p.CrossesMajorBoundary {
		fmt.Println("\nthis is a same-boundary patch upgrade: its settings don't need converting, " +
			"so there is nothing to rehearse. A real run would be a binary swap and restart.")
		return nil
	}

	scriptDest := filepath.Join(runWorkDir, "migrate_v016.py")
	settingsPath := filepath.Join(runWorkDir, "settings.json")
	principalsPath := filepath.Join(runWorkDir, "principals.json")
	convertedConfig := filepath.Join(runWorkDir, "config.json")
	convertedExport := filepath.Join(runWorkDir, "export.json")
	unmigratedPath := filepath.Join(runWorkDir, "unmigrated.txt")

	fmt.Println("\n--- dump (read-only) ---")
	if _, err := store.RunStep(rs, checkpoint.PhaseBackup, "settings-dump", func() (checkpoint.StepOutcome, error) {
		if err := os.MkdirAll(runWorkDir, 0o750); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		sum, err := backup.ProvideFile(ctx, httpClient, *migrationScriptPath, backup.DefaultMigrationScriptURL, scriptDest, *migrationScriptSHA256)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		pinNote := ""
		if *migrationScriptSHA256 == "" {
			pinNote = fmt.Sprintf(" (no pin configured - record sha256 %s as --migration-script-sha256 to pin it)", sum)
		}
		if err := backup.RunSettingsDump(ctx, backup.SettingsDumpOptions{
			PythonPath: *pythonPath, ScriptPath: scriptDest, URL: *adminURL,
			Username: *adminUser, Password: *adminPassword,
			SettingsPath: settingsPath, PrincipalsPath: principalsPath,
		}); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		_, settingsSize, err := backup.HashFile(settingsPath)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		_, principalsSize, err := backup.HashFile(principalsPath)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		return checkpoint.StepOutcome{
			Detail: fmt.Sprintf("dumped settings (%d bytes) and principals (%d bytes) from %s%s",
				settingsSize, principalsSize, *adminURL, pinNote),
		}, nil
	}); err != nil {
		return fmt.Errorf("settings dump: %w", err)
	}
	fmt.Println(rs.Outcome(checkpoint.PhaseBackup, "settings-dump").Detail)

	fmt.Println("\n--- convert ---")
	if _, err := store.RunStep(rs, checkpoint.PhaseStage, "convert-settings", func() (checkpoint.StepOutcome, error) {
		if err := backup.RunSettingsConvert(ctx, backup.SettingsConvertOptions{
			PythonPath: *pythonPath, ScriptPath: scriptDest,
			SettingsPath: settingsPath, PrincipalsPath: principalsPath,
			ConfigPath: convertedConfig, OutputPath: convertedExport,
			// migrate_v016.py writes unmigrated.txt into its working
			// directory; without this it lands wherever the operator
			// happened to be, or fails the convert if that isn't writable.
			WorkDir: runWorkDir,
		}); err != nil {
			return checkpoint.StepOutcome{}, err
		}
		// The same domain/tenant repair the real run performs, so a
		// rehearsal exercises the plan that would actually be applied -
		// and so an unrepresentable multi-tenant layout is reported here,
		// with the service still running, rather than during a cutover.
		tenantFix, err := applyplan.ReconcileDomainTenantsFile(convertedExport)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		detail := "converted this instance's settings into a v0.16 apply plan"
		if len(tenantFix.Adoptions) > 0 {
			detail += " - " + tenantFix.String()
		}
		if report, readErr := backup.ReadUnmigratedReport(unmigratedPath); readErr == nil && report != nil {
			detail += fmt.Sprintf("; %d setting(s) will NOT carry over", report.TotalKeys)
		}
		return checkpoint.StepOutcome{Detail: detail}, nil
	}); err != nil {
		return fmt.Errorf("convert settings: %w", err)
	}

	if err := copyFile(convertedExport, keptPlan); err != nil {
		fmt.Fprintf(os.Stderr, "warning: couldn't preserve the apply plan: %v\n", err)
	} else if sum, size, hashErr := backup.HashFile(keptPlan); hashErr == nil {
		rs.RecordArtifact("converted-export", checkpoint.Artifact{Path: keptPlan, SHA256: sum, SizeBytes: size})
		fmt.Printf("apply plan:  %s (%d bytes) - what WILL carry over\n", keptPlan, size)
	}

	unmigrated, err := backup.ReadUnmigratedReport(unmigratedPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: couldn't read the unmigrated-settings report: %v\n", err)
	} else if unmigrated != nil && unmigrated.TotalKeys > 0 {
		if err := copyFile(unmigratedPath, keptWorklist); err != nil {
			fmt.Fprintf(os.Stderr, "warning: couldn't preserve the worklist: %v\n", err)
		} else {
			unmigrated.Path = keptWorklist
			if sum, size, hashErr := backup.HashFile(keptWorklist); hashErr == nil {
				rs.RecordArtifact("unmigrated-settings", checkpoint.Artifact{Path: keptWorklist, SHA256: sum, SizeBytes: size})
			}
		}
		// Categorised rather than counted: the raw total is alarming and
		// misleading. Most of it either rebuilds itself or ships with
		// v0.16 - see backup.Disposition.
		fmt.Printf("\n  !! %s\n", unmigrated.Classify().Summary(keptWorklist))
	}

	// Generate what we can of the gap (ARCHITECTURE.md §4.3). This is
	// best-effort by design and reports its own coverage: the plan is
	// worth having precisely because it is honest about how much of the
	// worklist it does not touch.
	fmt.Println("\n--- supplemental plan (best-effort) ---")
	if err := generateSupplement(store, rs, settingsPath, principalsPath, unmigratedPath, keptSupplement); err != nil {
		fmt.Fprintf(os.Stderr, "warning: couldn't generate the supplemental plan: %v\n", err)
	}
	if err := store.Save(rs); err != nil {
		return fmt.Errorf("save run state: %w", err)
	}

	fmt.Printf("\nREHEARSAL COMPLETE for run %s. Nothing was modified: no data was copied, no server was started,\n"+
		"and the store was never written to.\n", rs.RunID)
	return nil
}

// copyFile duplicates src to dst, used to lift the worklist out of the
// scratch directory before it's cleaned up.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// generateSupplement builds the best-effort apply plan for settings
// migrate_v016.py left behind, writes it beside the run's other
// conclusions, and reports honestly how much of the gap it closed.
//
// It is deliberately applied *after* export.json rather than merged into
// it: the official conversion is the authority on everything it handles,
// and a generated plan that overlapped it could silently override a
// correct mapping with a guessed one.
func generateSupplement(store *checkpoint.Store, rs *checkpoint.RunState, settingsPath, principalsPath, unmigratedPath, outPath string) error {
	settings, err := backup.ReadSettingsDump(settingsPath)
	if err != nil {
		return err
	}
	unmigrated, err := backup.ReadUnmigratedKeys(unmigratedPath, settings)
	if err != nil {
		return err
	}

	plan, coverage, err := applyplan.Build(settings, unmigrated, applyplan.DefaultGenerators())
	if err != nil {
		return err
	}

	// Account roles don't live in the settings dump, so they come from the
	// principals dump rather than through a settings Generator. Without
	// this the migrated instance has no administrator: migrate_v016.py
	// gives every account the User role, whatever it had before.
	principals, err := backup.ReadPrincipalsDump(principalsPath)
	if err != nil {
		return err
	}
	roleOps, _, roleWarnings, err := applyplan.AccountRoleOperations(principals)
	if err != nil {
		return err
	}
	plan.Operations = append(plan.Operations, roleOps...)
	coverage.Warnings = append(coverage.Warnings, roleWarnings...)
	if len(roleOps) > 0 {
		coverage.ObjectsByType["Account role"] += len(roleOps)
	}
	if len(plan.Operations) == 0 {
		fmt.Println("nothing this tool can rebuild automatically yet - the whole worklist is manual")
		return nil
	}
	if err := plan.WriteNDJSON(outPath); err != nil {
		return err
	}
	if sum, size, hashErr := backup.HashFile(outPath); hashErr == nil {
		rs.RecordArtifact("supplemental-plan", checkpoint.Artifact{Path: outPath, SHA256: sum, SizeBytes: size})
	}

	fmt.Printf("%s\n", coverage.Summary(6))
	for _, w := range coverage.Warnings {
		fmt.Printf("      warning: %s\n", w)
	}
	fmt.Printf("\n  supplement:  %s\n", outPath)
	fmt.Println("  Review it, then apply it after export.json:")
	fmt.Printf("      stalwart-cli apply --file %s --url <migrated-instance>\n", outPath)
	fmt.Println("  It is generated, not authoritative - read it before you run it.")

	_, err = store.RunStep(rs, checkpoint.PhaseStage, "supplemental-plan", func() (checkpoint.StepOutcome, error) {
		return checkpoint.StepOutcome{
			Detail: fmt.Sprintf("generated %d operation(s) covering %d of %d unmigrated setting(s)",
				len(plan.Operations), coverage.CoveredKeys, coverage.TotalKeys),
		}, nil
	})
	return err
}
