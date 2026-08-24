// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadFileAcceptsMatchingChecksum(t *testing.T) {
	content := "print('fake migration script')\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(content))
	}))
	defer srv.Close()

	sum := sha256.Sum256([]byte(content))
	expected := hex.EncodeToString(sum[:])

	dest := filepath.Join(t.TempDir(), "script.py")
	got, err := DownloadFile(context.Background(), nil, srv.URL, dest, expected)
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	if got != expected {
		t.Errorf("returned checksum = %s, want %s", got, expected)
	}
	data, _ := os.ReadFile(dest)
	if string(data) != content {
		t.Errorf("downloaded content = %q, want %q", data, content)
	}
}

func TestDownloadFileRejectsMismatchedChecksum(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("unexpected content"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "script.py")
	_, err := DownloadFile(context.Background(), nil, srv.URL, dest, "0000000000000000000000000000000000000000000000000000000000000000")
	if err == nil {
		t.Fatal("DownloadFile should reject a checksum mismatch")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Error("DownloadFile should remove the file it wrote after a checksum mismatch")
	}
}

func TestDownloadFileWithoutPinReturnsComputedHash(t *testing.T) {
	content := "arbitrary content"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(content))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "script.py")
	got, err := DownloadFile(context.Background(), nil, srv.URL, dest, "")
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	sum := sha256.Sum256([]byte(content))
	want := hex.EncodeToString(sum[:])
	if got != want {
		t.Errorf("returned checksum = %s, want %s", got, want)
	}
}

func TestRunSettingsDumpInvokesScriptWithFlags(t *testing.T) {
	dir := t.TempDir()
	log := argsFile(t, dir)
	pythonDir := withFakeExecutable(t, "python3", fakeScriptLoggingArgs(log, "exit 0"))

	err := RunSettingsDump(context.Background(), SettingsDumpOptions{
		PythonPath:     filepath.Join(pythonDir, "python3"),
		ScriptPath:     "/opt/migrate_v016.py",
		URL:            "https://mail.example.com",
		Username:       "admin",
		Password:       "hunter2",
		SettingsPath:   filepath.Join(dir, "settings.json"),
		PrincipalsPath: filepath.Join(dir, "principals.json"),
	})
	if err != nil {
		t.Fatalf("RunSettingsDump: %v", err)
	}
	got := readArgsFile(t, log)
	for _, want := range []string{"/opt/migrate_v016.py", "dump", "--url https://mail.example.com", "--username admin"} {
		if !strings.Contains(got, want) {
			t.Errorf("script invoked with %q, missing %q", got, want)
		}
	}
}

func TestRunSettingsDumpPropagatesFailure(t *testing.T) {
	pythonDir := withFakeExecutable(t, "python3", "#!/bin/sh\necho 'auth failed' >&2\nexit 1\n")
	err := RunSettingsDump(context.Background(), SettingsDumpOptions{
		PythonPath: filepath.Join(pythonDir, "python3"),
		ScriptPath: "/opt/migrate_v016.py",
	})
	if err == nil {
		t.Fatal("RunSettingsDump should error when the script exits non-zero")
	}
	if !strings.Contains(err.Error(), "auth failed") {
		t.Errorf("error = %v, want it to include the script's stderr", err)
	}
}

func TestRunSettingsConvertInvokesScriptWithFlags(t *testing.T) {
	dir := t.TempDir()
	log := argsFile(t, dir)
	pythonDir := withFakeExecutable(t, "python3", fakeScriptLoggingArgs(log, "exit 0"))

	err := RunSettingsConvert(context.Background(), SettingsConvertOptions{
		PythonPath:     filepath.Join(pythonDir, "python3"),
		ScriptPath:     "/opt/migrate_v016.py",
		SettingsPath:   filepath.Join(dir, "settings.json"),
		PrincipalsPath: filepath.Join(dir, "principals.json"),
		ConfigPath:     filepath.Join(dir, "config.json"),
		OutputPath:     filepath.Join(dir, "export.json"),
	})
	if err != nil {
		t.Fatalf("RunSettingsConvert: %v", err)
	}
	got := readArgsFile(t, log)
	for _, want := range []string{"convert", "--settings", "--config", "--output"} {
		if !strings.Contains(got, want) {
			t.Errorf("script invoked with %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "--patch-paths") {
		t.Errorf("script invoked with %q, should not include --patch-paths when none given", got)
	}
}

func TestRunSettingsConvertWithPatchPaths(t *testing.T) {
	dir := t.TempDir()
	log := argsFile(t, dir)
	pythonDir := withFakeExecutable(t, "python3", fakeScriptLoggingArgs(log, "exit 0"))

	err := RunSettingsConvert(context.Background(), SettingsConvertOptions{
		PythonPath:     filepath.Join(pythonDir, "python3"),
		ScriptPath:     "/opt/migrate_v016.py",
		SettingsPath:   filepath.Join(dir, "settings.json"),
		PrincipalsPath: filepath.Join(dir, "principals.json"),
		ConfigPath:     filepath.Join(dir, "config.json"),
		OutputPath:     filepath.Join(dir, "export.json"),
		PatchPaths:     map[string]string{"/var/lib/stalwart": "/tmp/sandbox/stalwart"},
	})
	if err != nil {
		t.Fatalf("RunSettingsConvert: %v", err)
	}
	got := readArgsFile(t, log)
	if !strings.Contains(got, "--patch-paths /var/lib/stalwart=/tmp/sandbox/stalwart") {
		t.Errorf("script invoked with %q, missing the expected --patch-paths flag", got)
	}
}

func TestRunSettingsConvertPropagatesFailure(t *testing.T) {
	pythonDir := withFakeExecutable(t, "python3", "#!/bin/sh\necho 'unsupported settings key' >&2\nexit 1\n")
	err := RunSettingsConvert(context.Background(), SettingsConvertOptions{
		PythonPath: filepath.Join(pythonDir, "python3"),
		ScriptPath: "/opt/migrate_v016.py",
	})
	if err == nil {
		t.Fatal("RunSettingsConvert should error when the script exits non-zero")
	}
	if !strings.Contains(err.Error(), "unsupported settings key") {
		t.Errorf("error = %v, want it to include the script's stderr", err)
	}
}

// The report this parses is the most consequential output of a real
// migration: against a production instance with 12,401 settings,
// migrate_v016.py migrated 219 of them and listed the other 12,182 here.
// Losing or ignoring this file means bringing up a server that answers on
// no ports, since server.listener is among the settings that don't carry.
const sampleUnmigrated = `# Unmigrated v0.15 settings

These v0.15 settings were not migrated by the script and must be
reviewed manually.

Total unmigrated keys: 12182 across 69 prefixes.

  server.blocked-ip                     8547 keys
  lookup.url-redirectors                1076 keys
  spam-filter.rule                       424 keys
  server.listener                         26 keys
  asn.expires                              1 keys
`

func TestReadUnmigratedReportParsesTotalsAndPrefixes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unmigrated.txt")
	if err := os.WriteFile(path, []byte(sampleUnmigrated), 0o640); err != nil {
		t.Fatal(err)
	}

	report, err := ReadUnmigratedReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalKeys != 12182 {
		t.Errorf("TotalKeys = %d, want 12182", report.TotalKeys)
	}
	if len(report.Prefixes) != 5 {
		t.Fatalf("parsed %d prefixes, want 5", len(report.Prefixes))
	}
	// Largest first, so the summary leads with what matters most.
	if report.Prefixes[0].Prefix != "server.blocked-ip" || report.Prefixes[0].Keys != 8547 {
		t.Errorf("first prefix = %+v, want server.blocked-ip 8547", report.Prefixes[0])
	}

	summary := report.Summary(3)
	if !strings.Contains(summary, "12182") || !strings.Contains(summary, "must be recreated by hand") {
		t.Errorf("summary should lead with the scale of the problem:\n%s", summary)
	}
	if !strings.Contains(summary, "and 2 more prefix(es)") {
		t.Errorf("summary should say how much it elided:\n%s", summary)
	}
}

// An older script, or a conversion with nothing left over, writes no file.
// That is not an error.
func TestReadUnmigratedReportTreatsMissingFileAsNoReport(t *testing.T) {
	report, err := ReadUnmigratedReport(filepath.Join(t.TempDir(), "absent.txt"))
	if err != nil {
		t.Errorf("missing report should not be an error: %v", err)
	}
	if report != nil {
		t.Errorf("report = %+v, want nil", report)
	}
}

// migrate_v016.py writes unmigrated.txt into its working directory, so the
// convert has to run somewhere writable that the caller knows about.
func TestRunSettingsConvertRunsInTheGivenWorkDir(t *testing.T) {
	dir := t.TempDir()
	workDir := filepath.Join(dir, "work")
	log := argsFile(t, dir)
	withFakeExecutable(t, "python3", fakeScriptLoggingArgs(log, "pwd >> "+log+"\ntouch unmigrated.txt"))

	err := RunSettingsConvert(context.Background(), SettingsConvertOptions{
		ScriptPath: "/tmp/migrate_v016.py", SettingsPath: "/tmp/s.json", PrincipalsPath: "/tmp/p.json",
		ConfigPath: filepath.Join(dir, "c.json"), OutputPath: filepath.Join(dir, "e.json"),
		WorkDir: workDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readArgsFile(t, log), workDir) {
		t.Errorf("script did not run in WorkDir; log:\n%s", readArgsFile(t, log))
	}
	if _, err := os.Stat(filepath.Join(workDir, "unmigrated.txt")); err != nil {
		t.Errorf("unmigrated.txt should land in WorkDir, not the caller's cwd: %v", err)
	}
}
