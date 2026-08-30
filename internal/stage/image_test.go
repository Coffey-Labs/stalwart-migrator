// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package stage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDockerImage installs a `docker` that records its arguments and
// answers each subcommand as scripted. versionBody is what `docker run`
// prints for the version probe.
func fakeDockerImage(t *testing.T, pullExit, imageID, versionBody string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "docker-args.log")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
case "$1" in
  pull) exit %s ;;
  image) echo %q ;;
  run) %s ;;
esac
`, log, pullExit, imageID, versionBody)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

const testImageID = "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func TestRunImageStagesAndVerifies(t *testing.T) {
	log := fakeDockerImage(t, "0", testImageID, "echo 'stalwart 0.16.14'")
	store, rs := newRun(t)

	staged, err := RunImage(context.Background(), store, rs, ImageOptions{
		Image: "stalwartlabs/stalwart:v0.16.14", TargetVersion: "0.16.14",
	})
	if err != nil {
		t.Fatalf("RunImage: %v", err)
	}
	if staged.Version != "0.16.14" {
		t.Errorf("Version = %q", staged.Version)
	}
	// The ID, not the tag: a tag can move between staging and cutover, and
	// running the tag later would run something else.
	if staged.Ref != testImageID {
		t.Errorf("Ref = %q, want the image ID %q", staged.Ref, testImageID)
	}
	if got := readFile(t, log); !strings.Contains(got, "pull stalwartlabs/stalwart:v0.16.14") {
		t.Errorf("image was never pulled:\n%s", got)
	}
}

// The version check is the point of the phase. An image whose tag claims
// one version and whose binary reports another must not be staged.
func TestRunImageRefusesAVersionMismatch(t *testing.T) {
	fakeDockerImage(t, "0", testImageID, "echo 'stalwart 0.16.9'")
	store, rs := newRun(t)

	_, err := RunImage(context.Background(), store, rs, ImageOptions{
		Image: "stalwartlabs/stalwart:v0.16.14", TargetVersion: "0.16.14",
	})
	if err == nil {
		t.Fatal("expected a refusal when the image reports a different version")
	}
	if !strings.Contains(err.Error(), "does not contain what it claims") {
		t.Errorf("error should name the mismatch, got: %v", err)
	}
}

// An image that will not say what it is cannot be staged - the alternative
// is migrating a mail server with software nothing identified.
func TestRunImageRefusesAnImageThatWontReportItsVersion(t *testing.T) {
	fakeDockerImage(t, "0", testImageID, "echo 'no such flag' >&2; exit 1")
	store, rs := newRun(t)

	_, err := RunImage(context.Background(), store, rs, ImageOptions{
		Image: "img:1", TargetVersion: "0.16.14",
	})
	if err == nil {
		t.Fatal("expected a refusal when the image will not report a version")
	}
	if !strings.Contains(err.Error(), "will not stage an image it cannot identify") {
		t.Errorf("error should explain the refusal, got: %v", err)
	}
}

// If the plain entrypoint refuses the flag, the binary is tried by name
// before giving up.
func TestRunImageFallsBackToNamingTheBinary(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "docker-args.log")
	// Fails without --entrypoint, succeeds with it.
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
case "$1" in
  pull) exit 0 ;;
  image) echo %q ;;
  run) for a in "$@"; do if [ "$a" = "--entrypoint" ]; then echo 'stalwart 0.16.14'; exit 0; fi; done; exit 1 ;;
esac
`, log, testImageID)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	store, rs := newRun(t)
	staged, err := RunImage(context.Background(), store, rs, ImageOptions{
		Image: "img:1", TargetVersion: "0.16.14",
	})
	if err != nil {
		t.Fatalf("RunImage should have fallen back to --entrypoint: %v", err)
	}
	if staged.Version != "0.16.14" {
		t.Errorf("Version = %q", staged.Version)
	}
}

func TestRunImageRefusesAFailedPull(t *testing.T) {
	fakeDockerImage(t, "1", testImageID, "echo 'stalwart 0.16.14'")
	store, rs := newRun(t)

	if _, err := RunImage(context.Background(), store, rs, ImageOptions{
		Image: "img:1", TargetVersion: "0.16.14",
	}); err == nil {
		t.Fatal("expected a refusal when the pull failed")
	}
}

// SkipPull is for a host that loaded the image from a tarball, where a
// pull cannot work and its failure would say nothing useful.
func TestSkipPullUsesALocalImage(t *testing.T) {
	log := fakeDockerImage(t, "1", testImageID, "echo 'stalwart 0.16.14'")
	store, rs := newRun(t)

	if _, err := RunImage(context.Background(), store, rs, ImageOptions{
		Image: "img:1", TargetVersion: "0.16.14", SkipPull: true,
	}); err != nil {
		t.Fatalf("SkipPull should not have pulled: %v", err)
	}
	if got := readFile(t, log); strings.Contains(got, "pull ") {
		t.Errorf("SkipPull still pulled:\n%s", got)
	}
}

// The image is never guessed from what is running: a derived tag is wrong
// for a digest-pinned image, a mirror or a fork.
func TestRunImageRefusesWithoutAnImage(t *testing.T) {
	store, rs := newRun(t)
	_, err := RunImage(context.Background(), store, rs, ImageOptions{TargetVersion: "0.16.14"})
	if err == nil {
		t.Fatal("expected a refusal when no image was named")
	}
	if !strings.Contains(err.Error(), "never guessed") {
		t.Errorf("error should say the image is not inferred, got: %v", err)
	}
}

// A resumed run skips the step, and must still report the image the
// earlier run verified rather than an empty reference.
func TestRunImageOnResumeReturnsTheVerifiedImage(t *testing.T) {
	fakeDockerImage(t, "0", testImageID, "echo 'stalwart 0.16.14'")
	store, rs := newRun(t)
	opts := ImageOptions{Image: "img:1", TargetVersion: "0.16.14"}

	if _, err := RunImage(context.Background(), store, rs, opts); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.Load(rs.RunID)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := RunImage(context.Background(), store, resumed, opts)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if staged.Ref != testImageID {
		t.Errorf("resumed Ref = %q, want %q", staged.Ref, testImageID)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
