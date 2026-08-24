// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package checkpoint

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DefaultBaseDir is where runs are persisted when the operator doesn't
// override it.
const DefaultBaseDir = "/var/lib/stalwart-migrator/runs"

// Store persists RunState to disk. It's the only thing in this package that
// touches the filesystem - RunState itself is a plain data type.
type Store struct {
	baseDir string
}

func NewStore(baseDir string) *Store {
	if baseDir == "" {
		baseDir = DefaultBaseDir
	}
	return &Store{baseDir: baseDir}
}

func (s *Store) runDir(runID string) string    { return filepath.Join(s.baseDir, runID) }
func (s *Store) statePath(runID string) string { return filepath.Join(s.runDir(runID), "state.json") }

// Create starts a new run, assigns it an ID, and persists its initial
// state before returning it.
func (s *Store) Create(sourceVersion, targetVersion string) (*RunState, error) {
	id, err := newRunID()
	if err != nil {
		return nil, fmt.Errorf("checkpoint: generate run id: %w", err)
	}
	now := time.Now().UTC()
	rs := &RunState{
		RunID:         id,
		SourceVersion: sourceVersion,
		TargetVersion: targetVersion,
		CreatedAt:     now,
		UpdatedAt:     now,
		Artifacts:     map[string]Artifact{},
	}
	if err := s.Save(rs); err != nil {
		return nil, err
	}
	return rs, nil
}

// Load reads an existing run's state from disk.
func (s *Store) Load(runID string) (*RunState, error) {
	data, err := os.ReadFile(s.statePath(runID))
	if err != nil {
		return nil, fmt.Errorf("checkpoint: load run %s: %w", runID, err)
	}
	var rs RunState
	if err := json.Unmarshal(data, &rs); err != nil {
		return nil, fmt.Errorf("checkpoint: parse run %s: %w", runID, err)
	}
	return &rs, nil
}

// Save writes state to disk atomically: write to a temp file in the same
// directory, fsync it, then rename over the real path. A crash mid-write
// leaves the temp file orphaned and state.json untouched, never a
// truncated or corrupt state.json - that file is the one thing every phase
// and a human operator both trust as the source of truth for what's
// already happened, so a half-written version of it would be worse than an
// old one.
func (s *Store) Save(rs *RunState) error {
	dir := s.runDir(rs.RunID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("checkpoint: create run directory: %w", err)
	}
	data, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		return fmt.Errorf("checkpoint: marshal state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("checkpoint: create temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("checkpoint: write temp state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("checkpoint: sync temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("checkpoint: close temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, s.statePath(rs.RunID)); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("checkpoint: rename temp state file into place: %w", err)
	}
	return nil
}

// List returns known run IDs, most recently created first.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.baseDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint: list runs: %w", err)
	}
	type run struct {
		id      string
		created time.Time
	}
	var runs []run
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rs, err := s.Load(e.Name())
		if err != nil {
			continue // not a run directory (or a corrupt one) - skip it
		}
		runs = append(runs, run{rs.RunID, rs.CreatedAt})
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].created.After(runs[j].created) })
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.id
	}
	return ids, nil
}

// RunStep executes fn for (phase, name) unless it already completed
// successfully in a prior attempt at this run, in which case fn is skipped
// and the previously recorded StepOutcome is returned instead - this is
// what makes an interrupted run resumable without redoing (or worse,
// double-applying) work that already happened. State is persisted both
// before fn runs (so a crash during fn is visible as "running", not silently
// forgotten) and after (recording success or failure).
func (s *Store) RunStep(rs *RunState, phase Phase, name string, fn func() (StepOutcome, error)) (StepOutcome, error) {
	if rs.Done(phase, name) {
		return rs.Outcome(phase, name), nil
	}
	rs.Begin(phase, name)
	if err := s.Save(rs); err != nil {
		return StepOutcome{}, fmt.Errorf("checkpoint: persist step start for %s/%s: %w", phase, name, err)
	}
	outcome, stepErr := fn()
	if stepErr != nil {
		rs.Fail(phase, name, stepErr)
	} else {
		rs.Complete(phase, name, outcome)
	}
	if err := s.Save(rs); err != nil {
		if stepErr != nil {
			return outcome, fmt.Errorf("%w (additionally failed to persist checkpoint: %v)", stepErr, err)
		}
		return outcome, fmt.Errorf("step %s/%s succeeded but failed to persist checkpoint: %w", phase, name, err)
	}
	return outcome, stepErr
}

func newRunID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102-150405"), hex.EncodeToString(b)), nil
}
