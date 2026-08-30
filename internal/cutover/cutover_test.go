// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package cutover

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/service"
)

// fakeController stands in for systemd, recording call order.
type fakeController struct {
	calls    []string
	active   bool
	startErr error
}

func (f *fakeController) Stop(context.Context) error {
	f.calls = append(f.calls, "stop")
	f.active = false
	return nil
}

func (f *fakeController) Start(context.Context) error {
	f.calls = append(f.calls, "start")
	if f.startErr != nil {
		return f.startErr
	}
	f.active = true
	return nil
}

func (f *fakeController) Active(context.Context) (bool, error) { return f.active, nil }

func (f *fakeController) ReloadConfig(context.Context) error {
	f.calls = append(f.calls, "reload")
	return nil
}

func (f *fakeController) Target() string { return "test service" }

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// migratedRun builds a checkpoint for a run that backed up successfully and
// is ready to cut over: the state this phase is actually invoked against.
func migratedRun(t *testing.T) (store *checkpoint.Store, rs *checkpoint.RunState, opts Options) {
	t.Helper()
	root := t.TempDir()

	store = checkpoint.NewStore(filepath.Join(root, "runs"))
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatal(err)
	}
	rs.Topology = checkpoint.Topology{DeploymentKind: "systemd", StoreBackend: "rocksdb"}

	staged := filepath.Join(root, "staged-stalwart")
	if err := os.WriteFile(staged, []byte("#!/bin/sh\necho 'stalwart 0.16.14'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(root, "stalwart.service")
	if err := os.WriteFile(unitPath, []byte(realisticUnit), 0o644); err != nil {
		t.Fatal(err)
	}

	return store, rs, Options{
		StagedBinaryPath:       staged,
		BinaryPath:             filepath.Join(root, "bin-stalwart"),
		ServiceUnitPath:        unitPath,
		RecoveryPointConfirmed: true,
		Controller:             &fakeController{},
	}
}

// This tool can't undo a cutover, so the least it can do is refuse to
// perform one without the operator having been asked the question.
func TestBuildPlanRefusesWithoutAConfirmedRecoveryPoint(t *testing.T) {
	_, rs, opts := migratedRun(t)
	opts.RecoveryPointConfirmed = false

	_, err := BuildPlan(rs, opts)
	if err == nil {
		t.Fatal("BuildPlan: want refusal when no recovery point has been confirmed, got nil")
	}
	if !strings.Contains(err.Error(), "no way to undo") {
		t.Errorf("error %q should be plain that this is irreversible for the tool", err)
	}
}

func TestBuildPlanRefusesDockerDeployments(t *testing.T) {
	_, rs, opts := migratedRun(t)
	rs.Topology.DeploymentKind = string(service.Docker)
	opts.Controller = nil

	_, err := BuildPlan(rs, opts)
	if err == nil {
		t.Fatal("BuildPlan: want refusal for a container deployment, got nil")
	}
	if !strings.Contains(err.Error(), "recreating the container") {
		t.Errorf("error %q should explain what cutting over a container would actually involve", err)
	}
}

func TestBuildPlanRefusesAMissingStagedBinaryOrUnit(t *testing.T) {
	for _, tc := range []struct{ name, field string }{{"staged binary", "staged"}, {"service unit", "unit"}} {
		t.Run(tc.name, func(t *testing.T) {
			_, rs, opts := migratedRun(t)
			if tc.field == "staged" {
				opts.StagedBinaryPath = filepath.Join(t.TempDir(), "absent")
			} else {
				opts.ServiceUnitPath = filepath.Join(t.TempDir(), "absent.service")
			}
			if _, err := BuildPlan(rs, opts); err == nil {
				t.Fatalf("BuildPlan: want refusal for a missing %s, got nil", tc.name)
			}
		})
	}
}

func TestRunInstallsRewritesAndStarts(t *testing.T) {
	store, rs, opts := migratedRun(t)
	ctl := opts.Controller.(*fakeController)

	report, err := Run(context.Background(), store, rs, opts)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, report)
	}

	if got := readFile(t, opts.BinaryPath); !strings.Contains(got, "0.16.14") {
		t.Errorf("installed binary = %q, want the staged one", got)
	}
	if info, err := os.Stat(opts.BinaryPath); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("installed binary mode = %v (err %v), want 0755 - a non-executable binary won't start", info.Mode().Perm(), err)
	}
	unit := readFile(t, opts.ServiceUnitPath)
	if !strings.Contains(unit, "ExecStart="+opts.BinaryPath) {
		t.Errorf("unit not repointed at the new binary:\n%s", unit)
	}
	if got, want := strings.Join(ctl.calls, ","), "reload,start"; got != want {
		t.Errorf("controller calls = %q, want %q - the definition must be reloaded before the start", got, want)
	}
	if report.Blocking() {
		t.Errorf("report should be clean:\n%s", report)
	}
}

// Recovery is the operator's own snapshot, but a snapshot revert doesn't
// help someone who only wants their unit file back - so this phase has to
// leave the original where they can find it.
func TestRunPreservesTheOriginalServiceDefinition(t *testing.T) {
	store, rs, opts := migratedRun(t)
	if _, err := Run(context.Background(), store, rs, opts); err != nil {
		t.Fatal(err)
	}

	art, found := rs.Artifacts[ArtifactServiceUnit]
	if !found {
		t.Fatalf("no %q artifact recorded; an operator restoring by hand would have to reconstruct the unit from memory", ArtifactServiceUnit)
	}
	preserved := readFile(t, art.Path)
	if !strings.Contains(preserved, "ExecStart=/usr/local/bin/stalwart --config /etc/stalwart/config.toml") {
		t.Errorf("preserved unit = %q, want the definition as it was before the rewrite", preserved)
	}
	if art.SHA256 == "" {
		t.Error("preserved unit artifact has no checksum")
	}
}

// Installing a binary that isn't the version the run planned for would
// migrate to a version nobody chose.
func TestRunRefusesAStagedBinaryOfTheWrongVersion(t *testing.T) {
	store, rs, opts := migratedRun(t)
	if err := os.WriteFile(opts.StagedBinaryPath, []byte("#!/bin/sh\necho 'stalwart 0.16.9'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), store, rs, opts)
	if err == nil {
		t.Fatal("Run: want failure for a staged binary of the wrong version, got nil")
	}
	if _, statErr := os.Stat(opts.BinaryPath); !os.IsNotExist(statErr) {
		t.Error("the wrong-version binary was installed anyway")
	}
	if got := readFile(t, opts.ServiceUnitPath); !strings.Contains(got, "/usr/local/bin/stalwart") {
		t.Error("the service definition was rewritten despite the refusal")
	}
	if !report.Blocking() {
		t.Error("report should be blocking")
	}
}

func TestRunResumesWithoutRedoingCompletedSteps(t *testing.T) {
	store, rs, opts := migratedRun(t)
	if _, err := Run(context.Background(), store, rs, opts); err != nil {
		t.Fatal(err)
	}

	second := &fakeController{active: true}
	opts.Controller = second
	reloaded, err := store.Load(rs.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), store, reloaded, opts); err != nil {
		t.Fatal(err)
	}
	if len(second.calls) != 0 {
		t.Errorf("second invocation called the controller %v, want nothing - every step was already done", second.calls)
	}
}

func TestRunFailsWhenTheMigratedServiceNeverAnswers(t *testing.T) {
	store, rs, opts := migratedRun(t)
	// Nothing listening at all: a closed port, not an error response.
	opts.AdminURL = "http://127.0.0.1:1"
	opts.HealthTimeout = 300 * time.Millisecond

	report, err := Run(context.Background(), store, rs, opts)
	if err == nil {
		t.Fatal("Run: want failure when nothing answers at all, got nil")
	}
	if !strings.Contains(err.Error(), "never answered") {
		t.Errorf("error %q should distinguish 'started but not answering' from 'failed to start'", err)
	}
	if !report.Blocking() {
		t.Error("report should be blocking")
	}
}

// A 401 proves the service is up and routing. Treating it as unhealthy
// failed a cutover that had actually succeeded: the credentials supplied
// for the pre-migration instance were a config fallback-admin, which does
// not survive into v0.16.
func TestRunWarnsRatherThanFailsWhenCredentialsStopWorking(t *testing.T) {
	store, rs, opts := migratedRun(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	opts.AdminURL = srv.URL
	opts.HealthTimeout = 2 * time.Second

	report, err := Run(context.Background(), store, rs, opts)
	if err != nil {
		t.Fatalf("a service that is up but rejects these credentials is not a failed cutover: %v\n%s", err, report)
	}
	var warned bool
	for _, res := range report.Results {
		if res.Name == "wait-healthy" {
			warned = res.Status == StatusWarn
			if !strings.Contains(res.Detail, "fallback-admin") {
				t.Errorf("warning %q should name the likely cause", res.Detail)
			}
		}
	}
	if !warned {
		t.Errorf("wait-healthy should warn, not fail or pass silently:\n%s", report)
	}
}

// Rolling back a migration that completed successfully, because a counter
// didn't get rebuilt, would be worse than a stale counter.
func TestRunWarnsRatherThanFailsWhenQuotaRecalculationFails(t *testing.T) {
	store, rs, opts := migratedRun(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]any{"apiUrl": "/api"})
			return
		}
		w.WriteHeader(http.StatusInternalServerError) // the management API is unhappy
	}))
	defer srv.Close()
	opts.AdminURL = srv.URL
	opts.RecalculateQuotas = true

	report, err := Run(context.Background(), store, rs, opts)
	if err != nil {
		t.Fatalf("a failed quota rebuild must not fail the cutover: %v\n%s", err, report)
	}
	if report.Blocking() {
		t.Errorf("report should not be blocking:\n%s", report)
	}
	var warned bool
	for _, res := range report.Results {
		if res.Name == "recalculate-quotas" {
			warned = res.Status == StatusWarn
			if !strings.Contains(res.Detail, "Tasks panel") {
				t.Errorf("warning %q should tell the operator how to finish it by hand", res.Detail)
			}
		}
	}
	if !warned {
		t.Errorf("quota failure should be a warning, not silence:\n%s", report)
	}
}

func TestRunSkipsQuotaRecalculationOnThePatchFastPath(t *testing.T) {
	store, rs, opts := migratedRun(t)
	opts.RecalculateQuotas = false

	report, err := Run(context.Background(), store, rs, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range report.Results {
		if res.Name == "recalculate-quotas" {
			if res.Status != StatusSkipped {
				t.Errorf("recalculate-quotas = %s, want skip", res.Status)
			}
			if !strings.Contains(res.Detail, "0.15/0.16") {
				t.Errorf("skip detail %q should say why it isn't needed", res.Detail)
			}
		}
	}
}

// quotaServer answers the three calls quota recalculation makes: enumerate
// accounts, schedule one task per account, then poll until the queue
// drains. Tasks are removed once fetched, modelling a queue whose entries
// are consumed when they run.
func quotaServer(t *testing.T, accountIDs []string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var scheduled []map[string]any
	queued := map[string]bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]any{"apiUrl": "/api"})
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		call := body["methodCalls"].([]any)[0].([]any)
		name := call[0].(string)
		args := call[1].(map[string]any)

		switch name {
		case "x:Account/query":
			json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
				[]any{"x:Account/query", map[string]any{"ids": accountIDs}, "q"},
			}})
		case "x:Task/set":
			created := map[string]any{}
			for creationID, obj := range args["create"].(map[string]any) {
				scheduled = append(scheduled, obj.(map[string]any))
				id := "task-" + creationID
				queued[id] = true
				created[creationID] = map[string]any{"id": id}
			}
			json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
				[]any{"x:Task/set", map[string]any{"created": created}, "s"},
			}})
		case "x:Task/get":
			// Report every task as gone: it ran and left the queue.
			for _, raw := range args["ids"].([]any) {
				delete(queued, raw.(string))
			}
			json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
				[]any{"x:Task/get", map[string]any{"list": []any{}}, "g"},
			}})
		default:
			t.Errorf("unexpected method call %s", name)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &scheduled
}

func TestRunSchedulesOneQuotaTaskPerAccountAndWaits(t *testing.T) {
	store, rs, opts := migratedRun(t)
	srv, scheduled := quotaServer(t, []string{"a1", "a2", "a3"})
	opts.AdminURL = srv.URL
	opts.AdminUser = "admin"
	opts.RecalculateQuotas = true

	report, err := Run(context.Background(), store, rs, opts)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, report)
	}
	if len(*scheduled) != 3 {
		t.Fatalf("scheduled %d task(s), want one per account", len(*scheduled))
	}
	for _, obj := range *scheduled {
		if obj["maintenanceType"] != "recalculateQuota" {
			t.Errorf("maintenanceType = %v, want recalculateQuota", obj["maintenanceType"])
		}
	}
	for _, res := range report.Results {
		if res.Name == "recalculate-quotas" {
			if res.Status != StatusOK {
				t.Errorf("recalculate-quotas = %s: %s", res.Status, res.Detail)
			}
			if !strings.Contains(res.Detail, "3 account(s)") {
				t.Errorf("detail %q should say how many accounts were rebuilt", res.Detail)
			}
		}
	}
}

// A full migration crash-looped 28 times on "Failed to read data store
// settings: Permission denied" because the converted config was written as
// root while the service runs as its own user. The failure surfaced minutes
// later in the journal, not at the moment of the mistake, which is what
// makes it worth a test rather than care.
func TestRunInstallsTheConfigWithOwnershipTheServiceCanRead(t *testing.T) {
	store, rs, opts := migratedRun(t)
	dir := t.TempDir()

	// The config being replaced, standing in for the one the old version
	// ran with - restrictive mode, so a naive copy would lock the service
	// out of its own config.
	oldConfig := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(oldConfig, []byte("[server]\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	converted := filepath.Join(dir, "converted.json")
	if err := os.WriteFile(converted, []byte(`{"@type":"RocksDb","path":"/opt/stalwart/data"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(dir, "config.json")

	opts.ConfigSource = converted
	opts.ConfigPath = installed
	opts.ConfigOwnerReference = oldConfig

	report, err := Run(context.Background(), store, rs, opts)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, report)
	}
	if got := readFile(t, installed); !strings.Contains(got, "RocksDb") {
		t.Errorf("installed config = %q, want the converted one", got)
	}
	info, err := os.Stat(installed)
	if err != nil {
		t.Fatal(err)
	}
	// Mode comes from the file being replaced, not from the source's 0600.
	if info.Mode().Perm() != 0o640 {
		t.Errorf("installed config mode = %v, want 0640 copied from the old config", info.Mode().Perm())
	}
	// The unit must point at the installed config, not the scratch copy.
	if unit := readFile(t, opts.ServiceUnitPath); !strings.Contains(unit, installed) {
		t.Errorf("unit does not reference the installed config:\n%s", unit)
	}
}

func TestRunSkipsConfigInstallWhenThereIsNothingToInstall(t *testing.T) {
	store, rs, opts := migratedRun(t)
	report, err := Run(context.Background(), store, rs, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range report.Results {
		if res.Name == "install-config" && res.Status != StatusSkipped {
			t.Errorf("install-config = %s, want skip when no ConfigSource is given", res.Status)
		}
	}
}
