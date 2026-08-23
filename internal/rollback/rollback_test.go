package rollback

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/backup"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/service"
)

// fsRun builds a checkpoint that looks like a real embedded-backend run
// that got as far as taking (and recording) a verified filesystem backup:
// the state a rollback is actually invoked against.
func fsRun(t *testing.T) (store *checkpoint.Store, rs *checkpoint.RunState, dataDir, backupDir string) {
	t.Helper()
	root := t.TempDir()
	dataDir = writeTree(t, filepath.Join(root, "data"), map[string]string{
		"config": "original settings", "blobs/one": "original mail",
	})
	backupDir = filepath.Join(root, "backup")

	store = checkpoint.NewStore(filepath.Join(root, "runs"))
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatal(err)
	}
	rs.Topology = checkpoint.Topology{DeploymentKind: "systemd", StoreBackend: "rocksdb"}

	manifest, err := backup.CopyDataDir(dataDir, backupDir)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "backup.manifest.json")
	if err := backup.WriteManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	sum, err := manifest.Checksum()
	if err != nil {
		t.Fatal(err)
	}
	rs.RecordArtifact(ArtifactFSBackup, checkpoint.Artifact{Path: backupDir, SHA256: sum, SizeBytes: manifest.TotalBytes})
	if _, err := store.RunStep(rs, checkpoint.PhaseBackup, "fs-snapshot", func() (checkpoint.StepOutcome, error) {
		return checkpoint.StepOutcome{Detail: "copied", Extra: manifestPath}, nil
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate the half-migrated state a failed cutover leaves behind.
	if err := os.WriteFile(filepath.Join(dataDir, "config"), []byte("half-migrated settings"), 0o640); err != nil {
		t.Fatal(err)
	}
	return store, rs, dataDir, backupDir
}

func TestBuildPlanRefusesOnceTheRollbackWindowIsClosed(t *testing.T) {
	_, rs, dataDir, _ := fsRun(t)
	rs.RollbackWindowClosed = true

	_, err := BuildPlan(rs, Options{DataDir: dataDir, Controller: &fakeController{}})
	if err == nil {
		t.Fatal("BuildPlan: want refusal once the operator has confirmed the migration, got nil")
	}
	if !strings.Contains(err.Error(), "rollback window closed") {
		t.Errorf("error %q should say why it refuses", err)
	}
}

func TestBuildPlanRefusesWhatItCannotRestore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend string
		wantIn  string
	}{
		{"foundationdb", "foundationdb", "fdbrestore"},
		{"unrecognized", "", "no recognized store backend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rs, dataDir, _ := fsRun(t)
			rs.Topology.StoreBackend = tc.backend
			_, err := BuildPlan(rs, Options{DataDir: dataDir, Controller: &fakeController{}})
			if err == nil {
				t.Fatalf("BuildPlan for backend %q: want refusal, got nil", tc.backend)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q should mention %q", err, tc.wantIn)
			}
		})
	}
}

func TestBuildPlanRefusesWhenNoBackupWasEverRecorded(t *testing.T) {
	_, rs, dataDir, _ := fsRun(t)
	delete(rs.Artifacts, ArtifactFSBackup)

	_, err := BuildPlan(rs, Options{DataDir: dataDir, Controller: &fakeController{}})
	if err == nil {
		t.Fatal("BuildPlan: want refusal when there's no backup to restore, got nil")
	}
	if !strings.Contains(err.Error(), "cannot be rolled back") {
		t.Errorf("error %q should say the run can't be rolled back by this tool", err)
	}
}

func TestBuildPlanRefusesAnUnknownDeploymentKind(t *testing.T) {
	_, rs, dataDir, _ := fsRun(t)
	rs.Topology.DeploymentKind = string(service.Unknown)

	_, err := BuildPlan(rs, Options{DataDir: dataDir})
	if err == nil {
		t.Fatal("BuildPlan: want refusal when it doesn't know how to stop Stalwart, got nil")
	}
}

func TestBuildPlanFillsPathsFromTheCheckpoint(t *testing.T) {
	_, rs, dataDir, backupDir := fsRun(t)
	rs.RecordArtifact(ArtifactOldBinary, checkpoint.Artifact{Path: "/usr/local/bin/stalwart.v0.15.5", SHA256: "abc"})

	plan, err := BuildPlan(rs, Options{
		DataDir: dataDir, BinaryPath: "/usr/local/bin/stalwart", Controller: &fakeController{},
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if plan.Method != "filesystem" {
		t.Errorf("Method = %q, want filesystem", plan.Method)
	}
	if plan.BackupDir != backupDir {
		t.Errorf("BackupDir = %q, want the recorded artifact %q", plan.BackupDir, backupDir)
	}
	if plan.ManifestPath == "" {
		t.Error("ManifestPath should come from the fs-snapshot step's recorded outcome")
	}
	if plan.PreservedBinary != "/usr/local/bin/stalwart.v0.15.5" {
		t.Errorf("PreservedBinary = %q, want the recorded artifact", plan.PreservedBinary)
	}
	if !strings.Contains(plan.String(), "nothing is deleted") {
		t.Errorf("plan text should tell the operator nothing is deleted:\n%s", plan)
	}
}

func TestRunRestoresTheDataDirectoryWhileTheServiceIsDown(t *testing.T) {
	store, rs, dataDir, _ := fsRun(t)
	var configAtStop string
	ctl := &fakeController{active: true}
	ctl.onStop = func() { configAtStop = readFile(t, filepath.Join(dataDir, "config")) }

	report, err := Run(context.Background(), store, rs, Options{DataDir: dataDir, Controller: ctl})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, report)
	}

	if got, want := strings.Join(ctl.calls, ","), "stop,start"; got != want {
		t.Errorf("controller calls = %q, want %q", got, want)
	}
	if configAtStop != "half-migrated settings" {
		t.Errorf("data was already touched when the service stopped (config=%q) - the restore must happen after the stop", configAtStop)
	}
	if got := readFile(t, filepath.Join(dataDir, "config")); got != "original settings" {
		t.Errorf("restored config = %q, want the pre-migration contents", got)
	}
	if got := readFile(t, filepath.Join(dataDir, "blobs/one")); got != "original mail" {
		t.Errorf("restored mail = %q, want the pre-migration contents", got)
	}
	if got := readFile(t, filepath.Join(dataDir+".failed-"+rs.RunID, "config")); got != "half-migrated settings" {
		t.Errorf("failed attempt's data = %q, want it preserved rather than deleted", got)
	}
	if report.Blocking() {
		t.Errorf("report should be clean:\n%s", report)
	}
}

// Discovering a corrupt backup is survivable while the failed instance is
// still up, and unsurvivable once its data directory has been moved aside.
func TestRunVerifiesTheBackupBeforeStoppingAnything(t *testing.T) {
	store, rs, dataDir, backupDir := fsRun(t)
	if err := os.WriteFile(filepath.Join(backupDir, "blobs/one"), []byte("corrupt"), 0o640); err != nil {
		t.Fatal(err)
	}
	ctl := &fakeController{active: true}

	report, err := Run(context.Background(), store, rs, Options{DataDir: dataDir, Controller: ctl})
	if err == nil {
		t.Fatal("Run: want failure for a corrupt backup, got nil")
	}
	if len(ctl.calls) != 0 {
		t.Errorf("controller was called %v - the service must not be stopped when the backup can't be trusted", ctl.calls)
	}
	if got := readFile(t, filepath.Join(dataDir, "config")); got != "half-migrated settings" {
		t.Errorf("data directory was modified (%q) despite the refusal", got)
	}
	if !report.Blocking() {
		t.Error("report should be blocking")
	}
}

func TestRunResumesWithoutRedoingCompletedSteps(t *testing.T) {
	store, rs, dataDir, _ := fsRun(t)
	first := &fakeController{active: true}
	if _, err := Run(context.Background(), store, rs, Options{DataDir: dataDir, Controller: first}); err != nil {
		t.Fatal(err)
	}

	// Re-invoking a completed rollback must be inert: stopping the service
	// a second time, or re-restoring over a data directory that has been
	// live since, would turn a no-op into an outage.
	second := &fakeController{active: true}
	reloaded, err := store.Load(rs.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), store, reloaded, Options{DataDir: dataDir, Controller: second}); err != nil {
		t.Fatal(err)
	}
	if len(second.calls) != 0 {
		t.Errorf("second invocation called the controller %v, want nothing - every step was already done", second.calls)
	}
}

func TestRunReinstallsThePreservedBinary(t *testing.T) {
	store, rs, dataDir, _ := fsRun(t)
	binDir := t.TempDir()
	binaryPath := filepath.Join(binDir, "stalwart")
	preserved := binaryPath + ".v0.15.5"
	// Real scripts, not placeholder bytes: the verification step runs the
	// restored binary's --version, so this also proves the instance really
	// came back on the version the run started from.
	if err := os.WriteFile(preserved, []byte("#!/bin/sh\necho 'stalwart 0.15.5'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\necho 'stalwart 0.16.14'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum, _, err := hashFile(preserved)
	if err != nil {
		t.Fatal(err)
	}
	rs.RecordArtifact(ArtifactOldBinary, checkpoint.Artifact{Path: preserved, SHA256: sum})

	report, err := Run(context.Background(), store, rs, Options{
		DataDir: dataDir, BinaryPath: binaryPath, Controller: &fakeController{active: true},
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, report)
	}
	if got := readFile(t, binaryPath); !strings.Contains(got, "0.15.5") {
		t.Errorf("binary = %q, want the preserved old one reinstalled", got)
	}
	if got := readFile(t, binaryPath+".failed-"+rs.RunID); !strings.Contains(got, "0.16.14") {
		t.Errorf("displaced binary = %q, want the new one kept for a retry", got)
	}
}

func TestRunRestoresAPreservedServiceUnit(t *testing.T) {
	store, rs, dataDir, _ := fsRun(t)
	unitDir := t.TempDir()
	unitPath := filepath.Join(unitDir, "stalwart.service")
	preservedUnit := filepath.Join(unitDir, "stalwart.service.preserved")
	if err := os.WriteFile(preservedUnit, []byte("ExecStart=/usr/local/bin/stalwart\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte("ExecStart=/usr/local/bin/stalwart-0.16\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rs.RecordArtifact(ArtifactServiceUnit, checkpoint.Artifact{Path: preservedUnit})

	ctl := &fakeController{active: true}
	report, err := Run(context.Background(), store, rs, Options{
		DataDir: dataDir, ServiceUnitPath: unitPath, Controller: ctl,
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, report)
	}
	if got := readFile(t, unitPath); !strings.Contains(got, "/usr/local/bin/stalwart\n") {
		t.Errorf("unit = %q, want the preserved definition restored", got)
	}
	if got, want := strings.Join(ctl.calls, ","), "stop,reload,start"; got != want {
		t.Errorf("controller calls = %q, want %q - a restored unit has to be reloaded before the start", got, want)
	}
}

// Nothing writes a service-unit artifact yet (cutover, which would rewrite
// the unit in the first place, doesn't exist). That has to read as an
// explicit skip, not a silent success.
func TestRunSkipsServiceConfigRestoreWithAnExplanation(t *testing.T) {
	store, rs, dataDir, _ := fsRun(t)
	report, err := Run(context.Background(), store, rs, Options{DataDir: dataDir, Controller: &fakeController{active: true}})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, res := range report.Results {
		if res.Name == "restore-service-config" {
			found = true
			if res.Status != StatusSkipped {
				t.Errorf("restore-service-config status = %q, want %q", res.Status, StatusSkipped)
			}
			if !strings.Contains(res.Detail, "cutover") {
				t.Errorf("skip detail %q should say what would record one", res.Detail)
			}
		}
	}
	if !found {
		t.Errorf("no restore-service-config result in report:\n%s", report)
	}
}

func TestRunFailsWhenTheServiceWontStop(t *testing.T) {
	store, rs, dataDir, _ := fsRun(t)
	ctl := &fakeController{active: true, stopErr: os.ErrPermission}

	report, err := Run(context.Background(), store, rs, Options{DataDir: dataDir, Controller: ctl})
	if err == nil {
		t.Fatal("Run: want failure when the service can't be stopped, got nil")
	}
	if got := readFile(t, filepath.Join(dataDir, "config")); got != "half-migrated settings" {
		t.Errorf("data directory was touched (%q) even though the service never stopped", got)
	}
	if !report.Blocking() {
		t.Error("report should be blocking")
	}
}

func TestRunSurfacesVerificationFailures(t *testing.T) {
	store, rs, dataDir, _ := fsRun(t)
	binDir := withFakeExecutable(t, "stalwart", "#!/bin/sh\necho 'stalwart 0.16.14'\n")

	report, err := Run(context.Background(), store, rs, Options{
		DataDir: dataDir, BinaryPath: filepath.Join(binDir, "stalwart"), Controller: &fakeController{active: true},
	})
	if err == nil {
		t.Fatal("Run: want failure when the restored instance isn't on the original version, got nil")
	}
	var sawVersionFailure bool
	for _, res := range report.Results {
		if res.Name == "version" && res.Status == StatusFail {
			sawVersionFailure = true
			if !strings.Contains(res.Detail, "0.15.5") {
				t.Errorf("version failure %q should name the version it expected", res.Detail)
			}
		}
	}
	if !sawVersionFailure {
		t.Errorf("individual verification results should be in the report, not collapsed into one line:\n%s", report)
	}
}
