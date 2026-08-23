package rollback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/backup"
)

// PreserveFailedState moves a path aside to "<path>.failed-<runID>" instead
// of deleting it, so a rollback never destroys the failed attempt's own
// state (ARCHITECTURE.md §4.8) - if the rollback itself turns out to have
// been the wrong call, or the failure needs diagnosing afterward, the
// half-migrated data is still there under a name that says which run
// produced it.
//
// It's idempotent for a resumed rollback: if the preserved path already
// exists, a previous attempt already did this, and the function reports
// that rather than clobbering the earlier rescue with whatever is at path
// now.
func PreserveFailedState(path, runID string) (preservedPath string, moved bool, err error) {
	preservedPath = fmt.Sprintf("%s.failed-%s", path, runID)

	if _, statErr := os.Stat(preservedPath); statErr == nil {
		return preservedPath, false, nil
	} else if !os.IsNotExist(statErr) {
		return "", false, fmt.Errorf("rollback: stat %s: %w", preservedPath, statErr)
	}

	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return preservedPath, false, nil // nothing there to preserve
	} else if statErr != nil {
		return "", false, fmt.Errorf("rollback: stat %s: %w", path, statErr)
	}

	if err := os.Rename(path, preservedPath); err != nil {
		return "", false, fmt.Errorf("rollback: move %s aside to %s: %w", path, preservedPath, err)
	}
	return preservedPath, true, nil
}

// RestoreDataDir copies a verified backup back over the original data
// directory and then re-verifies what it wrote against the same manifest.
// The second verification is the point: a restore that silently truncated
// or corrupted a file would otherwise be indistinguishable from a good one
// until Stalwart failed to open its store, long after this tool reported
// success.
//
// The caller must have moved any existing dataDir aside first (see
// PreserveFailedState) - CopyDataDir clears its destination, and clearing a
// live data directory is not a decision this function should be making on
// its own.
func RestoreDataDir(backupDir, dataDir string, m *backup.Manifest) error {
	if _, err := backup.CopyDataDir(backupDir, dataDir); err != nil {
		return fmt.Errorf("rollback: restore %s from %s: %w", dataDir, backupDir, err)
	}
	if err := backup.VerifyDataDirBackup(dataDir, m); err != nil {
		return fmt.Errorf("rollback: the restored data directory doesn't match the backup manifest: %w", err)
	}
	return nil
}

// RestoreBinary puts the preserved old binary back at binaryPath, moving
// whatever is there now aside first (never deleting it - the new-version
// binary is what a retry after the underlying issue is fixed will want).
// It verifies the preserved binary's checksum against the one recorded when
// it was preserved before installing it, so a rollback can't restore a
// binary that was corrupted or swapped since backup ran.
func RestoreBinary(preservedPath, binaryPath, wantSHA256, runID string) (displaced string, err error) {
	sum, _, err := hashFile(preservedPath)
	if err != nil {
		return "", err
	}
	if wantSHA256 != "" && sum != wantSHA256 {
		return "", fmt.Errorf(
			"rollback: preserved binary %s has sha256 %s but the checkpoint recorded %s when it was preserved - refusing to install a binary that changed since then",
			preservedPath, sum, wantSHA256)
	}

	displaced, moved, err := PreserveFailedState(binaryPath, runID)
	if err != nil {
		return "", err
	}
	if !moved {
		displaced = ""
	}

	if err := os.Rename(preservedPath, binaryPath); err != nil {
		return "", fmt.Errorf("rollback: restore %s to %s: %w", preservedPath, binaryPath, err)
	}
	return displaced, nil
}

// RestoreFile copies src over dst (used for a preserved systemd unit or
// Compose file), preserving dst's own mode if it exists and falling back to
// src's otherwise. It writes to a temp file in the destination directory
// and renames it into place, so a service definition is never left
// half-written - the same reason checkpoint.Store.Save does it.
func RestoreFile(src, dst string) error {
	perm := os.FileMode(0o644)
	if info, err := os.Stat(dst); err == nil {
		perm = info.Mode().Perm()
	} else if info, err := os.Stat(src); err == nil {
		perm = info.Mode().Perm()
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("rollback: read %s: %w", src, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("rollback: create temp file next to %s: %w", dst, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("rollback: write %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("rollback: chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("rollback: sync %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("rollback: close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("rollback: move %s into place at %s: %w", tmpPath, dst, err)
	}
	return nil
}

// BuildPsqlArgs returns psql's argv for restoring the critical-table dump
// backup.RunPgDump produced, without the leading "psql". ON_ERROR_STOP=1 is
// not optional here: psql's default is to report an error and carry on,
// which would let a restore that only half-applied exit zero and be
// reported as a successful rollback.
func BuildPsqlArgs(o backup.SQLOptions) []string {
	args := []string{"-v", "ON_ERROR_STOP=1", "-U", o.User, "-d", o.Database}
	if o.Host != "" {
		args = append(args, "-h", o.Host)
	}
	if o.Port != "" {
		args = append(args, "-p", o.Port)
	}
	return append(args, "-f", o.OutPath)
}

// BuildMySQLRestoreArgs returns mysql's argv for the same restore. mysql
// reads the dump from stdin rather than taking a file flag, so RunMySQLRestore
// redirects it.
func BuildMySQLRestoreArgs(o backup.SQLOptions) []string {
	args := []string{"-u", o.User}
	if o.Host != "" {
		args = append(args, "-h", o.Host)
	}
	if o.Port != "" {
		args = append(args, "-P", o.Port)
	}
	return append(args, o.Database)
}

// RunPsqlRestore replays a pg_dump file, passing the password via
// PGPASSWORD exactly as backup.RunPgDump does rather than on the command
// line where `ps` could read it.
func RunPsqlRestore(ctx context.Context, o backup.SQLOptions) error {
	cmd := exec.CommandContext(ctx, "psql", BuildPsqlArgs(o)...)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+o.Password)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rollback: psql restore failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RunMySQLRestore replays a mysqldump file from stdin, with the password
// passed via MYSQL_PWD.
func RunMySQLRestore(ctx context.Context, o backup.SQLOptions) error {
	f, err := os.Open(o.OutPath)
	if err != nil {
		return fmt.Errorf("rollback: open dump %s: %w", o.OutPath, err)
	}
	defer f.Close()

	cmd := exec.CommandContext(ctx, "mysql", BuildMySQLRestoreArgs(o)...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+o.Password)
	cmd.Stdin = f
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rollback: mysql restore failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// hashFile returns a file's SHA256 and size, for checking a preserved
// artifact against what the checkpoint recorded for it.
func hashFile(path string) (sha256Hex string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("rollback: hash %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("rollback: hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
