// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateSaveLoadRoundtrip(t *testing.T) {
	store := NewStore(t.TempDir())

	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rs.RunID == "" {
		t.Fatal("Create: expected a non-empty run id")
	}

	rs.Topology = Topology{DeploymentKind: "systemd", StoreBackend: "rocksdb"}
	rs.RecordArtifact("fs-backup", Artifact{Path: "/var/lib/stalwart.v0155-backup", SHA256: "deadbeef", SizeBytes: 1024})
	if err := store.Save(rs); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.Load(rs.RunID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.SourceVersion != "0.15.5" || loaded.TargetVersion != "0.16.14" {
		t.Errorf("Load: versions = %s -> %s, want 0.15.5 -> 0.16.14", loaded.SourceVersion, loaded.TargetVersion)
	}
	if loaded.Topology.DeploymentKind != "systemd" {
		t.Errorf("Load: DeploymentKind = %q, want systemd", loaded.Topology.DeploymentKind)
	}
	if got := loaded.Artifacts["fs-backup"].SHA256; got != "deadbeef" {
		t.Errorf("Load: artifact sha256 = %q, want deadbeef", got)
	}
}

func TestSaveIsAtomicNoLeftoverTempFiles(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for i := 0; i < 5; i++ {
		rs.Begin(PhasePreflight, "version")
		rs.Complete(PhasePreflight, "version", StepOutcome{Verdict: "ok", Detail: "ok"})
		if err := store.Save(rs); err != nil {
			t.Fatalf("Save #%d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(dir, rs.RunID))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("unexpected leftover file in run dir: %s", e.Name())
		}
	}
}

func TestRunStepSkipsAlreadyDoneStep(t *testing.T) {
	store := NewStore(t.TempDir())
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	calls := 0
	fn := func() (StepOutcome, error) {
		calls++
		return StepOutcome{Verdict: "ok", Detail: "did the thing"}, nil
	}

	outcome1, err := store.RunStep(rs, PhasePreflight, "version", fn)
	if err != nil {
		t.Fatalf("RunStep #1: %v", err)
	}
	if outcome1.Detail != "did the thing" {
		t.Errorf("RunStep #1 detail = %q, want %q", outcome1.Detail, "did the thing")
	}
	if calls != 1 {
		t.Fatalf("calls after first RunStep = %d, want 1", calls)
	}

	// Simulate a resumed run: same rs, same step name. fn must not run again,
	// and the previously recorded outcome must come back unchanged.
	outcome2, err := store.RunStep(rs, PhasePreflight, "version", fn)
	if err != nil {
		t.Fatalf("RunStep #2 (resume): %v", err)
	}
	if calls != 1 {
		t.Errorf("calls after resumed RunStep = %d, want 1 (fn should be skipped)", calls)
	}
	if outcome2 != outcome1 {
		t.Errorf("RunStep #2 outcome = %+v, want %+v (unchanged from before)", outcome2, outcome1)
	}
}

func TestRunStepRetriesAfterFailure(t *testing.T) {
	store := NewStore(t.TempDir())
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	calls := 0
	failThenSucceed := func() (StepOutcome, error) {
		calls++
		if calls == 1 {
			return StepOutcome{}, errors.New("transient failure")
		}
		return StepOutcome{Verdict: "ok", Detail: "succeeded on retry"}, nil
	}

	if _, err := store.RunStep(rs, PhaseBackup, "fs-snapshot", failThenSucceed); err == nil {
		t.Fatal("RunStep #1: expected error, got nil")
	}
	if rs.Status(PhaseBackup, "fs-snapshot") != StepFailed {
		t.Errorf("status after failed attempt = %s, want failed", rs.Status(PhaseBackup, "fs-snapshot"))
	}

	outcome, err := store.RunStep(rs, PhaseBackup, "fs-snapshot", failThenSucceed)
	if err != nil {
		t.Fatalf("RunStep #2 (retry): %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (failed step must retry, not skip)", calls)
	}
	if outcome.Detail != "succeeded on retry" {
		t.Errorf("detail = %q, want %q", outcome.Detail, "succeeded on retry")
	}
	if !rs.Done(PhaseBackup, "fs-snapshot") {
		t.Error("step should be Done after a successful retry")
	}
}

func TestListOrdersNewestFirst(t *testing.T) {
	store := NewStore(t.TempDir())
	first, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	first.CreatedAt = first.CreatedAt.Add(-time.Hour)
	if err := store.Save(first); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	second, err := store.Create("0.16.14", "0.16.15")
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	ids, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 2 || ids[0] != second.RunID || ids[1] != first.RunID {
		t.Errorf("List = %v, want [%s %s]", ids, second.RunID, first.RunID)
	}
}
