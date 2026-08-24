// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package recovery

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"testing"
)

// TestMain lets this test binary also act as a fake Stalwart binary for
// subprocess tests - the standard os/exec "helper process" technique (see
// Go's own os/exec_test.go). When STALWART_MIGRATOR_TEST_HELPER=1 is set,
// the binary runs a minimal HTTP server on STALWART_MIGRATOR_TEST_PORT
// until it receives SIGTERM (or, if STALWART_MIGRATOR_TEST_IGNORE_SIGTERM=1,
// ignores SIGTERM to exercise the SIGKILL escalation path) instead of
// running the actual test suite. This avoids needing a real Stalwart binary
// - or a fabricated stand-in for its HTTP behavior - anywhere in these
// tests: it's real net/http and real process signaling, just running under
// this package's own compiled binary.
func TestMain(m *testing.M) {
	if os.Getenv("STALWART_MIGRATOR_TEST_HELPER") == "1" {
		runFakeStalwartServer()
		return
	}
	os.Exit(m.Run())
}

func runFakeStalwartServer() {
	port := os.Getenv("STALWART_MIGRATOR_TEST_PORT")
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake stalwart: listen:", err)
		os.Exit(1)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go srv.Serve(ln)

	sigCh := make(chan os.Signal, 1)
	if os.Getenv("STALWART_MIGRATOR_TEST_IGNORE_SIGTERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
		select {} // block forever; the test must SIGKILL this process itself
	}
	signal.Notify(sigCh, syscall.SIGTERM)
	<-sigCh
	os.Exit(0)
}
