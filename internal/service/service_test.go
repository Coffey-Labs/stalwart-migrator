// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNewRefusesUnknownKind(t *testing.T) {
	for _, kind := range []Kind{Unknown, "", "kubernetes"} {
		if _, err := New(Options{Kind: kind}); err == nil {
			t.Errorf("New(%q): want error, got nil - guessing how to stop Stalwart is exactly what this must not do", kind)
		}
	}
}

func TestNewDefaultsTargetNames(t *testing.T) {
	systemd, err := New(Options{Kind: Systemd})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := systemd.Target(), "systemd unit stalwart"; got != want {
		t.Errorf("Target() = %q, want %q", got, want)
	}
	docker, err := New(Options{Kind: Docker})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := docker.Target(), "docker container stalwart"; got != want {
		t.Errorf("Target() = %q, want %q", got, want)
	}
}

func TestSystemdStopStartReloadInvocations(t *testing.T) {
	dir := t.TempDir()
	log := argsFile(t, dir)
	withFakeExecutable(t, "systemctl", fakeScriptLoggingArgs(log, "exit 0"))

	c, err := New(Options{Kind: Systemd, UnitName: "stalwart-mail"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}

	want := "stop stalwart-mail\ndaemon-reload\nstart stalwart-mail\n"
	if got := readArgsFile(t, log); got != want {
		t.Errorf("systemctl invocations:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestSystemdStopReportsCommandFailure(t *testing.T) {
	withFakeExecutable(t, "systemctl", "#!/bin/sh\necho 'Failed to stop stalwart.service: Access denied' >&2\nexit 1\n")
	c, err := New(Options{Kind: Systemd})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Stop(context.Background())
	if err == nil {
		t.Fatal("Stop: want error when systemctl fails, got nil")
	}
	if !strings.Contains(err.Error(), "Access denied") {
		t.Errorf("Stop error %q does not carry systemctl's own output, which is the only clue an operator gets", err)
	}
}

// systemctl exits non-zero for every non-active state, so Active has to
// read its output rather than its exit status - otherwise "inactive", the
// answer a caller waiting for a clean stop most needs, would look like a
// failure to read the state at all.
func TestSystemdActiveReadsOutputNotExitStatus(t *testing.T) {
	for _, tc := range []struct {
		state    string
		exitCode int
		want     bool
	}{
		{"active", 0, true},
		{"activating", 3, true},
		{"reloading", 3, true},
		{"deactivating", 3, true}, // still holding the data directory open
		{"inactive", 3, false},
		{"failed", 3, false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			withFakeExecutable(t, "systemctl", fmt.Sprintf("#!/bin/sh\necho %s\nexit %d\n", tc.state, tc.exitCode))
			c, err := New(Options{Kind: Systemd})
			if err != nil {
				t.Fatal(err)
			}
			active, err := c.Active(context.Background())
			if err != nil {
				t.Fatalf("Active() for state %q: unexpected error %v", tc.state, err)
			}
			if active != tc.want {
				t.Errorf("Active() for state %q = %v, want %v", tc.state, active, tc.want)
			}
		})
	}
}

func TestSystemdActiveErrorsOnUnrecognizedState(t *testing.T) {
	withFakeExecutable(t, "systemctl", "#!/bin/sh\necho 'command not found'\nexit 127\n")
	c, err := New(Options{Kind: Systemd})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Active(context.Background()); err == nil {
		t.Error("Active: want error for unreadable state, got nil - an unreadable state must never collapse into 'not running'")
	}
}

func TestDockerStopStartInvocations(t *testing.T) {
	dir := t.TempDir()
	log := argsFile(t, dir)
	withFakeExecutable(t, "docker", fakeScriptLoggingArgs(log, "exit 0"))

	c, err := New(Options{Kind: Docker, ContainerName: "mail"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.ReloadConfig(ctx); err != nil {
		t.Fatalf("ReloadConfig should be a no-op for docker: %v", err)
	}

	want := "stop mail\nstart mail\n"
	if got := readArgsFile(t, log); got != want {
		t.Errorf("docker invocations:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestDockerActive(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{{"true", true}, {"false", false}} {
		t.Run(tc.out, func(t *testing.T) {
			withFakeExecutable(t, "docker", fmt.Sprintf("#!/bin/sh\necho %s\n", tc.out))
			c, err := New(Options{Kind: Docker})
			if err != nil {
				t.Fatal(err)
			}
			active, err := c.Active(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if active != tc.want {
				t.Errorf("Active() = %v, want %v", active, tc.want)
			}
		})
	}
}

func TestDockerActiveErrorsWhenContainerMissing(t *testing.T) {
	withFakeExecutable(t, "docker", "#!/bin/sh\necho 'Error: No such object: stalwart' >&2\nexit 1\n")
	c, err := New(Options{Kind: Docker})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Active(context.Background()); err == nil {
		t.Error("Active: want error when the container doesn't exist, got nil")
	}
}

// WaitFor exists because systemctl and docker both return as soon as the
// *request* succeeded: this proves it keeps polling past a still-running
// state rather than accepting the first answer.
func TestWaitForPollsUntilStateChanges(t *testing.T) {
	dir := t.TempDir()
	counter := dir + "/calls"
	withFakeExecutable(t, "systemctl", fmt.Sprintf(
		"#!/bin/sh\nn=$(cat %[1]q 2>/dev/null || echo 0)\nn=$((n+1))\necho $n > %[1]q\n"+
			"if [ $n -lt 3 ]; then echo deactivating; exit 3; fi\necho inactive; exit 3\n", counter))

	c, err := New(Options{Kind: Systemd})
	if err != nil {
		t.Fatal(err)
	}
	if err := WaitFor(context.Background(), c, false, 5*time.Second); err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
}

func TestWaitForTimesOutWhileStillActive(t *testing.T) {
	withFakeExecutable(t, "systemctl", "#!/bin/sh\necho active\n")
	c, err := New(Options{Kind: Systemd})
	if err != nil {
		t.Fatal(err)
	}
	err = WaitFor(context.Background(), c, false, 300*time.Millisecond)
	if err == nil {
		t.Fatal("WaitFor: want timeout error while the unit is still active, got nil")
	}
	if !strings.Contains(err.Error(), "not stopped") {
		t.Errorf("WaitFor error %q should say what it was waiting for", err)
	}
}

func TestWaitForSurfacesLastStateReadError(t *testing.T) {
	withFakeExecutable(t, "systemctl", "#!/bin/sh\necho 'no such unit' >&2\nexit 4\n")
	c, err := New(Options{Kind: Systemd})
	if err != nil {
		t.Fatal(err)
	}
	err = WaitFor(context.Background(), c, false, 300*time.Millisecond)
	if err == nil {
		t.Fatal("WaitFor: want error when the state can't be read at all, got nil")
	}
	if !strings.Contains(err.Error(), "state couldn't be read") {
		t.Errorf("WaitFor error %q should distinguish 'never reached the state' from 'never could tell'", err)
	}
}
