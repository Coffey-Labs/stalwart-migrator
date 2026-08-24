// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// criticalTables is the exact table set Stalwart's own v0.16 upgrade guide
// backs up before migrating (principals/directory, domains, and the other
// tables its migration script and recovery mode depend on) - a targeted
// dump, not a full-instance one, matching the guide's own tested restore
// path and staying fast on large installs. See ARCHITECTURE.md §4.2 and
// UPGRADING/v0_16.md.
var criticalTables = []string{"s", "d", "r", "h", "b", "g", "j", "f", "u"}

// SQLOptions configures a targeted critical-table dump for an external SQL
// store backend.
type SQLOptions struct {
	Host     string
	Port     string
	Database string
	User     string
	Password string
	OutPath  string
}

// BuildPgDumpArgs returns pg_dump's argv for the critical-table backup,
// without the leading "pg_dump" itself.
func BuildPgDumpArgs(o SQLOptions) []string {
	args := []string{"-U", o.User, "-d", o.Database}
	if o.Host != "" {
		args = append(args, "-h", o.Host)
	}
	if o.Port != "" {
		args = append(args, "-p", o.Port)
	}
	for _, t := range criticalTables {
		args = append(args, "-t", t)
	}
	return append(args, "-f", o.OutPath)
}

// BuildMySQLDumpArgs returns mysqldump's argv for the same critical-table
// set. mysqldump writes to stdout, so RunMySQLDump redirects it rather than
// this function taking an output path flag.
func BuildMySQLDumpArgs(o SQLOptions) []string {
	args := []string{"-u", o.User, o.Database}
	if o.Host != "" {
		args = append(args, "-h", o.Host)
	}
	if o.Port != "" {
		args = append(args, "-P", o.Port)
	}
	return append(args, criticalTables...)
}

// RunPgDump executes pg_dump with the password passed via the standard
// PGPASSWORD environment variable, never on the command line where it would
// be visible to anything reading this process's argv (e.g. `ps`).
func RunPgDump(ctx context.Context, o SQLOptions) error {
	cmd := exec.CommandContext(ctx, "pg_dump", BuildPgDumpArgs(o)...)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+o.Password)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("backup: pg_dump failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RunMySQLDump executes mysqldump with the password passed via the standard
// MYSQL_PWD environment variable, redirecting its stdout to o.OutPath.
func RunMySQLDump(ctx context.Context, o SQLOptions) error {
	f, err := os.OpenFile(o.OutPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("backup: create %s: %w", o.OutPath, err)
	}
	defer f.Close()

	cmd := exec.CommandContext(ctx, "mysqldump", BuildMySQLDumpArgs(o)...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+o.Password)
	cmd.Stdout = f
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("backup: mysqldump failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
