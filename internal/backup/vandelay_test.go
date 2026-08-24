// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package backup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildVandelayImportArgs(t *testing.T) {
	args := BuildVandelayImportArgs(VandelayOptions{URL: "https://mail.example.com", AuthBasic: "alice:app-pass"}, "alice@example.com", "/backups/alice.sqlite")
	joined := strings.Join(args, " ")
	for _, want := range []string{"import", "jmap", "--url https://mail.example.com", "--auth-basic alice:app-pass", "--account-name alice@example.com", "/backups/alice.sqlite"} {
		if !strings.Contains(joined, want) {
			t.Errorf("BuildVandelayImportArgs = %q, missing %q", joined, want)
		}
	}
}

func TestExportAccountsRunsEachAccount(t *testing.T) {
	dir := t.TempDir()
	log := argsFile(t, dir)
	withFakeExecutable(t, "vandelay", fakeScriptLoggingArgs(log, "exit 0"))

	outDir := filepath.Join(dir, "out")
	files, err := ExportAccounts(context.Background(), VandelayOptions{URL: "https://mail.example.com", AuthBasic: "a:b", OutDir: outDir}, []string{"alice@example.com", "bob@example.com"})
	if err != nil {
		t.Fatalf("ExportAccounts: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2: %v", len(files), files)
	}
	got := readArgsFile(t, log)
	if !strings.Contains(got, "alice@example.com") || !strings.Contains(got, "bob@example.com") {
		t.Errorf("vandelay invocations = %q, want both accounts", got)
	}
}

func TestExportAccountsStopsAtFirstFailure(t *testing.T) {
	dir := t.TempDir()
	// Fails on the second invocation (bob), succeeds on the first (alice).
	script := "#!/bin/sh\ncase \"$*\" in\n  *bob*) echo 'account not found' >&2; exit 1 ;;\n  *) exit 0 ;;\nesac\n"
	withFakeExecutable(t, "vandelay", script)

	outDir := filepath.Join(dir, "out")
	files, err := ExportAccounts(context.Background(), VandelayOptions{URL: "https://mail.example.com", AuthBasic: "a:b", OutDir: outDir}, []string{"alice@example.com", "bob@example.com", "carol@example.com"})
	if err == nil {
		t.Fatal("ExportAccounts should have failed on bob@example.com")
	}
	if len(files) != 1 {
		t.Errorf("got %d successful exports before failure, want 1 (alice only)", len(files))
	}
	if !strings.Contains(err.Error(), "1/3") {
		t.Errorf("error = %v, want it to report 1/3 accounts exported before failing", err)
	}
}
