// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Coffey-Labs/stalwart-migrator/internal/checkpoint"
	"github.com/Coffey-Labs/stalwart-migrator/internal/preflight"
)

func runPreflight(args []string) error {
	fs := flag.NewFlagSet("preflight", flag.ExitOnError)
	binaryPath := fs.String("binary", "/usr/local/bin/stalwart", "path to the installed stalwart binary")
	configPath := fs.String("config", "/etc/stalwart/config.toml", "path to stalwart's config file")
	dataDir := fs.String("data-dir", "/var/lib/stalwart", "stalwart data directory")
	containerName := fs.String("container", "stalwart", "docker container name, if applicable")
	adminURL := fs.String("admin-url", "", "base URL for a JMAP reachability check (optional but recommended)")
	adminUser := fs.String("admin-user", "", "admin username for the reachability check")
	adminPassword := fs.String("admin-password", os.Getenv("STALWART_MIGRATE_ADMIN_PASSWORD"),
		"admin password for the reachability check (or set STALWART_MIGRATE_ADMIN_PASSWORD)")
	targetVersion := fs.String("target", "latest", `target Stalwart version, or "latest"`)
	targetBinary := fs.String("target-binary", "", "read the target version from this already-downloaded binary instead of the release API (for a host with no route to the internet)")
	minFree := fs.Float64("min-free-multiple", 2.0, "required free disk space as a multiple of the data directory size")
	stateDir := fs.String("state-dir", checkpoint.DefaultBaseDir, "directory to store run checkpoints in")
	pythonPath := fs.String("python", "python3", "path to python3, needed by migrate_v016.py")
	stalwartCLI := fs.String("stalwart-cli", "stalwart-cli", "path to stalwart-cli (v1.0.2 or later; a separate download from the server)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store := checkpoint.NewStore(*stateDir)
	rs, err := store.Create("", *targetVersion)
	if err != nil {
		return fmt.Errorf("create run: %w", err)
	}

	checker := preflight.New(preflight.Options{
		BinaryPath:       *binaryPath,
		ConfigPath:       *configPath,
		DataDir:          *dataDir,
		ContainerName:    *containerName,
		AdminURL:         *adminURL,
		AdminUser:        *adminUser,
		AdminPassword:    *adminPassword,
		TargetVersion:    *targetVersion,
		TargetBinaryPath: *targetBinary,
		MinFreeMultiple:  *minFree,
		CLIPath:          *stalwartCLI,
		PythonPath:       *pythonPath,
	})

	report, err := checker.Run(context.Background(), store, rs)
	fmt.Print(report.String())
	fmt.Printf("\nrun id: %s\n", rs.RunID)
	if err != nil {
		return fmt.Errorf("preflight run %s failed to complete: %w", rs.RunID, err)
	}
	if report.Blocking() {
		return fmt.Errorf("preflight found blocking issues for run %s - see FAIL lines above", rs.RunID)
	}
	fmt.Println("preflight passed - safe to proceed with `stalwart-migrate run`")
	return nil
}
