// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/backup"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
)

// preservePlan lifts the run's irreplaceable inputs out of the scratch
// directory into the run's state directory, which is never cleaned up, and
// records each as a checkpoint artifact.
//
// These four are not intermediate files. The settings and principals dumps
// can only be taken from a live pre-migration instance, and the apply plan
// and its supplement are what was actually replayed into the store - so
// after cutover there is no way to produce any of them again. Until this
// existed they lived only in --work-dir, which a successful run deletes,
// and `rehearse` kept more of its conclusions than `run` did: the
// read-only command preserved the plan and the destructive one threw it
// away.
//
// What made that concrete: an operator who booted recovery mode again
// after a completed migration, for an unrelated reason, and found Domain
// and Account queries coming back empty. Re-applying the original run's
// export.json and supplement.json against a fresh recovery boot is what
// got their server back, twice, on two different machines - and they had
// them only because they had passed --keep-artifacts. Nobody should need
// to have guessed that in advance. Reported by @kaya-eu in #1.
//
// A file that isn't there is skipped rather than failing the step: a patch
// bump converts nothing, and a supplement that couldn't be generated is
// already a warning of its own.
func preservePlan(stateDir string, files map[string]string, rs *checkpoint.RunState) ([]string, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("no state directory to preserve the run's plan in")
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var kept []string
	for _, name := range names {
		src := files[name]
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(stateDir, filepath.Base(src))
		if err := copyFile(src, dst); err != nil {
			return nil, fmt.Errorf("preserve %s as %s: %w", src, dst, err)
		}
		sum, size, err := backup.HashFile(dst)
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", dst, err)
		}
		rs.RecordArtifact(name, checkpoint.Artifact{Path: dst, SHA256: sum, SizeBytes: size})
		kept = append(kept, filepath.Base(dst))
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("none of the run's plan files exist to preserve - the conversion produced nothing")
	}
	return kept, nil
}
