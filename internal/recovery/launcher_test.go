// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package recovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
)

// recordingLauncher stands in for a deployment that is not a local binary -
// a container, when that lands. It records what it was asked for and
// delegates the actual process to BinaryLauncher, so the recovery cycle
// still has something real to talk to.
type recordingLauncher struct {
	inner  Launcher
	calls  int
	lastOp LaunchOptions
}

func (r *recordingLauncher) Launch(ctx context.Context, o LaunchOptions) (Supervised, error) {
	r.calls++
	r.lastOp = o
	return r.inner.Launch(ctx, o)
}

// The whole point of the seam: a deployment that is not a host binary can
// supply its own way of starting the target version, and everything the
// recovery cycle does afterwards is unchanged.
func TestRunUsesTheSuppliedLauncher(t *testing.T) {
	port := freePort(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	applyDir := t.TempDir()
	withFakeExecutable(t, "stalwart-cli", fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\nexit 0\n", argsFile(t, applyDir)))
	exportFile := filepath.Join(t.TempDir(), "export.json")
	if err := os.WriteFile(exportFile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatal(err)
	}

	launcher := &recordingLauncher{inner: BinaryLauncher{BinaryPath: testBinaryPath(t)}}
	_, err = Run(context.Background(), store, rs, Options{
		// Deliberately empty: a supplied Launcher owns how the target
		// version is started, so BinaryPath must not be consulted at all.
		BinaryPath:     "",
		ConfigPath:     configPath,
		ListenURL:      fmt.Sprintf("http://127.0.0.1:%d/", port),
		AdminUser:      "admin",
		ApplyFiles:     []string{exportFile},
		ExtraEnv:       helperProcessEnv(port),
		StartupTimeout: 5 * time.Second,
		StopGrace:      5 * time.Second,
		Launcher:       launcher,
	})
	if err != nil {
		t.Fatalf("Run with a supplied launcher: %v", err)
	}
	if launcher.calls != 1 {
		t.Errorf("launcher called %d times, want 1", launcher.calls)
	}
	if !launcher.lastOp.RecoveryMode {
		t.Error("recovery cycle launched without RecoveryMode set")
	}
	if launcher.lastOp.AdminPassword == "" {
		t.Error("recovery cycle launched without a generated admin password")
	}
	if launcher.lastOp.ConfigPath != configPath {
		t.Errorf("ConfigPath = %q, want %q", launcher.lastOp.ConfigPath, configPath)
	}
}

// A nil Launcher has to keep meaning exactly what it meant before this
// seam existed, or every existing caller changes behaviour silently.
func TestNilLauncherStillRunsTheBinaryPath(t *testing.T) {
	port := freePort(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	applyDir := t.TempDir()
	applyLog := argsFile(t, applyDir)
	withFakeExecutable(t, "stalwart-cli", fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\nexit 0\n", applyLog))
	exportFile := filepath.Join(t.TempDir(), "export.json")
	if err := os.WriteFile(exportFile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), store, rs, Options{
		BinaryPath:     testBinaryPath(t),
		ConfigPath:     configPath,
		ListenURL:      fmt.Sprintf("http://127.0.0.1:%d/", port),
		AdminUser:      "admin",
		ApplyFiles:     []string{exportFile},
		ExtraEnv:       helperProcessEnv(port),
		StartupTimeout: 5 * time.Second,
		StopGrace:      5 * time.Second,
		// Launcher deliberately unset.
	}); err != nil {
		t.Fatalf("Run with a nil launcher: %v", err)
	}
	if got := readArgsFile(t, applyLog); got == "" {
		t.Fatal("stalwart-cli apply was never invoked, so the cycle did not run")
	}
}
