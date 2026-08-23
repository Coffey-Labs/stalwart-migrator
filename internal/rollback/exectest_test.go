package rollback

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// withFakeExecutable puts a fake executable named `name` at the front of
// PATH for the duration of the test, so code that shells out to a
// real-world tool (psql, mysql, the stalwart binary's --version) can be
// exercised without that tool being installed. t.Setenv restores PATH
// automatically and marks the test non-parallel.
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

func fakeScriptLoggingArgs(logPath string, extraBody string) string {
	return fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\n%s\n", logPath, extraBody)
}

// fakeController stands in for a real systemd unit or Docker container, so
// the orchestration in Run can be tested without either. It records the
// order it was called in - which is the thing that actually matters here:
// restoring a data directory while the service is still running would be a
// silent corruption bug, and call order is how that's caught.
type fakeController struct {
	calls    []string
	active   bool
	stopErr  error
	startErr error

	// onStop runs after a successful Stop, letting a test assert on the
	// state of the world at the exact moment the service went down.
	onStop func()
}

func (f *fakeController) Stop(context.Context) error {
	f.calls = append(f.calls, "stop")
	if f.stopErr != nil {
		return f.stopErr
	}
	f.active = false
	if f.onStop != nil {
		f.onStop()
	}
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
