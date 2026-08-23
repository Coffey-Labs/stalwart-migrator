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
