// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package backup

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
)

func TestBackupRunEndToEndAndResume(t *testing.T) {
	dir := t.TempDir()

	binaryPath := filepath.Join(dir, "stalwart")
	if err := os.WriteFile(binaryPath, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "db.bin"), []byte("some rocksdb-shaped data"), 0o644); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(dir, "data-backup")

	settingsPath := filepath.Join(dir, "settings.json")
	principalsPath := filepath.Join(dir, "principals.json")
	pythonScript := fmt.Sprintf("#!/bin/sh\necho fake-settings > %q\necho fake-principals > %q\n", settingsPath, principalsPath)
	pythonDir := withFakeExecutable(t, "python3", pythonScript)
	pythonScriptPath := filepath.Join(pythonDir, "python3")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("print('fake migrate_v016.py')\n"))
	}))
	defer srv.Close()

	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	rs.Topology = checkpoint.Topology{StoreBackend: "rocksdb"}

	opts := Options{
		BinaryPath:         binaryPath,
		DataDir:            dataDir,
		BackupDir:          backupDir,
		MigrationScriptURL: srv.URL,
		ScriptDestPath:     filepath.Join(dir, "migrate_v016.py"),
		AdminURL:           "https://mail.example.com",
		AdminUser:          "admin",
		AdminPassword:      "hunter2",
		SettingsDumpPath:   settingsPath,
		PrincipalsDumpPath: principalsPath,
	}

	report, err := Run(context.Background(), store, rs, opts)
	if err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	for _, r := range report.Results {
		if r.Status == StatusFail {
			t.Errorf("Run #1: step %s failed: %s", r.Name, r.Detail)
		}
	}
	for _, name := range []string{"old-binary", "fs-backup", "settings-dump", "principals-dump"} {
		if _, ok := rs.Artifacts[name]; !ok {
			t.Errorf("Run #1: expected artifact %q to be recorded, got %v", name, rs.Artifacts)
		}
	}
	if _, err := os.Stat(binaryPath + ".v0.15.5"); err != nil {
		t.Errorf("preserved binary missing: %v", err)
	}

	// -- simulate a crash and resume, making every non-idempotent step's
	// -- inputs unusable so a re-execution (rather than a skip) fails loudly.
	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if err := os.WriteFile(pythonScriptPath, []byte("#!/bin/sh\necho should-not-run-again >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	resumed, err := store.Load(rs.RunID)
	if err != nil {
		t.Fatalf("store.Load (resume): %v", err)
	}
	report2, err := Run(context.Background(), store, resumed, opts)
	if err != nil {
		t.Fatalf("Run #2 (resume) should succeed without redoing completed steps: %v", err)
	}
	if len(report2.Results) != len(report.Results) {
		t.Errorf("resumed report has %d results, want %d (same as first run)", len(report2.Results), len(report.Results))
	}
	for _, r := range report2.Results {
		if r.Status == StatusFail {
			t.Errorf("Run #2 (resume): step %s unexpectedly failed: %s", r.Name, r.Detail)
		}
	}
}

func TestBackupRunSkipsVandelayWhenNoAccountsGiven(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "stalwart")
	os.WriteFile(binaryPath, []byte("fake binary"), 0o755)
	dataDir := filepath.Join(dir, "data")
	os.MkdirAll(dataDir, 0o755)
	os.WriteFile(filepath.Join(dataDir, "db.bin"), []byte("x"), 0o644)

	withFakeExecutable(t, "python3", fmt.Sprintf(
		"#!/bin/sh\necho x > %q\necho x > %q\n",
		filepath.Join(dir, "settings.json"), filepath.Join(dir, "principals.json"),
	))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("script"))
	}))
	defer srv.Close()

	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatal(err)
	}
	rs.Topology = checkpoint.Topology{StoreBackend: "rocksdb"}

	opts := Options{
		BinaryPath:         binaryPath,
		DataDir:            dataDir,
		BackupDir:          filepath.Join(dir, "data-backup"),
		MigrationScriptURL: srv.URL,
		ScriptDestPath:     filepath.Join(dir, "migrate_v016.py"),
		AdminURL:           "https://mail.example.com",
		SettingsDumpPath:   filepath.Join(dir, "settings.json"),
		PrincipalsDumpPath: filepath.Join(dir, "principals.json"),
		// Accounts intentionally left empty.
	}

	report, err := Run(context.Background(), store, rs, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, r := range report.Results {
		if r.Name == "vandelay-export" {
			found = true
			if r.Status != StatusSkipped {
				t.Errorf("vandelay-export status = %s, want skipped", r.Status)
			}
		}
	}
	if !found {
		t.Error("report missing a vandelay-export entry")
	}
}

func TestBackupRunSkipsBinaryPreservationForDryRun(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "stalwart")
	if err := os.WriteFile(binaryPath, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "data")
	os.MkdirAll(dataDir, 0o755)
	os.WriteFile(filepath.Join(dataDir, "db.bin"), []byte("x"), 0o644)

	withFakeExecutable(t, "python3", fmt.Sprintf(
		"#!/bin/sh\necho x > %q\necho x > %q\n",
		filepath.Join(dir, "settings.json"), filepath.Join(dir, "principals.json"),
	))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("script"))
	}))
	defer srv.Close()

	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatal(err)
	}
	rs.Topology = checkpoint.Topology{StoreBackend: "rocksdb"}

	opts := Options{
		BinaryPath:             binaryPath,
		SkipBinaryPreservation: true,
		DataDir:                dataDir,
		BackupDir:              filepath.Join(dir, "data-backup"),
		MigrationScriptURL:     srv.URL,
		ScriptDestPath:         filepath.Join(dir, "migrate_v016.py"),
		AdminURL:               "https://mail.example.com",
		SettingsDumpPath:       filepath.Join(dir, "settings.json"),
		PrincipalsDumpPath:     filepath.Join(dir, "principals.json"),
	}

	report, err := Run(context.Background(), store, rs, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, r := range report.Results {
		if r.Name == "preserve-binary" && r.Status != StatusSkipped {
			t.Errorf("preserve-binary status = %s, want skipped", r.Status)
		}
	}
	if _, err := os.Stat(binaryPath); err != nil {
		t.Errorf("production binary at %s should be untouched, but stat failed: %v", binaryPath, err)
	}
	if _, ok := rs.Artifacts["old-binary"]; ok {
		t.Error("no old-binary artifact should be recorded when preservation is skipped")
	}
}
