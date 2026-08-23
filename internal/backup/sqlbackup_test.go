package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildPgDumpArgs(t *testing.T) {
	args := BuildPgDumpArgs(SQLOptions{
		Host: "db.internal", Port: "5432", Database: "stalwart", User: "stalwart", OutPath: "/backups/out.sql",
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{"-U stalwart", "-d stalwart", "-h db.internal", "-p 5432", "-f /backups/out.sql"} {
		if !strings.Contains(joined, want) {
			t.Errorf("BuildPgDumpArgs = %q, missing %q", joined, want)
		}
	}
	for _, table := range criticalTables {
		if !strings.Contains(joined, "-t "+table) {
			t.Errorf("BuildPgDumpArgs missing critical table flag for %q: %q", table, joined)
		}
	}
}

func TestBuildMySQLDumpArgs(t *testing.T) {
	args := BuildMySQLDumpArgs(SQLOptions{Host: "db.internal", Port: "3306", Database: "stalwart", User: "stalwart"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-u stalwart") || !strings.Contains(joined, "-h db.internal") || !strings.Contains(joined, "-P 3306") {
		t.Errorf("BuildMySQLDumpArgs = %q, missing expected flags", joined)
	}
	if !strings.HasSuffix(joined, strings.Join(criticalTables, " ")) {
		t.Errorf("BuildMySQLDumpArgs = %q, want it to end with the critical table list", joined)
	}
}

func TestRunPgDumpInvokesPgDumpWithArgs(t *testing.T) {
	dir := t.TempDir()
	log := argsFile(t, dir)
	withFakeExecutable(t, "pg_dump", fakeScriptLoggingArgs(log, "exit 0"))

	err := RunPgDump(context.Background(), SQLOptions{
		User: "stalwart", Database: "stalwart", OutPath: filepath.Join(dir, "out.sql"),
	})
	if err != nil {
		t.Fatalf("RunPgDump: %v", err)
	}
	got := readArgsFile(t, log)
	if !strings.Contains(got, "-U stalwart") {
		t.Errorf("pg_dump was invoked with %q, missing -U stalwart", got)
	}
}

func TestRunPgDumpPropagatesFailure(t *testing.T) {
	dir := t.TempDir()
	withFakeExecutable(t, "pg_dump", "#!/bin/sh\necho 'connection refused' >&2\nexit 1\n")

	err := RunPgDump(context.Background(), SQLOptions{User: "stalwart", Database: "stalwart", OutPath: filepath.Join(dir, "out.sql")})
	if err == nil {
		t.Fatal("RunPgDump should have returned an error when pg_dump exits non-zero")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error = %v, want it to include pg_dump's stderr", err)
	}
}

func TestRunMySQLDumpWritesStdoutToOutPath(t *testing.T) {
	dir := t.TempDir()
	withFakeExecutable(t, "mysqldump", "#!/bin/sh\necho '-- fake dump output'\n")

	outPath := filepath.Join(dir, "out.sql")
	err := RunMySQLDump(context.Background(), SQLOptions{User: "stalwart", Database: "stalwart", OutPath: outPath})
	if err != nil {
		t.Fatalf("RunMySQLDump: %v", err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "fake dump output") {
		t.Errorf("out.sql = %q, want fake dump output redirected into it", data)
	}
}
