// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package recovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDocker installs a `docker` that logs every invocation's arguments and
// behaves as scripted per subcommand. runBody is what `docker run` does;
// stateBody is what `docker inspect -f {{.State.Running}}` prints.
func fakeDocker(t *testing.T, runBody, stopExit, stateBody string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "docker-args.log")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
case "$1" in
  run) %s ;;
  stop) exit %s ;;
  inspect) %s ;;
esac
`, log, runBody, stopExit, stateBody)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// waitForArgs polls until the fake has recorded an invocation. Launch
// returns once the OS has started the client, which is before the shell it
// started has run anything.
func waitForArgs(t *testing.T, log string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := readArgsFile(t, log); got != "" {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake docker was never invoked (log %s stayed empty)", log)
	return ""
}

// waitForOutput polls until the supervised process has produced some, for
// the same reason waitForArgs exists: Launch returns before the thing it
// started has said anything, and stopping it first would race the output
// away.
func waitForOutput(t *testing.T, sup Supervised) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := sup.Output(); strings.TrimSpace(got) != "" {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return sup.Output()
}

func mounts() []ContainerMount {
	return []ContainerMount{{Source: "stalwart-data", Destination: "/opt/stalwart"}}
}

func TestContainerLauncherBuildsTheRunCommand(t *testing.T) {
	log := fakeDocker(t, "exec sleep 300", "0", "echo false")

	sup, err := ContainerLauncher{
		Image: "stalwartlabs/stalwart@sha256:abc", Mounts: mounts(),
		Name: "recov", Publish: []string{"127.0.0.1:8080:8080"},
	}.Launch(context.Background(), LaunchOptions{
		ConfigPath: "/opt/stalwart/etc/config.json", RecoveryMode: true,
		AdminUser: "admin", AdminPassword: "s3cret", ExtraEnv: []string{"FOO=bar"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer sup.Stop(time.Second)

	args := waitForArgs(t, log)
	for _, want := range []string{
		"run --rm --name recov",
		"-e STALWART_RECOVERY_MODE=1",
		"-e STALWART_RECOVERY_ADMIN=admin:s3cret",
		"-e FOO=bar",
		"-v stalwart-data:/opt/stalwart",
		"-p 127.0.0.1:8080:8080",
		"stalwartlabs/stalwart@sha256:abc --config /opt/stalwart/etc/config.json",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("docker run args missing %q\ngot: %s", want, args)
		}
	}
}

// An ordinary boot has no recovery variables. Leaving them set is the
// documented footgun cutover strips from a unit; the container path must
// not reintroduce it.
func TestContainerLauncherOmitsRecoveryEnvOnAnOrdinaryBoot(t *testing.T) {
	log := fakeDocker(t, "exec sleep 300", "0", "echo false")
	sup, err := ContainerLauncher{Image: "img", Mounts: mounts(), Name: "recov"}.
		Launch(context.Background(), LaunchOptions{RecoveryMode: false})
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Stop(time.Second)

	if args := waitForArgs(t, log); strings.Contains(args, "STALWART_RECOVERY") {
		t.Errorf("ordinary boot set recovery env:\n%s", args)
	}
}

// Migrating an empty store and reporting success is the worst outcome
// available here, so no mounts is refused rather than defaulted.
func TestContainerLauncherRefusesWithoutMounts(t *testing.T) {
	fakeDocker(t, "exec sleep 300", "0", "echo false")
	_, err := ContainerLauncher{Image: "img", Name: "recov"}.
		Launch(context.Background(), LaunchOptions{})
	if err == nil {
		t.Fatal("expected a refusal when no mounts were given")
	}
	if !strings.Contains(err.Error(), "empty store") {
		t.Errorf("refusal should say why it matters, got: %v", err)
	}
}

func TestContainerLauncherRefusesWithoutAnImage(t *testing.T) {
	_, err := ContainerLauncher{Mounts: mounts()}.Launch(context.Background(), LaunchOptions{})
	if err == nil {
		t.Fatal("expected a refusal when no image was given")
	}
}

// Stop must stop the container by name, not just the client. A killed
// client leaves the container running and holding the store, and the run
// would move on believing it had stopped.
func TestStopStopsTheContainerAndNotOnlyTheClient(t *testing.T) {
	log := fakeDocker(t, "exec sleep 300", "0", "echo false")
	sup, err := ContainerLauncher{Image: "img", Mounts: mounts(), Name: "recov"}.
		Launch(context.Background(), LaunchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	args := waitForArgs(t, log)
	if !strings.Contains(args, "stop --time 2 recov") {
		t.Errorf("Stop did not stop the container by name:\n%s", args)
	}
}

// The hazard this exists for: docker reported the stop fine, but the
// container is still running. Nothing may touch that store, so Stop has to
// fail loudly rather than let the run continue.
func TestStopFailsWhenTheContainerSurvives(t *testing.T) {
	fakeDocker(t, "exec sleep 300", "0", "echo true")
	sup, err := ContainerLauncher{Image: "img", Mounts: mounts(), Name: "recov"}.
		Launch(context.Background(), LaunchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = sup.Stop(time.Second)
	if err == nil {
		t.Fatal("Stop reported success while the container was still running")
	}
	if !strings.Contains(err.Error(), "still holds the store open") {
		t.Errorf("error should say why it matters, got: %v", err)
	}
}

// With --rm a clean shutdown removes the container before Stop runs, so
// `docker stop` failing on a container that is already gone is the normal
// path, not a failure.
func TestStopToleratesAnAlreadyGoneContainer(t *testing.T) {
	fakeDocker(t, "exec sleep 300", "1", "exit 1")
	sup, err := ContainerLauncher{Image: "img", Mounts: mounts(), Name: "recov"}.
		Launch(context.Background(), LaunchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Stop(time.Second); err != nil {
		t.Fatalf("Stop should tolerate an already-removed container, got: %v", err)
	}
}

// The server's own words are the diagnosis; a container must not lose them.
func TestContainerOutputIsCaptured(t *testing.T) {
	fakeDocker(t, "echo 'Failed to bind to [::]:8080: Address already in use'; exit 1", "0", "exit 1")
	sup, err := ContainerLauncher{Image: "img", Mounts: mounts(), Name: "recov"}.
		Launch(context.Background(), LaunchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := waitForOutput(t, sup)
	_ = sup.Stop(time.Second)
	if !strings.Contains(out, "Address already in use") {
		t.Errorf("container output was not captured, got: %q", out)
	}
}
