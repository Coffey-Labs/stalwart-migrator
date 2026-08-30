// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package validate

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func testBinaryPath(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

func TestBootCheckSucceedsWhenInstanceComesUp(t *testing.T) {
	port := freePort(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(configPath, []byte("{}"), 0o644)

	detail, result, err := BootCheck(context.Background(), BootCheckOptions{
		BinaryPath: testBinaryPath(t),
		ConfigPath: configPath,
		ListenURL:  fmt.Sprintf("http://127.0.0.1:%d/", port),
		ExtraEnv: []string{
			"STALWART_MIGRATOR_TEST_HELPER=1",
			fmt.Sprintf("STALWART_MIGRATOR_TEST_PORT=%d", port),
		},
		Timeout:   5 * time.Second,
		StopGrace: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("BootCheck: %v", err)
	}
	if detail == "" {
		t.Error("BootCheck returned an empty detail on success")
	}
	if result != nil {
		t.Errorf("result = %+v, want nil when ContentIntegrityBefore wasn't set", result)
	}
}

func TestBootCheckFailsWhenInstanceNeverComesUp(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(configPath, []byte("{}"), 0o644)
	binPath := filepath.Join(t.TempDir(), "stalwart")
	os.WriteFile(binPath, []byte("#!/bin/sh\nsleep 5\n"), 0o755)
	port := freePort(t)

	_, _, err := BootCheck(context.Background(), BootCheckOptions{
		BinaryPath: binPath,
		ConfigPath: configPath,
		ListenURL:  fmt.Sprintf("http://127.0.0.1:%d/", port),
		Timeout:    300 * time.Millisecond,
		StopGrace:  2 * time.Second,
	})
	if err == nil {
		t.Fatal("BootCheck should fail when nothing ever answers ListenURL")
	}
}

func TestRunEndToEndAndResume(t *testing.T) {
	port := freePort(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(configPath, []byte("{}"), 0o644)

	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatal(err)
	}

	opts := BootCheckOptions{
		BinaryPath: testBinaryPath(t),
		ConfigPath: configPath,
		ListenURL:  fmt.Sprintf("http://127.0.0.1:%d/", port),
		ExtraEnv: []string{
			"STALWART_MIGRATOR_TEST_HELPER=1",
			fmt.Sprintf("STALWART_MIGRATOR_TEST_PORT=%d", port),
		},
		Timeout:   5 * time.Second,
		StopGrace: 5 * time.Second,
	}

	report, err := Run(context.Background(), store, rs, opts)
	if err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	if report.Blocking() {
		t.Fatalf("Run #1: unexpected failure: %s", report.String())
	}

	// Resume with a config that would fail if re-executed (nothing listens
	// on badPort) - a skip proves the step didn't re-run.
	badPort := freePort(t)
	resumedOpts := opts
	resumedOpts.ListenURL = fmt.Sprintf("http://127.0.0.1:%d/", badPort)
	resumedOpts.Timeout = 300 * time.Millisecond

	resumed, err := store.Load(rs.RunID)
	if err != nil {
		t.Fatal(err)
	}
	report2, err := Run(context.Background(), store, resumed, resumedOpts)
	if err != nil {
		t.Fatalf("Run #2 (resume) should succeed without redoing the check: %v", err)
	}
	if report2.Blocking() {
		t.Fatalf("Run #2 (resume): unexpected failure: %s", report2.String())
	}
}

// beforeSnapshotWithAliceInbox builds a checkpoint.PreflightSnapshot
// matching the fake server's single hardcoded account (alice@example.com,
// mailbox "Inbox") with the given pre-migration message count.
func beforeSnapshotWithAliceInbox(messages int) *checkpoint.PreflightSnapshot {
	return &checkpoint.PreflightSnapshot{
		AccountCount: 1,
		Domains:      []string{"example.com"},
		MailboxCounts: map[string][]checkpoint.MailboxCount{
			"alice@example.com": {{Mailbox: "Inbox", Messages: messages}},
		},
	}
}

func TestBootCheckContentIntegrityPassesWhenCountsMatch(t *testing.T) {
	port := freePort(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(configPath, []byte("{}"), 0o644)

	detail, result, err := BootCheck(context.Background(), BootCheckOptions{
		BinaryPath: testBinaryPath(t),
		ConfigPath: configPath,
		ListenURL:  fmt.Sprintf("http://127.0.0.1:%d/", port),
		ExtraEnv: []string{
			"STALWART_MIGRATOR_TEST_HELPER=1",
			fmt.Sprintf("STALWART_MIGRATOR_TEST_PORT=%d", port),
			"STALWART_MIGRATOR_TEST_MAILBOX_COUNT=42", // matches beforeSnapshotWithAliceInbox(42)
		},
		Timeout:                5 * time.Second,
		StopGrace:              5 * time.Second,
		ContentIntegrityBefore: beforeSnapshotWithAliceInbox(42),
		AdminUser:              "admin",
		AdminPassword:          "hunter2",
	})
	if err != nil {
		t.Fatalf("BootCheck: %v", err)
	}
	if result == nil {
		t.Fatal("result should be populated when ContentIntegrityBefore was set")
	}
	if !result.OK() {
		t.Errorf("result.OK() = false, want true: %s", result.String())
	}
	if result.AccountsChecked != 1 || result.MailboxesChecked != 1 {
		t.Errorf("AccountsChecked=%d MailboxesChecked=%d, want 1 and 1", result.AccountsChecked, result.MailboxesChecked)
	}
	if detail == "" {
		t.Error("detail should still describe the boot")
	}
}

func TestBootCheckContentIntegrityFailsWhenCountsMismatch(t *testing.T) {
	port := freePort(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(configPath, []byte("{}"), 0o644)

	_, result, err := BootCheck(context.Background(), BootCheckOptions{
		BinaryPath: testBinaryPath(t),
		ConfigPath: configPath,
		ListenURL:  fmt.Sprintf("http://127.0.0.1:%d/", port),
		ExtraEnv: []string{
			"STALWART_MIGRATOR_TEST_HELPER=1",
			fmt.Sprintf("STALWART_MIGRATOR_TEST_PORT=%d", port),
			"STALWART_MIGRATOR_TEST_MAILBOX_COUNT=40", // the "after" server reports 40
		},
		Timeout:                5 * time.Second,
		StopGrace:              5 * time.Second,
		ContentIntegrityBefore: beforeSnapshotWithAliceInbox(42), // but "before" said 42 - two messages went missing
		AdminUser:              "admin",
		AdminPassword:          "hunter2",
	})
	if err == nil {
		t.Fatal("BootCheck should fail when a post-migration mailbox count doesn't match the pre-migration one")
	}
	if result == nil || result.OK() {
		t.Fatalf("result = %+v, want a non-OK result describing the mismatch", result)
	}
	if len(result.MessageCountMismatches) != 1 {
		t.Fatalf("MessageCountMismatches = %+v, want exactly one entry", result.MessageCountMismatches)
	}
	mismatch := result.MessageCountMismatches[0]
	if mismatch.Account != "alice@example.com" || mismatch.Mailbox != "Inbox" || mismatch.Before != 42 || mismatch.After != 40 {
		t.Errorf("mismatch = %+v, want alice@example.com/Inbox 42->40", mismatch)
	}
}

func TestBootCheckContentIntegrityDetectsMissingAccount(t *testing.T) {
	port := freePort(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(configPath, []byte("{}"), 0o644)

	before := &checkpoint.PreflightSnapshot{
		AccountCount: 2,
		Domains:      []string{"example.com", "example.net"},
		MailboxCounts: map[string][]checkpoint.MailboxCount{
			"alice@example.com": {{Mailbox: "Inbox", Messages: 42}},
			"carol@example.net": {{Mailbox: "Inbox", Messages: 5}}, // the fake server only ever knows about alice
		},
	}

	_, result, err := BootCheck(context.Background(), BootCheckOptions{
		BinaryPath: testBinaryPath(t),
		ConfigPath: configPath,
		ListenURL:  fmt.Sprintf("http://127.0.0.1:%d/", port),
		ExtraEnv: []string{
			"STALWART_MIGRATOR_TEST_HELPER=1",
			fmt.Sprintf("STALWART_MIGRATOR_TEST_PORT=%d", port),
			"STALWART_MIGRATOR_TEST_MAILBOX_COUNT=42",
		},
		Timeout:                5 * time.Second,
		StopGrace:              5 * time.Second,
		ContentIntegrityBefore: before,
		AdminUser:              "admin",
		AdminPassword:          "hunter2",
	})
	if err == nil {
		t.Fatal("BootCheck should fail when an account present before migration can't be found afterward")
	}
	if result == nil || len(result.MissingAccounts) != 1 || result.MissingAccounts[0] != "carol@example.net" {
		t.Fatalf("result = %+v, want MissingAccounts = [carol@example.net]", result)
	}
}
