package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// withFakeExecutable puts a fake executable named `name` at the front of
// PATH for the duration of the test, so code that shells out to a
// real-world tool (pg_dump, mysqldump, fdbbackup, vandelay, python3) can be
// exercised without that tool actually being installed. t.Setenv restores
// PATH automatically and marks the test non-parallel.
func withFakeExecutable(t *testing.T, name, script string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// argsFile is a shared convention the fake scripts below use: they append
// their own argv (space-joined, one invocation per line) to a file so the
// test can assert on exactly what was passed.
func argsFile(t *testing.T, dir string) string {
	t.Helper()
	return filepath.Join(dir, "invoked-args.log")
}

func readArgsFile(t *testing.T, path string) string {
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

func fakeScriptLoggingArgs(logPath string, extraBody string) string {
	return fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\n%s\n", logPath, extraBody)
}
