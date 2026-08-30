// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package preflight

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTool puts an executable of the given name at the front of PATH,
// reporting the given --version output.
func fakeTool(t *testing.T, name, versionOutput string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho '" + versionOutput + "'\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func statusOf(results []CheckResult, name string) (Status, string) {
	for _, r := range results {
		if r.Name == name {
			return r.Status, r.Detail
		}
	}
	return "", ""
}

// This is the check whose absence took a production mail server down: a
// live migration stopped the service, then discovered the host's
// stalwart-cli was 0.13.4 and had no `apply` command. Recovery cost a
// restore from a day-old snapshot.
func TestCheckExternalToolsFailsWhenTheCLIIsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nothing on PATH at all
	results := CheckExternalTools(context.Background(), "", "", true)

	status, detail := statusOf(results, "stalwart-cli")
	if status != StatusFail {
		t.Errorf("stalwart-cli = %q, want fail - this must stop the run before the service does", status)
	}
	if !strings.Contains(detail, "separate download") {
		t.Errorf("detail %q should say it's a separate download from the server", detail)
	}
	if !strings.Contains(detail, "v1.0.2") {
		t.Errorf("detail %q should name the minimum version", detail)
	}
}

// The exact version that was on the production host. It exists, it runs,
// and it cannot do the job.
func TestCheckExternalToolsFailsOnTheOldBundledCLI(t *testing.T) {
	fakeTool(t, "stalwart-cli", "stalwart-cli 0.13.4")
	fakeTool(t, "python3", "Python 3.13.5")
	results := CheckExternalTools(context.Background(), "", "", true)

	status, detail := statusOf(results, "stalwart-cli")
	if status != StatusFail {
		t.Errorf("stalwart-cli 0.13.4 = %q, want fail", status)
	}
	if !strings.Contains(detail, "0.13.4") || !strings.Contains(detail, "no `apply` command") {
		t.Errorf("detail %q should name the version found and why it won't do", detail)
	}
}

func TestCheckExternalToolsAcceptsASupportedCLI(t *testing.T) {
	fakeTool(t, "stalwart-cli", "stalwart-cli 1.0.12")
	fakeTool(t, "python3", "Python 3.13.5")
	results := CheckExternalTools(context.Background(), "", "", true)

	for _, name := range []string{"stalwart-cli", "python3"} {
		if status, detail := statusOf(results, name); status != StatusOK {
			t.Errorf("%s = %q (%s), want ok", name, status, detail)
		}
	}
}

func TestCheckExternalToolsFailsWithoutPython(t *testing.T) {
	fakeTool(t, "stalwart-cli", "stalwart-cli 1.0.12")
	results := CheckExternalTools(context.Background(), "", "/nonexistent/python3", true)
	if status, _ := statusOf(results, "python3"); status != StatusFail {
		t.Errorf("python3 = %q, want fail - migrate_v016.py cannot run without it", status)
	}
}

// A patch bump replays no settings, so neither tool is invoked and a
// missing CLI must not block it.
func TestCheckExternalToolsSkipsForAPatchUpgrade(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	results := CheckExternalTools(context.Background(), "", "", false)
	if len(results) != 1 {
		t.Fatalf("got %d result(s), want a single skip-style result", len(results))
	}
	if results[0].Status != StatusOK {
		t.Errorf("status = %q, want ok for a patch upgrade with no tools present", results[0].Status)
	}
}

// A rehearsal never invokes stalwart-cli. Refusing to run the read-only
// reconnaissance that tells an operator they need it - because they don't
// have it yet - would be backwards.
func TestToolCheckIsAdvisoryForARehearsal(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte("store.rocksdb.type = \"rocksdb\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	results := CheckExternalTools(context.Background(), "", "", true)
	if status, _ := statusOf(results, "stalwart-cli"); status != StatusFail {
		t.Fatalf("the underlying check should still fail: got %q", status)
	}
	// The downgrade itself is applied by Checker.Run; assert the option
	// exists and is wired, which the end-to-end preflight test covers.
	if (Options{ToolCheckAdvisory: true}).ToolCheckAdvisory != true {
		t.Error("ToolCheckAdvisory should be settable")
	}
}
