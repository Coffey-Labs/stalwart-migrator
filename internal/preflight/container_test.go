// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
)

// fakeInspect writes a `docker` that answers `inspect` with the given JSON
// document, so the container checks can be exercised without a container.
func fakeInspect(t *testing.T, doc string) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "inspect.json")
	if err := os.WriteFile(out, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in inspect) cat %q ;; *) exit 1 ;; esac\n", out)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func inspectDoc(t *testing.T, labels map[string]string, mounts []Mount) string {
	t.Helper()
	doc := []map[string]any{{
		"Name":   "/stalwart",
		"Image":  "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"Config": map[string]any{"Image": "stalwartlabs/stalwart:v0.15.5", "Labels": labels},
		"State":  map[string]any{"Running": true},
		"Mounts": mounts,
	}}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func dataVolume(dest string) Mount {
	return Mount{Type: "volume", Name: "stalwart-data", Destination: dest, RW: true}
}

func TestInspectContainerReadsTheFactsThatMatter(t *testing.T) {
	fakeInspect(t, inspectDoc(t, map[string]string{"com.docker.compose.project": "mail"}, []Mount{dataVolume("/opt/stalwart")}))

	facts, err := InspectContainer(context.Background(), "stalwart")
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if facts.Name != "stalwart" {
		t.Errorf("Name = %q, want stalwart (leading slash stripped)", facts.Name)
	}
	if facts.Image != "stalwartlabs/stalwart:v0.15.5" {
		t.Errorf("Image = %q", facts.Image)
	}
	if facts.ComposeProject() != "mail" {
		t.Errorf("ComposeProject() = %q, want mail", facts.ComposeProject())
	}
	if !facts.Running {
		t.Error("Running = false, want true")
	}
}

// A tag and the digest actually running can disagree - :latest is the
// obvious way, but any moved tag does it. Both are reported because only
// one of them says what is really running.
func TestInspectContainerKeepsTagAndDigestApart(t *testing.T) {
	fakeInspect(t, inspectDoc(t, nil, nil))
	facts, err := InspectContainer(context.Background(), "stalwart")
	if err != nil {
		t.Fatal(err)
	}
	if facts.ImageID == facts.Image {
		t.Error("ImageID and Image should be distinct - one is a tag, the other a digest")
	}
	if got := shortID(facts.ImageID); got != "0123456789ab" {
		t.Errorf("shortID = %q, want 0123456789ab", got)
	}
}

func TestMountForPrefersTheMostSpecificMount(t *testing.T) {
	facts := ContainerFacts{Mounts: []Mount{
		{Type: "bind", Source: "/srv", Destination: "/var/lib", RW: true},
		{Type: "volume", Name: "data", Destination: "/var/lib/stalwart/data", RW: true},
	}}
	m, ok := facts.MountFor("/var/lib/stalwart/data/db")
	if !ok {
		t.Fatal("MountFor found nothing for a path under a mount")
	}
	if m.Name != "data" {
		t.Errorf("MountFor returned %q, want the more specific 'data' mount", m.Name)
	}
	if _, ok := facts.MountFor("/etc/stalwart"); ok {
		t.Error("MountFor matched a path no mount covers")
	}
}

// containerReport runs preflight against a fake container.
func containerReport(t *testing.T, doc string, dataDir string, advisory bool) Report {
	t.Helper()
	for _, p := range systemdUnitPaths {
		if _, err := os.Stat(p); err == nil {
			t.Skipf("host has %s, which detection prefers over docker", p)
		}
	}
	fakeInspect(t, doc)

	counterPath := filepath.Join(t.TempDir(), "invocations")
	binaryPath := writeFakeBinary(t, "0.15.5", counterPath)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("[server]\nhostname = \"mail.example.com\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withFakeGithub(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Release{TagName: "v0.16.14"})
	})
	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if dataDir == "" {
		dataDir = t.TempDir()
	}
	report, err := New(Options{
		BinaryPath: binaryPath, ConfigPath: configPath, DataDir: dataDir,
		TargetVersion: "latest", ToolCheckAdvisory: true, DeploymentCheckAdvisory: advisory,
	}).Run(context.Background(), store, rs)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return report
}

func resultFor(t *testing.T, r Report, name string) CheckResult {
	t.Helper()
	for _, res := range r.Results {
		if res.Name == name {
			return res
		}
	}
	t.Fatalf("no %q result in report:\n%s", name, r.String())
	return CheckResult{}
}

// A compose-managed container must be refused even once container cutover
// exists: recreating it desyncs the running container from the compose
// file, and the next `compose up` reverts the migration.
func TestComposeManagedContainerIsRefused(t *testing.T) {
	doc := inspectDoc(t, map[string]string{"com.docker.compose.project": "mail"}, []Mount{dataVolume("/opt/stalwart")})
	res := resultFor(t, containerReport(t, doc, "/opt/stalwart", false), "container-runtime")
	if res.Status != StatusFail {
		t.Errorf("container-runtime = %q, want %q\n%s", res.Status, StatusFail, res.Detail)
	}
}

func TestPlainContainerPassesTheRuntimeCheck(t *testing.T) {
	doc := inspectDoc(t, nil, []Mount{dataVolume("/opt/stalwart")})
	res := resultFor(t, containerReport(t, doc, "/opt/stalwart", false), "container-runtime")
	if res.Status != StatusOK {
		t.Errorf("container-runtime = %q, want %q\n%s", res.Status, StatusOK, res.Detail)
	}
}

// Data in the container's own writable layer does not survive the container
// being replaced, and replacing it is what migrating it means.
func TestContainerWithNoWritableMountIsRefused(t *testing.T) {
	doc := inspectDoc(t, nil, nil)
	res := resultFor(t, containerReport(t, doc, "/opt/stalwart", false), "container-data-volume")
	if res.Status != StatusFail {
		t.Errorf("container-data-volume = %q, want %q\n%s", res.Status, StatusFail, res.Detail)
	}
}

// Mounts existing is not the same as the data being on one.
func TestDataDirOutsideEveryMountIsRefused(t *testing.T) {
	doc := inspectDoc(t, nil, []Mount{dataVolume("/opt/stalwart")})
	res := resultFor(t, containerReport(t, doc, "/var/lib/stalwart", false), "container-data-volume")
	if res.Status != StatusFail {
		t.Errorf("container-data-volume = %q, want %q\n%s", res.Status, StatusFail, res.Detail)
	}
}

func TestDataDirOnAVolumePasses(t *testing.T) {
	doc := inspectDoc(t, nil, []Mount{dataVolume("/opt/stalwart")})
	res := resultFor(t, containerReport(t, doc, "/opt/stalwart/data", false), "container-data-volume")
	if res.Status != StatusOK {
		t.Errorf("container-data-volume = %q, want %q\n%s", res.Status, StatusOK, res.Detail)
	}
}

// rehearse has to keep working against a container it cannot migrate -
// that is when its report is most useful - so the same findings are
// advisory there.
func TestRehearseReportsContainerProblemsWithoutBlocking(t *testing.T) {
	doc := inspectDoc(t, map[string]string{"com.docker.compose.project": "mail"}, nil)
	// A real directory, because disk-space stats DataDir on the host. That
	// a container-internal path breaks host-side checks is true and is
	// PR 3's problem (path translation); it is not what this is testing.
	report := containerReport(t, doc, t.TempDir(), true)
	if report.Blocking() {
		t.Fatalf("advisory mode should not block:\n%s", report.String())
	}
	for _, name := range []string{"container-runtime", "container-data-volume"} {
		if got := resultFor(t, report, name).Status; got != StatusWarn {
			t.Errorf("%s = %q, want %q in advisory mode", name, got, StatusWarn)
		}
	}
}
