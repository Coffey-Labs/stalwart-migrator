// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPreserveBinary(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "stalwart")
	if err := os.WriteFile(binaryPath, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	preserved, err := PreserveBinary(binaryPath, "0.15.5")
	if err != nil {
		t.Fatalf("PreserveBinary: %v", err)
	}
	wantPath := binaryPath + ".v0.15.5"
	if preserved != wantPath {
		t.Errorf("preserved = %s, want %s", preserved, wantPath)
	}
	if _, err := os.Stat(binaryPath); !os.IsNotExist(err) {
		t.Error("original binary path should no longer exist after preservation")
	}
	if _, err := os.Stat(preserved); err != nil {
		t.Errorf("preserved binary missing: %v", err)
	}
}

func TestPreserveBinaryIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "stalwart")
	if err := os.WriteFile(binaryPath, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	first, err := PreserveBinary(binaryPath, "0.15.5")
	if err != nil {
		t.Fatalf("PreserveBinary #1: %v", err)
	}

	// Simulate a resumed run: the binary is already gone from binaryPath
	// (moved on the prior attempt). Calling again must not error just
	// because the source no longer exists.
	second, err := PreserveBinary(binaryPath, "0.15.5")
	if err != nil {
		t.Fatalf("PreserveBinary #2 (resume): %v", err)
	}
	if first != second {
		t.Errorf("preserved paths differ across calls: %s vs %s", first, second)
	}
}

func TestPreserveBinaryRequiresSourceVersion(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "stalwart")
	os.WriteFile(binaryPath, []byte("x"), 0o755)
	if _, err := PreserveBinary(binaryPath, ""); err == nil {
		t.Fatal("PreserveBinary with an empty source version should error")
	}
}
