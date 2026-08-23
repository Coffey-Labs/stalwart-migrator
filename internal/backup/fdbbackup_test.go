package backup

import (
	"context"
	"strings"
	"testing"
)

func TestBuildFDBBackupStartArgs(t *testing.T) {
	args := BuildFDBBackupStartArgs(FDBOptions{ClusterFile: "/etc/foundationdb/fdb.cluster", Destination: "file:///var/backups/stalwart-fdb", Tag: "stalwart-migrate"})
	joined := strings.Join(args, " ")
	for _, want := range []string{"start", "-C /etc/foundationdb/fdb.cluster", "-d file:///var/backups/stalwart-fdb", "-t stalwart-migrate"} {
		if !strings.Contains(joined, want) {
			t.Errorf("BuildFDBBackupStartArgs = %q, missing %q", joined, want)
		}
	}
}

func TestBuildFDBBackupStartArgsDefaultTag(t *testing.T) {
	args := BuildFDBBackupStartArgs(FDBOptions{Destination: "file:///var/backups/stalwart-fdb"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-t default") {
		t.Errorf("BuildFDBBackupStartArgs = %q, want default tag when none given", joined)
	}
}

func TestStartFDBBackupInvokesFdbbackup(t *testing.T) {
	dir := t.TempDir()
	log := argsFile(t, dir)
	withFakeExecutable(t, "fdbbackup", fakeScriptLoggingArgs(log, "exit 0"))

	err := StartFDBBackup(context.Background(), FDBOptions{Destination: "file:///var/backups/stalwart-fdb"})
	if err != nil {
		t.Fatalf("StartFDBBackup: %v", err)
	}
	got := readArgsFile(t, log)
	if !strings.Contains(got, "start") || !strings.Contains(got, "-d file:///var/backups/stalwart-fdb") {
		t.Errorf("fdbbackup was invoked with %q", got)
	}
}

func TestStartFDBBackupPropagatesFailure(t *testing.T) {
	withFakeExecutable(t, "fdbbackup", "#!/bin/sh\necho 'cluster unreachable' >&2\nexit 1\n")
	err := StartFDBBackup(context.Background(), FDBOptions{Destination: "file:///var/backups/stalwart-fdb"})
	if err == nil {
		t.Fatal("StartFDBBackup should error when fdbbackup exits non-zero")
	}
}
