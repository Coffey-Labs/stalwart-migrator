// SPDX-FileCopyrightText: 2026 Coffey Labs LLC
// SPDX-License-Identifier: GPL-3.0-or-later

package plan

import "testing"

func TestDecideCrossesMajorBoundary(t *testing.T) {
	p, err := Decide("0.15.5", "0.16.14")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !p.CrossesMajorBoundary {
		t.Error("CrossesMajorBoundary = false, want true for 0.15.5 -> 0.16.14")
	}
	if !p.HasPhase(PhaseRecovery) {
		t.Errorf("phases %v missing PhaseRecovery", p.Phases)
	}
	wantOrder := []PhaseName{PhasePreflight, PhaseBackup, PhaseRecovery, PhaseCutover, PhaseValidate}
	if len(p.Phases) != len(wantOrder) {
		t.Fatalf("phases = %v, want %v", p.Phases, wantOrder)
	}
	for i, ph := range wantOrder {
		if p.Phases[i] != ph {
			t.Errorf("phases[%d] = %s, want %s", i, p.Phases[i], ph)
		}
	}
}

func TestDecidePatchBumpFastPath(t *testing.T) {
	p, err := Decide("0.16.5", "0.16.14")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if p.CrossesMajorBoundary {
		t.Error("CrossesMajorBoundary = true, want false for a 0.16.x -> 0.16.x bump")
	}
	if p.HasPhase(PhaseRecovery) {
		t.Errorf("phases %v should not include PhaseRecovery on the fast path", p.Phases)
	}
}

func TestDecideRefusesNoOp(t *testing.T) {
	if _, err := Decide("0.16.14", "0.16.14"); err == nil {
		t.Fatal("Decide should refuse when source == target")
	}
}

func TestDecideRefusesRegression(t *testing.T) {
	if _, err := Decide("0.16.14", "0.15.5"); err == nil {
		t.Fatal("Decide should refuse when source is already beyond target")
	}
}

func TestDecideRefusesStalwart1(t *testing.T) {
	// This used to accept 0.15.x -> 1.0.0 as one more crossing of the
	// 0.15/0.16 boundary. Stalwart 1.0 shipped needing its data copied into
	// a new store, which no plan here does, so both routes to it are
	// refused - 0.16.x -> 1.0.0 would otherwise pass as a patch bump.
	for _, src := range []string{"0.15.5", "0.16.22", "0.16.27"} {
		if p, err := Decide(src, "1.0.0"); err == nil {
			t.Errorf("Decide(%s, 1.0.0) = %+v, want a refusal", src, p)
		}
	}
	if _, err := Decide("0.15.5", "0.16.27"); err != nil {
		t.Errorf("Decide(0.15.5, 0.16.27): %v", err)
	}
}

func TestDecideRejectsUnparseableVersions(t *testing.T) {
	if _, err := Decide("not-a-version", "0.16.14"); err == nil {
		t.Fatal("Decide should reject an unparseable source version")
	}
	if _, err := Decide("0.15.5", "not-a-version"); err == nil {
		t.Fatal("Decide should reject an unparseable target version")
	}
}
