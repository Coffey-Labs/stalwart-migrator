// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Coffey-Labs/stalwart-migrator/internal/checkpoint"
)

func writeTemp(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newRunState(t *testing.T) *checkpoint.RunState {
	t.Helper()
	rs, err := checkpoint.NewStore(t.TempDir()).Create("0.15.5", "0.16.19")
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// The dumps can only be taken from a live pre-migration instance and the
// apply plan is what was replayed into the store, so after cutover none of
// them can be produced again. They used to live only in the work dir,
// which a successful run deletes.
func TestPreservePlanKeepsTheRunsInputsOutOfTheScratchDirectory(t *testing.T) {
	work, state := t.TempDir(), t.TempDir()
	rs := newRunState(t)

	kept, err := preservePlan(state, map[string]string{
		"settings-dump":    writeTemp(t, work, "settings.json", `{"settings":1}`),
		"principals-dump":  writeTemp(t, work, "principals.json", `{"principals":1}`),
		"converted-export": writeTemp(t, work, "export.json", `[{"@type":"create"}]`),
		"supplement":       writeTemp(t, work, "supplement.json", `[]`),
	}, rs)
	if err != nil {
		t.Fatalf("preservePlan: %v", err)
	}
	if len(kept) != 4 {
		t.Errorf("kept %v, want all four", kept)
	}

	// The work dir is what gets deleted; what matters is that the state
	// dir now stands on its own.
	if err := os.RemoveAll(work); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"settings-dump", "principals-dump", "converted-export", "supplement"} {
		art, ok := rs.Artifacts[name]
		if !ok {
			t.Errorf("%s was not recorded as an artifact", name)
			continue
		}
		if !strings.HasPrefix(art.Path, state) {
			t.Errorf("%s kept at %s, want it inside the state dir", name, art.Path)
		}
		if _, err := os.Stat(art.Path); err != nil {
			t.Errorf("%s does not survive the work dir being cleaned: %v", name, err)
		}
		if art.SHA256 == "" || art.SizeBytes == 0 {
			t.Errorf("%s recorded without a checksum or size: %+v", name, art)
		}
	}
}

// A patch bump converts nothing and a supplement that could not be
// generated is already a warning of its own, so an absent file is skipped
// rather than failing the step.
func TestPreservePlanSkipsWhatIsNotThere(t *testing.T) {
	work, state := t.TempDir(), t.TempDir()
	rs := newRunState(t)

	kept, err := preservePlan(state, map[string]string{
		"converted-export": writeTemp(t, work, "export.json", `[]`),
		"supplement":       filepath.Join(work, "never-written.json"),
	}, rs)
	if err != nil {
		t.Fatalf("preservePlan: %v", err)
	}
	if len(kept) != 1 || kept[0] != "export.json" {
		t.Errorf("kept %v, want just export.json", kept)
	}
	if _, ok := rs.Artifacts["supplement"]; ok {
		t.Error("a file that was never written should not be recorded as an artifact")
	}
}

// Nothing at all to preserve means the conversion produced nothing, which
// is not a state to carry quietly into a migration.
func TestPreservePlanRefusesWhenThereIsNothingToKeep(t *testing.T) {
	work, state := t.TempDir(), t.TempDir()
	rs := newRunState(t)

	if _, err := preservePlan(state, map[string]string{
		"converted-export": filepath.Join(work, "absent.json"),
	}, rs); err == nil {
		t.Fatal("want an error when none of the plan files exist")
	}
}
