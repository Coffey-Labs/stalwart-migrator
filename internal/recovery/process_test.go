// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package recovery

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// freePort asks the OS for an unused TCP port by binding to :0 and
// immediately releasing it. There's a small window where something else
// could grab it before the fake server binds, but that's an acceptable,
// standard tradeoff for tests.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func helperProcessEnv(port int, extra ...string) []string {
	env := []string{
		"STALWART_MIGRATOR_TEST_HELPER=1",
		fmt.Sprintf("STALWART_MIGRATOR_TEST_PORT=%d", port),
	}
	return append(env, extra...)
}

func testBinaryPath(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

func TestProcessStartWaitHealthyStop(t *testing.T) {
	port := freePort(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(configPath, []byte("{}"), 0o644)

	proc := &Process{}
	err := proc.Start(context.Background(), ProcessOptions{
		BinaryPath: testBinaryPath(t),
		ConfigPath: configPath,
		ExtraEnv:   helperProcessEnv(port),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	if err := WaitForHealthy(context.Background(), nil, url, 5*time.Second); err != nil {
		t.Fatalf("WaitForHealthy: %v", err)
	}

	if err := proc.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestProcessStopEscalatesToSIGKILL(t *testing.T) {
	port := freePort(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(configPath, []byte("{}"), 0o644)

	proc := &Process{}
	err := proc.Start(context.Background(), ProcessOptions{
		BinaryPath: testBinaryPath(t),
		ConfigPath: configPath,
		ExtraEnv:   helperProcessEnv(port, "STALWART_MIGRATOR_TEST_IGNORE_SIGTERM=1"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	if err := WaitForHealthy(context.Background(), nil, url, 5*time.Second); err != nil {
		t.Fatalf("WaitForHealthy: %v", err)
	}

	err = proc.Stop(500 * time.Millisecond)
	if err == nil {
		t.Fatal("Stop should report an error when it had to escalate to SIGKILL")
	}
}

func TestWaitForHealthyTimesOut(t *testing.T) {
	// Nothing listens on this port.
	port := freePort(t)
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	err := WaitForHealthy(context.Background(), nil, url, 500*time.Millisecond)
	if err == nil {
		t.Fatal("WaitForHealthy should time out when nothing is listening")
	}
}

// The bug this guards against cost several rounds of diagnosis against a
// real Stalwart: recovery mode failed because port 8080 was already in use,
// Stalwart said exactly that immediately, and the tool discarded it and
// reported only a 60-second timeout and "connection refused". The helper
// process here fails the same way - it can't bind its port and says so on
// stderr before exiting.
func TestProcessCapturesOutputFromAFailedStart(t *testing.T) {
	// Occupy the port first, so the child's bind fails exactly as
	// Stalwart's did.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)

	proc := &Process{}
	if err := proc.Start(context.Background(), ProcessOptions{
		BinaryPath: os.Args[0],
		ExtraEnv: []string{
			"STALWART_MIGRATOR_TEST_HELPER=1",
			"STALWART_MIGRATOR_TEST_PORT=" + port,
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Poll a port nothing is listening on, exactly as the real flow does:
	// the tool waits for a listener that never appears while the child is
	// busy failing and saying why.
	err = WaitForHealthy(context.Background(), nil, "http://127.0.0.1:1/", time.Second)
	if err == nil {
		t.Fatal("WaitForHealthy should not have succeeded against an unreachable URL")
	}
	_ = proc.Stop(5 * time.Second)

	got := proc.Output()
	if !strings.Contains(got, "fake stalwart: listen:") {
		t.Errorf("captured output = %q, want the child's own bind failure - without it a caller can only report a timeout", got)
	}
}

func TestProcessOutputIsSafeBeforeStartAndAfterStop(t *testing.T) {
	proc := &Process{}
	if got := proc.Output(); got != "" {
		t.Errorf("Output() before Start = %q, want empty", got)
	}
	if err := proc.Stop(time.Second); err != nil {
		t.Errorf("Stop on an unstarted process: %v", err)
	}
}

// A long-lived server would otherwise grow this buffer without bound.
func TestOutputBufferKeepsTheMostRecentOutput(t *testing.T) {
	b := &outputBuffer{}
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(b, "line %d filler filler filler filler filler\n", i)
	}
	got := b.String()
	if len(got) > maxCapturedOutput+64 {
		t.Errorf("buffer grew to %d bytes, want it bounded near %d", len(got), maxCapturedOutput)
	}
	if !strings.Contains(got, "line 3999") {
		t.Error("the most recent output was dropped; a failure's reason is at the end of the log")
	}
	if !strings.Contains(got, "truncated") {
		t.Error("truncation should be visible, not silent")
	}
}
