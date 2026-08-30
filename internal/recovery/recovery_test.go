// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package recovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Coffey-Labs/stalwart-migrator/internal/checkpoint"
)

func TestRecoveryRunEndToEndAndResume(t *testing.T) {
	port := freePort(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(configPath, []byte("{}"), 0o644)

	applyDir := t.TempDir()
	applyLog := argsFile(t, applyDir)
	withFakeExecutable(t, "stalwart-cli", fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\nexit 0\n", applyLog))

	exportFile := filepath.Join(t.TempDir(), "export.json")
	os.WriteFile(exportFile, []byte("{}"), 0o644)

	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	opts := Options{
		BinaryPath:     testBinaryPath(t),
		ConfigPath:     configPath,
		ListenURL:      fmt.Sprintf("http://127.0.0.1:%d/", port),
		AdminUser:      "admin",
		ApplyFiles:     []string{exportFile},
		ExtraEnv:       helperProcessEnv(port),
		StartupTimeout: 5 * time.Second,
		StopGrace:      5 * time.Second,
	}

	report, err := Run(context.Background(), store, rs, opts)
	if err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	if len(report.Results) != 1 || report.Results[0].Status != StatusOK {
		t.Fatalf("Run #1 report = %+v, want a single OK result", report.Results)
	}
	if !rs.Done(checkpoint.PhaseRecovery, "recovery-cycle") {
		t.Fatal("recovery-cycle should be marked done after a successful run")
	}
	if got := readArgsFile(t, applyLog); got == "" {
		t.Fatal("stalwart-cli apply was never invoked")
	}

	// -- resume: break the CLI so a re-invocation would fail loudly, and use
	// -- a port nothing listens on, so re-starting the process would time out.
	withFakeExecutable(t, "stalwart-cli", "#!/bin/sh\necho should-not-run-again >&2\nexit 1\n")
	badPort := freePort(t)
	resumedOpts := opts
	resumedOpts.ListenURL = fmt.Sprintf("http://127.0.0.1:%d/", badPort)
	resumedOpts.ExtraEnv = helperProcessEnv(badPort)
	resumedOpts.StartupTimeout = 300 * time.Millisecond

	resumed, err := store.Load(rs.RunID)
	if err != nil {
		t.Fatalf("store.Load (resume): %v", err)
	}
	report2, err := Run(context.Background(), store, resumed, resumedOpts)
	if err != nil {
		t.Fatalf("Run #2 (resume) should succeed without redoing the cycle: %v", err)
	}
	if len(report2.Results) != 1 || report2.Results[0].Detail != report.Results[0].Detail {
		t.Errorf("resumed report = %+v, want the cached outcome from Run #1", report2.Results)
	}
}

func TestRecoveryRunFailsWhenProcessNeverBecomesHealthy(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(configPath, []byte("{}"), 0o644)

	// Nothing listens on this port - the binary never actually starts an
	// HTTP server, since BinaryPath here is a script that just exits.
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "stalwart")
	os.WriteFile(binPath, []byte("#!/bin/sh\nsleep 5\n"), 0o755)
	port := freePort(t)

	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatal(err)
	}

	opts := Options{
		BinaryPath:     binPath,
		ConfigPath:     configPath,
		ListenURL:      fmt.Sprintf("http://127.0.0.1:%d/", port),
		AdminUser:      "admin",
		ApplyFiles:     []string{},
		StartupTimeout: 300 * time.Millisecond,
		StopGrace:      2 * time.Second,
	}

	_, err = Run(context.Background(), store, rs, opts)
	if err == nil {
		t.Fatal("Run should fail when the process never becomes healthy")
	}
	if rs.Status(checkpoint.PhaseRecovery, "recovery-cycle") != checkpoint.StepFailed {
		t.Errorf("step status = %s, want failed (so a retry is possible)", rs.Status(checkpoint.PhaseRecovery, "recovery-cycle"))
	}
}
