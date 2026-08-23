package rollback

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/backup"
)

// writeTree creates a small directory tree and returns its path, standing
// in for a Stalwart data directory.
func writeTree(t *testing.T, root string, files map[string]string) string {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPreserveFailedStateMovesRatherThanDeletes(t *testing.T) {
	dir := t.TempDir()
	dataDir := writeTree(t, filepath.Join(dir, "data"), map[string]string{"blobs/a": "half-migrated"})

	preserved, moved, err := PreserveFailedState(dataDir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !moved {
		t.Error("moved = false, want true")
	}
	if want := dataDir + ".failed-run-1"; preserved != want {
		t.Errorf("preserved path = %q, want %q", preserved, want)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Errorf("original path still exists after being moved aside: %v", err)
	}
	if got := readFile(t, filepath.Join(preserved, "blobs/a")); got != "half-migrated" {
		t.Errorf("preserved content = %q, want the failed attempt's data intact", got)
	}
}

// A resumed rollback re-runs steps that were interrupted. If this clobbered
// the earlier rescue with whatever is at the original path the second time
// around, the failed attempt's state - the thing §4.8 promises never to
// delete - would be lost precisely when a rollback got interrupted.
func TestPreserveFailedStateDoesNotClobberAnEarlierRescue(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	writeTree(t, dataDir, map[string]string{"a": "first"})
	if _, _, err := PreserveFailedState(dataDir, "run-1"); err != nil {
		t.Fatal(err)
	}
	writeTree(t, dataDir, map[string]string{"a": "second"})

	preserved, moved, err := PreserveFailedState(dataDir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if moved {
		t.Error("moved = true on the second call, want false")
	}
	if got := readFile(t, filepath.Join(preserved, "a")); got != "first" {
		t.Errorf("preserved content = %q, want %q - the first rescue must survive", got, "first")
	}
}

func TestPreserveFailedStateIsFineWhenThereIsNothingToPreserve(t *testing.T) {
	preserved, moved, err := PreserveFailedState(filepath.Join(t.TempDir(), "absent"), "run-1")
	if err != nil {
		t.Fatalf("want no error when the path doesn't exist, got %v", err)
	}
	if moved {
		t.Error("moved = true, want false")
	}
	if preserved == "" {
		t.Error("preserved path should still be reported")
	}
}

func TestRestoreDataDirRestoresAndReverifies(t *testing.T) {
	dir := t.TempDir()
	src := writeTree(t, filepath.Join(dir, "data"), map[string]string{
		"config": "settings", "blobs/one": "hello", "blobs/two": "world",
	})
	backupDir := filepath.Join(dir, "backup")
	manifest, err := backup.CopyDataDir(src, backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}

	if err := RestoreDataDir(backupDir, src, manifest); err != nil {
		t.Fatalf("RestoreDataDir: %v", err)
	}
	for rel, want := range map[string]string{"config": "settings", "blobs/one": "hello", "blobs/two": "world"} {
		if got := readFile(t, filepath.Join(src, rel)); got != want {
			t.Errorf("restored %s = %q, want %q", rel, got, want)
		}
	}
}

// A restore that put back corrupt bytes and reported success would be worse
// than one that failed: the operator would believe the rollback worked and
// only find out when Stalwart couldn't open its store.
func TestRestoreDataDirFailsWhenTheBackupNoLongerMatchesItsManifest(t *testing.T) {
	dir := t.TempDir()
	src := writeTree(t, filepath.Join(dir, "data"), map[string]string{"blobs/one": "hello"})
	backupDir := filepath.Join(dir, "backup")
	manifest, err := backup.CopyDataDir(src, backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "blobs/one"), []byte("corrupted"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}

	err = RestoreDataDir(backupDir, src, manifest)
	if err == nil {
		t.Fatal("RestoreDataDir: want error for a backup that no longer matches its manifest, got nil")
	}
	if !strings.Contains(err.Error(), "doesn't match the backup manifest") {
		t.Errorf("error %q should say the restored directory didn't match the manifest", err)
	}
}

func TestRestoreBinaryReinstallsOldAndPreservesCurrent(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "stalwart")
	preserved := binaryPath + ".v0.15.5"
	if err := os.WriteFile(preserved, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum, _, err := hashFile(preserved)
	if err != nil {
		t.Fatal(err)
	}

	displaced, err := RestoreBinary(preserved, binaryPath, sum, "run-1")
	if err != nil {
		t.Fatalf("RestoreBinary: %v", err)
	}
	if got := readFile(t, binaryPath); got != "old binary" {
		t.Errorf("binary at %s = %q, want the old one back", binaryPath, got)
	}
	if got := readFile(t, displaced); got != "new binary" {
		t.Errorf("displaced binary = %q, want the new one preserved for a retry", got)
	}
}

func TestRestoreBinaryRefusesAChangedBinary(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "stalwart")
	preserved := binaryPath + ".v0.15.5"
	if err := os.WriteFile(preserved, []byte("swapped out from under us"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := RestoreBinary(preserved, binaryPath, "0000000000000000000000000000000000000000000000000000000000000000", "run-1")
	if err == nil {
		t.Fatal("RestoreBinary: want error when the preserved binary's checksum doesn't match, got nil")
	}
	if _, statErr := os.Stat(binaryPath); !os.IsNotExist(statErr) {
		t.Error("a binary failing its checksum must not be installed")
	}
}

func TestRestoreFileKeepsDestinationPermissions(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "stalwart.service.preserved")
	dst := filepath.Join(dir, "stalwart.service")
	if err := os.WriteFile(src, []byte("[Service]\nExecStart=/usr/local/bin/stalwart\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("[Service]\nExecStart=/usr/local/bin/stalwart-new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RestoreFile(src, dst); err != nil {
		t.Fatalf("RestoreFile: %v", err)
	}
	if got := readFile(t, dst); !strings.Contains(got, "/usr/local/bin/stalwart\n") {
		t.Errorf("restored unit = %q, want the preserved one", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("restored unit mode = %v, want 0644 (the destination's own mode)", info.Mode().Perm())
	}
	// The temp file it writes through must not be left behind next to a
	// service definition directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("directory has %d entries, want 2 - a temp file was left behind", len(entries))
	}
}

// psql's default is to report an error and keep going, which would let a
// half-applied restore exit zero and be reported as a successful rollback.
func TestBuildPsqlArgsStopsOnError(t *testing.T) {
	args := BuildPsqlArgs(backup.SQLOptions{User: "stalwart", Database: "mail", Host: "db", Port: "5432", OutPath: "/tmp/dump.sql"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-v ON_ERROR_STOP=1") {
		t.Errorf("psql args %q must set ON_ERROR_STOP=1", joined)
	}
	for _, want := range []string{"-U stalwart", "-d mail", "-h db", "-p 5432", "-f /tmp/dump.sql"} {
		if !strings.Contains(joined, want) {
			t.Errorf("psql args %q missing %q", joined, want)
		}
	}
}

func TestBuildMySQLRestoreArgsOmitsUnsetConnectionFields(t *testing.T) {
	args := BuildMySQLRestoreArgs(backup.SQLOptions{User: "stalwart", Database: "mail"})
	if got, want := strings.Join(args, " "), "-u stalwart mail"; got != want {
		t.Errorf("mysql args = %q, want %q", got, want)
	}
}

func TestRunPsqlRestorePassesPasswordViaEnvironmentNotArgv(t *testing.T) {
	dir := t.TempDir()
	log := argsFile(t, dir)
	withFakeExecutable(t, "psql", fakeScriptLoggingArgs(log, "echo \"PGPASSWORD=$PGPASSWORD\" >> "+log))

	err := RunPsqlRestore(context.Background(), backup.SQLOptions{
		User: "stalwart", Database: "mail", Password: "hunter2", OutPath: filepath.Join(dir, "dump.sql"),
	})
	if err != nil {
		t.Fatal(err)
	}
	logged := readFile(t, log)
	if !strings.Contains(logged, "PGPASSWORD=hunter2") {
		t.Errorf("psql invocation %q should receive the password via PGPASSWORD", logged)
	}
	argv := strings.SplitN(logged, "\n", 2)[0]
	if strings.Contains(argv, "hunter2") {
		t.Errorf("psql argv %q contains the password - anything running `ps` could read it", argv)
	}
}

func TestRunPsqlRestoreSurfacesFailureOutput(t *testing.T) {
	dir := t.TempDir()
	withFakeExecutable(t, "psql", "#!/bin/sh\necho 'ERROR: relation \"s\" already exists' >&2\nexit 1\n")
	err := RunPsqlRestore(context.Background(), backup.SQLOptions{User: "u", Database: "d", OutPath: filepath.Join(dir, "dump.sql")})
	if err == nil {
		t.Fatal("want error when psql fails, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error %q should carry psql's own output", err)
	}
}

func TestRunMySQLRestoreFeedsTheDumpOnStdin(t *testing.T) {
	dir := t.TempDir()
	log := argsFile(t, dir)
	dump := filepath.Join(dir, "dump.sql")
	if err := os.WriteFile(dump, []byte("INSERT INTO s VALUES (1);\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	withFakeExecutable(t, "mysql", fakeScriptLoggingArgs(log, "cat >> "+log))

	err := RunMySQLRestore(context.Background(), backup.SQLOptions{
		User: "stalwart", Database: "mail", Password: "hunter2", OutPath: dump,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, log); !strings.Contains(got, "INSERT INTO s VALUES (1);") {
		t.Errorf("mysql stdin = %q, want the dump's contents", got)
	}
}

func TestRunMySQLRestoreFailsOnMissingDump(t *testing.T) {
	withFakeExecutable(t, "mysql", "#!/bin/sh\nexit 0\n")
	err := RunMySQLRestore(context.Background(), backup.SQLOptions{User: "u", Database: "d", OutPath: filepath.Join(t.TempDir(), "absent.sql")})
	if err == nil {
		t.Fatal("want error when the dump file doesn't exist, got nil")
	}
}
