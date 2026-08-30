// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package service

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// withFakeExecutable puts a fake executable named `name` at the front of
// PATH for the duration of the test, so code that shells out to a
// real-world tool (systemctl, docker) can be exercised without stopping
// anything real. t.Setenv restores PATH automatically and marks the test
// non-parallel.
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
