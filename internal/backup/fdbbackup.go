// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package backup

import (
	"context"
	"fmt"
	"os/exec"
)

// FDBOptions configures a FoundationDB backup via fdbbackup, FDB's own
// backup CLI (not something Stalwart-specific) - see ARCHITECTURE.md §4.2.
type FDBOptions struct {
	ClusterFile string // -C; empty uses fdbbackup's own default cluster file
	Destination string // -d, a backup URL e.g. "file:///var/backups/stalwart-fdb"
	Tag         string // -t; defaults to "default" if empty
}

func (o FDBOptions) tag() string {
	if o.Tag == "" {
		return "default"
	}
	return o.Tag
}

// BuildFDBBackupStartArgs returns the fdbbackup argv for starting a backup,
// without the leading "fdbbackup".
func BuildFDBBackupStartArgs(o FDBOptions) []string {
	args := []string{"start"}
	if o.ClusterFile != "" {
		args = append(args, "-C", o.ClusterFile)
	}
	args = append(args, "-d", o.Destination, "-t", o.tag())
	return args
}

// StartFDBBackup kicks off an fdbbackup run. fdbbackup start returns as soon
// as the backup job is registered, not when it finishes - callers that need
// to know it's done should poll FDBBackupStatus and look for their own
// definition of "complete" in its output, since this wrapper doesn't parse
// fdbbackup's status text (that format isn't stable enough here to depend
// on without verifying it against the fdbbackup version actually in use).
func StartFDBBackup(ctx context.Context, o FDBOptions) error {
	cmd := exec.CommandContext(ctx, "fdbbackup", BuildFDBBackupStartArgs(o)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("backup: fdbbackup start failed: %w (output: %s)", err, out)
	}
	return nil
}

// FDBBackupStatus returns fdbbackup's raw status output for the given tag,
// for a human or a caller-supplied parser to interpret.
func FDBBackupStatus(ctx context.Context, o FDBOptions) (string, error) {
	args := []string{"status", "-t", o.tag()}
	if o.ClusterFile != "" {
		args = append(args, "-C", o.ClusterFile)
	}
	cmd := exec.CommandContext(ctx, "fdbbackup", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("backup: fdbbackup status failed: %w (output: %s)", err, out)
	}
	return string(out), nil
}
