// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package cutover

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
)

// inspectJSON builds a `docker inspect` document. extra is merged into
// HostConfig so a test can add the configuration a recreate would drop.
func inspectJSON(t *testing.T, extraHost map[string]any, networks map[string]any) string {
	t.Helper()
	host := map[string]any{
		"PortBindings":  map[string]any{"143/tcp": []map[string]string{{"HostIp": "0.0.0.0", "HostPort": "143"}}},
		"RestartPolicy": map[string]any{"Name": "unless-stopped"},
		"NetworkMode":   "bridge",
		"LogConfig":     map[string]any{"Type": "json-file"},
	}
	for k, v := range extraHost {
		host[k] = v
	}
	if networks == nil {
		networks = map[string]any{"bridge": map[string]any{}}
	}
	doc := []map[string]any{{
		"Name":  "/stalwart",
		"Image": "sha256:old",
		"Config": map[string]any{
			"Image": "stalwartlabs/stalwart:v0.15.5",
			"Env":   []string{"TZ=UTC", "STALWART_RECOVERY_MODE=1"},
		},
		"State":           map[string]any{"Running": false},
		"Mounts":          []map[string]any{{"Type": "volume", "Name": "stalwart-data", "Destination": "/opt/stalwart", "RW": true}},
		"HostConfig":      host,
		"NetworkSettings": map[string]any{"Networks": networks},
	}}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fakeDockerCutover installs a docker that records arguments and serves the
// given inspect document.
func fakeDockerCutover(t *testing.T, doc string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "args.log")
	inspectFile := filepath.Join(dir, "inspect.json")
	if err := os.WriteFile(inspectFile, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
case "$1" in
  inspect) cat %q ;;
  rename) exit 0 ;;
  run) echo newcontainerid ;;
esac
`, log, inspectFile)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func runContainerFor(t *testing.T, doc string) (*checkpoint.RunState, Report, error) {
	t.Helper()
	fakeDockerCutover(t, doc)
	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatal(err)
	}
	var report Report
	step := func(name string, fn func() (checkpoint.StepOutcome, error)) error {
		outcome, err := store.RunStep(rs, checkpoint.PhaseCutover, name, fn)
		if err != nil {
			report.Results = append(report.Results, CheckResult{Name: name, Status: StatusFail, Detail: err.Error()})
			return err
		}
		report.Results = append(report.Results, CheckResult{Name: name, Status: StatusOK, Detail: outcome.Detail})
		return nil
	}
	err = runContainerCutover(context.Background(), rs, step, ContainerOptions{
		ContainerName: "stalwart", StagedImage: "sha256:new", PreserveDir: t.TempDir(),
	})
	return rs, report, err
}

func TestContainerCutoverPreservesTheDefinitionFirst(t *testing.T) {
	rs, _, err := runContainerFor(t, inspectJSON(t, nil, nil))
	if err != nil {
		t.Fatalf("runContainerCutover: %v", err)
	}
	art, ok := rs.Artifacts[ArtifactContainerDefinition]
	if !ok {
		t.Fatal("the container definition was not recorded as an artifact")
	}
	if art.SHA256 == "" || art.SizeBytes == 0 {
		t.Errorf("artifact recorded without a checksum or size: %+v", art)
	}
	body, err := os.ReadFile(art.Path)
	if err != nil {
		t.Fatalf("preserved definition unreadable: %v", err)
	}
	if !strings.Contains(string(body), "stalwart-data") {
		t.Error("preserved definition does not contain the container's mounts")
	}
}

// The old container is kept, not removed: with the old image unpruned it is
// the manual restore path (ARCHITECTURE.md §4.8).
func TestContainerCutoverRetiresRatherThanRemoves(t *testing.T) {
	log := fakeDockerCutover(t, inspectJSON(t, nil, nil))
	store := checkpoint.NewStore(t.TempDir())
	rs, _ := store.Create("0.15.5", "0.16.14")
	step := func(name string, fn func() (checkpoint.StepOutcome, error)) error {
		_, err := store.RunStep(rs, checkpoint.PhaseCutover, name, fn)
		return err
	}
	if err := runContainerCutover(context.Background(), rs, step, ContainerOptions{
		ContainerName: "stalwart", StagedImage: "sha256:new", PreserveDir: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	args := readLog(t, log)
	if !strings.Contains(args, "rename stalwart stalwart-premigration-0.15.5") {
		t.Errorf("old container was not retired by rename:\n%s", args)
	}
	if strings.Contains(args, "rm stalwart") || strings.Contains(args, "image rm") || strings.Contains(args, "prune") {
		t.Errorf("cutover removed something it should have kept:\n%s", args)
	}
}

func TestContainerCutoverRecreatesWithTheCarriedSettings(t *testing.T) {
	log := fakeDockerCutover(t, inspectJSON(t, nil, nil))
	store := checkpoint.NewStore(t.TempDir())
	rs, _ := store.Create("0.15.5", "0.16.14")
	step := func(name string, fn func() (checkpoint.StepOutcome, error)) error {
		_, err := store.RunStep(rs, checkpoint.PhaseCutover, name, fn)
		return err
	}
	if err := runContainerCutover(context.Background(), rs, step, ContainerOptions{
		ContainerName: "stalwart", StagedImage: "sha256:new", PreserveDir: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	args := readLog(t, log)
	for _, want := range []string{
		"run -d --name stalwart",
		"--restart unless-stopped",
		"-e TZ=UTC",
		"-v stalwart-data:/opt/stalwart",
		"-p 0.0.0.0:143:143",
		"sha256:new",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("recreate missing %q\ngot: %s", want, args)
		}
	}
	// Leaving recovery mode set would recovery-boot on every restart - the
	// footgun §4.5 strips from a unit.
	if strings.Contains(args, "STALWART_RECOVERY_MODE") {
		t.Errorf("recovery-mode env survived into the recreated container:\n%s", args)
	}
}

// The hazard this design exists for: a container carrying configuration a
// recreate would drop must be refused, not quietly rebuilt without it.
func TestContainerCutoverRefusesConfigurationItWouldDrop(t *testing.T) {
	for _, tc := range []struct {
		name  string
		host  map[string]any
		nets  map[string]any
		wants string
	}{
		{"capabilities", map[string]any{"CapAdd": []string{"NET_ADMIN"}}, nil, "capabilities"},
		{"devices", map[string]any{"Devices": []any{map[string]any{}}}, nil, "device mappings"},
		{"sysctls", map[string]any{"Sysctls": map[string]string{"net.core.somaxconn": "1024"}}, nil, "sysctls"},
		{"privileged", map[string]any{"Privileged": true}, nil, "privileged"},
		{"log driver", map[string]any{"LogConfig": map[string]any{"Type": "syslog"}}, nil, "log driver"},
		{"user network", nil, map[string]any{"mailnet": map[string]any{}}, "user-defined network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runContainerFor(t, inspectJSON(t, tc.host, tc.nets))
			if err == nil {
				t.Fatalf("expected a refusal for a container with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("refusal should name %q, got: %v", tc.wants, err)
			}
			if !strings.Contains(err.Error(), "by hand") {
				t.Errorf("refusal should tell the operator what to do instead, got: %v", err)
			}
		})
	}
}

// A refusal must happen before anything is touched.
func TestContainerCutoverRefusesBeforeRetiringAnything(t *testing.T) {
	log := fakeDockerCutover(t, inspectJSON(t, map[string]any{"Privileged": true}, nil))
	store := checkpoint.NewStore(t.TempDir())
	rs, _ := store.Create("0.15.5", "0.16.14")
	step := func(name string, fn func() (checkpoint.StepOutcome, error)) error {
		_, err := store.RunStep(rs, checkpoint.PhaseCutover, name, fn)
		return err
	}
	if err := runContainerCutover(context.Background(), rs, step, ContainerOptions{
		ContainerName: "stalwart", StagedImage: "sha256:new", PreserveDir: t.TempDir(),
	}); err == nil {
		t.Fatal("expected a refusal")
	}
	if args := readLog(t, log); strings.Contains(args, "rename") || strings.Contains(args, "run -d") {
		t.Errorf("refusal came after the container was already changed:\n%s", args)
	}
}

func TestContainerCutoverNeedsAStagedImage(t *testing.T) {
	store := checkpoint.NewStore(t.TempDir())
	rs, _ := store.Create("0.15.5", "0.16.14")
	step := func(name string, fn func() (checkpoint.StepOutcome, error)) error {
		_, err := store.RunStep(rs, checkpoint.PhaseCutover, name, fn)
		return err
	}
	if err := runContainerCutover(context.Background(), rs, step, ContainerOptions{
		ContainerName: "stalwart", PreserveDir: t.TempDir(),
	}); err == nil {
		t.Fatal("expected a refusal with no staged image")
	}
}

func readLog(t *testing.T, path string) string {
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
