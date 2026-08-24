// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
)

// writeFakeBinary creates a shell script that behaves like `stalwart
// --version` and records each invocation to counterPath, so tests can
// assert a checkpointed step was (or wasn't) re-executed on resume.
func writeFakeBinary(t *testing.T, version, counterPath string) string {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-stalwart.sh")
	script := fmt.Sprintf("#!/bin/sh\necho invoked >> %q\necho 'stalwart %s'\n", counterPath, version)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return scriptPath
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Split(strings.TrimSpace(string(data)), "\n"))
}

func TestCheckerRunEndToEndAndResume(t *testing.T) {
	// preflight now verifies the external tools before anything else; give
	// this environment ones that satisfy it.
	fakeTool(t, "stalwart-cli", "stalwart-cli 1.0.12")
	fakeTool(t, "python3", "Python 3.13.5")
	// -- fixtures --------------------------------------------------------
	counterPath := filepath.Join(t.TempDir(), "invocations")
	binaryPath := writeFakeBinary(t, "0.15.5", counterPath)

	configPath := filepath.Join(t.TempDir(), "config.toml")
	tomlCfg := "[store.\"rocksdb\"]\ntype = \"rocksdb\"\npath = \"/var/lib/stalwart/data\"\n"
	if err := os.WriteFile(configPath, []byte(tomlCfg), 0o644); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "db"), make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}

	withFakeGithub(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Release{
			TagName: "v0.16.14",
			Assets:  []ReleaseAsset{{Name: "checksums.txt"}},
		})
	})

	stateDir := t.TempDir()
	store := checkpoint.NewStore(stateDir)
	rs, err := store.Create("", "latest")
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	checker := New(Options{
		BinaryPath:    binaryPath,
		ConfigPath:    configPath,
		DataDir:       dataDir,
		TargetVersion: "latest",
	})

	// -- first run: everything should execute -----------------------------
	report, err := checker.Run(context.Background(), store, rs)
	if err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	if report.Blocking() {
		t.Fatalf("Run #1: unexpected blocking report:\n%s", report.String())
	}
	if got := countLines(t, counterPath); got != 1 {
		t.Fatalf("binary invocations after Run #1 = %d, want 1", got)
	}
	if rs.SourceVersion != "0.15.5" {
		t.Errorf("rs.SourceVersion = %q, want 0.15.5", rs.SourceVersion)
	}
	if rs.TargetVersion != "0.16.14" {
		t.Errorf("rs.TargetVersion = %q, want 0.16.14 (resolved from \"latest\")", rs.TargetVersion)
	}
	if rs.Topology.StoreBackend != "rocksdb" {
		t.Errorf("rs.Topology.StoreBackend = %q, want rocksdb", rs.Topology.StoreBackend)
	}

	foundDirection := false
	for _, res := range report.Results {
		if res.Name == "upgrade-direction" {
			foundDirection = true
			if !strings.Contains(res.Detail, "major boundary") {
				t.Errorf("upgrade-direction detail = %q, want mention of the major boundary", res.Detail)
			}
		}
	}
	if !foundDirection {
		t.Error("report missing upgrade-direction check")
	}

	// -- simulate a crash and resume: reload state from disk fresh --------
	resumed, err := store.Load(rs.RunID)
	if err != nil {
		t.Fatalf("store.Load (resume): %v", err)
	}
	report2, err := checker.Run(context.Background(), store, resumed)
	if err != nil {
		t.Fatalf("Run #2 (resume): %v", err)
	}
	if got := countLines(t, counterPath); got != 1 {
		t.Errorf("binary invocations after resumed Run = %d, want 1 (already-done steps must not re-execute)", got)
	}
	if len(report2.Results) != len(report.Results) {
		t.Errorf("resumed report has %d results, want %d (same as first run)", len(report2.Results), len(report.Results))
	}
}

func TestCheckerRunFlagsTooOldSource(t *testing.T) {
	counterPath := filepath.Join(t.TempDir(), "invocations")
	binaryPath := writeFakeBinary(t, "0.14.2", counterPath)

	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("[server]\nhostname = \"mail.example.com\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()

	withFakeGithub(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Release{TagName: "v0.16.14"})
	})

	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("", "latest")
	if err != nil {
		t.Fatal(err)
	}
	checker := New(Options{BinaryPath: binaryPath, ConfigPath: configPath, DataDir: dataDir, TargetVersion: "latest"})

	report, err := checker.Run(context.Background(), store, rs)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Blocking() {
		t.Fatalf("expected a blocking report for a too-old source version, got:\n%s", report.String())
	}
}

func TestCheckerRunFlagsInsufficientDiskSpace(t *testing.T) {
	counterPath := filepath.Join(t.TempDir(), "invocations")
	binaryPath := writeFakeBinary(t, "0.15.5", counterPath)

	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("[store.\"rocksdb\"]\ntype = \"rocksdb\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "db"), make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}

	withFakeGithub(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Release{TagName: "v0.16.14"})
	})

	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("", "latest")
	if err != nil {
		t.Fatal(err)
	}
	// An absurd multiple guarantees the free-space check fails regardless
	// of how much space the test host actually has free.
	checker := New(Options{
		BinaryPath: binaryPath, ConfigPath: configPath, DataDir: dataDir,
		TargetVersion: "latest", MinFreeMultiple: 1e12,
	})

	report, err := checker.Run(context.Background(), store, rs)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Blocking() {
		t.Fatalf("expected a blocking report for insufficient disk space, got:\n%s", report.String())
	}
}

func TestCheckerRunCapturesAccountSnapshotWhenAdminURLSet(t *testing.T) {
	// preflight now verifies the external tools before anything else; give
	// this environment ones that satisfy it.
	fakeTool(t, "stalwart-cli", "stalwart-cli 1.0.12")
	fakeTool(t, "python3", "Python 3.13.5")
	counterPath := filepath.Join(t.TempDir(), "invocations")
	binaryPath := writeFakeBinary(t, "0.15.5", counterPath)

	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("[store.\"rocksdb\"]\ntype = \"rocksdb\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "db"), make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}

	withFakeGithub(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Release{TagName: "v0.16.14"})
	})

	// A fake admin server: answers Ping's session-discovery GET, the
	// impersonated session-discovery GET MailboxSnapshot makes for
	// alice@example.com, and the POST /api calls for x:Account/query,
	// x:Account/get, and Mailbox/get.
	var apiURL string
	adminSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/principal" {
			w.WriteHeader(http.StatusNotFound) // v0.16 shape: no REST management API
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/.well-known/jmap":
			user, _, _ := r.BasicAuth()
			if strings.Contains(user, "%") {
				json.NewEncoder(w).Encode(map[string]any{
					"apiUrl":          apiURL,
					"primaryAccounts": map[string]string{"urn:ietf:params:jmap:mail": "mail-alice"},
				})
				return
			}
			// A 0.16-era instance: the urn:stalwart:jmap capability is what
			// says its management API is the JMAP one.
			json.NewEncoder(w).Encode(map[string]any{
				"apiUrl":       apiURL,
				"capabilities": map[string]any{"urn:ietf:params:jmap:core": map[string]any{}, "urn:stalwart:jmap": map[string]any{}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			methodCalls := body["methodCalls"].([]any)
			name := methodCalls[0].([]any)[0].(string)
			switch name {
			case "x:Domain/query":
				json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
					[]any{"x:Domain/query", map[string]any{"ids": []string{"d1"}}, "q"},
					[]any{"x:Domain/get", map[string]any{"list": []map[string]any{
						{"id": "d1", "name": "example.com"},
					}}, "g"},
				}})
			case "x:Account/query":
				json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
					[]any{"x:Account/query", map[string]any{"ids": []string{"a1"}}, "q"},
				}})
			case "x:Account/get":
				json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
					[]any{"x:Account/get", map[string]any{"list": []map[string]any{
						{"id": "a1", "name": "alice@example.com", "domainId": "example.com"},
					}}, "g"},
				}})
			case "Mailbox/get":
				json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
					[]any{"Mailbox/get", map[string]any{"list": []map[string]any{
						{"name": "Inbox", "totalEmails": 5},
					}}, "m"},
				}})
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	apiURL = adminSrv.URL + "/api"
	defer adminSrv.Close()

	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("", "latest")
	if err != nil {
		t.Fatal(err)
	}
	checker := New(Options{
		BinaryPath: binaryPath, ConfigPath: configPath, DataDir: dataDir, TargetVersion: "latest",
		AdminURL: adminSrv.URL, AdminUser: "admin", AdminPassword: "hunter2",
	})

	report, err := checker.Run(context.Background(), store, rs)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Blocking() {
		t.Fatalf("unexpected blocking report:\n%s", report.String())
	}
	if rs.PreflightSnapshot == nil {
		t.Fatal("PreflightSnapshot should be populated when AdminURL is set")
	}
	if rs.PreflightSnapshot.AccountCount != 1 {
		t.Errorf("AccountCount = %d, want 1", rs.PreflightSnapshot.AccountCount)
	}
	if len(rs.PreflightSnapshot.Domains) != 1 || rs.PreflightSnapshot.Domains[0] != "example.com" {
		t.Errorf("Domains = %v, want [example.com]", rs.PreflightSnapshot.Domains)
	}
	aliceMailboxes := rs.PreflightSnapshot.MailboxCounts["alice@example.com"]
	if len(aliceMailboxes) != 1 || aliceMailboxes[0].Mailbox != "Inbox" || aliceMailboxes[0].Messages != 5 {
		t.Errorf("alice's mailbox counts = %+v, want [{Inbox 5}]", aliceMailboxes)
	}

	// Resume: the snapshot must survive a reload from disk without
	// re-running the check (the admin server would still work, but this
	// confirms the persisted value is what's actually being relied on).
	resumed, err := store.Load(rs.RunID)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if resumed.PreflightSnapshot == nil || resumed.PreflightSnapshot.AccountCount != 1 {
		t.Errorf("resumed PreflightSnapshot = %+v, want AccountCount 1", resumed.PreflightSnapshot)
	}
}

// A host with no route to the internet cannot reach the release API, and
// failing there would stop it migrating even though the binary it needs is
// already on disk. The binary itself answers the question.
func TestTargetReleaseReadsALocalBinaryInsteadOfTheAPI(t *testing.T) {
	fakeTool(t, "stalwart-cli", "stalwart-cli 1.0.12")
	fakeTool(t, "python3", "Python 3.13.5")
	counter := filepath.Join(t.TempDir(), "invocations")
	current := writeFakeBinary(t, "0.15.5", counter)
	target := writeFakeBinary(t, "0.16.19", counter)

	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("[store.\"rocksdb\"]\ntype = \"rocksdb\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()

	// Point the release API at a listener that fails the test if used.
	withFakeGithub(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the release API must not be consulted when a local target binary was given (%s)", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("", "0.16.19")
	if err != nil {
		t.Fatal(err)
	}
	report, err := New(Options{
		BinaryPath: current, ConfigPath: configPath, DataDir: dataDir,
		TargetVersion: "0.16.19", TargetBinaryPath: target, MinFreeMultiple: 0.0001,
	}).Run(context.Background(), store, rs)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	var found *CheckResult
	for i := range report.Results {
		if report.Results[i].Name == "target-release" {
			found = &report.Results[i]
		}
	}
	if found == nil || found.Status != StatusOK {
		t.Fatalf("target-release = %+v, want OK without touching the network\n%s", found, report.String())
	}
	if !strings.Contains(found.Detail, "0.16.19") {
		t.Fatalf("detail should name the version it read, got %q", found.Detail)
	}
}

func TestTargetReleaseRejectsAMismatchedLocalBinary(t *testing.T) {
	fakeTool(t, "stalwart-cli", "stalwart-cli 1.0.12")
	fakeTool(t, "python3", "Python 3.13.5")
	counter := filepath.Join(t.TempDir(), "invocations")
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("[store.\"rocksdb\"]\ntype = \"rocksdb\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withFakeGithub(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("", "0.16.19")
	if err != nil {
		t.Fatal(err)
	}
	report, err := New(Options{
		BinaryPath: writeFakeBinary(t, "0.15.5", counter), ConfigPath: configPath, DataDir: t.TempDir(),
		TargetVersion: "0.16.19", TargetBinaryPath: writeFakeBinary(t, "0.16.14", counter), MinFreeMultiple: 0.0001,
	}).Run(context.Background(), store, rs)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	for _, r := range report.Results {
		if r.Name == "target-release" {
			if r.Status != StatusFail {
				t.Fatalf("target-release = %+v, want FAIL for a binary that is not the target", r)
			}
			return
		}
	}
	t.Fatal("no target-release check in the report")
}
