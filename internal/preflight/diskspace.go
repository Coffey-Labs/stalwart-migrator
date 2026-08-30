// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package preflight

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"syscall"
)

// DirSize walks dir and sums the size of every regular file in it. Used to
// estimate how much free space a filesystem-level backup copy will need -
// see ARCHITECTURE.md §4.2.
func DirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("preflight: measure size of %s: %w", dir, err)
	}
	return total, nil
}

// FreeBytes returns the free space available (to an unprivileged process)
// on the filesystem containing path.
func FreeBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("preflight: statfs %s: %w", path, err)
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for n/div >= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
