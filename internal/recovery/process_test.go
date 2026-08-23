package recovery

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
