// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package stage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Coffey-Labs/stalwart-migrator/internal/checkpoint"
)

// tarGzWith builds a release-shaped archive containing one entry.
func tarGzWith(t *testing.T, name, body string, typeflag byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: typeflag}
	if typeflag == tar.TypeSymlink {
		hdr.Size = 0
		hdr.Linkname = "/etc/passwd"
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if typeflag == tar.TypeReg {
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// releaseServer stands in for the GitHub release API plus asset hosting.
func releaseServer(t *testing.T, tag string, archive []byte, assetName string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/download") {
			w.Write(archive)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": tag,
			"assets": []map[string]any{
				{"name": "stalwart-foundationdb-x86_64-unknown-linux-gnu.tar.gz", "browser_download_url": srv.URL + "/wrong/download"},
				{"name": assetName, "browser_download_url": srv.URL + "/right/download"},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A fake "binary" that reports a version, so the staged-version check has
// something real to run.
func versionScript(v string) string {
	return fmt.Sprintf("#!/bin/sh\necho 'stalwart %s'\n", v)
}

func newRun(t *testing.T) (*checkpoint.Store, *checkpoint.RunState) {
	t.Helper()
	store := checkpoint.NewStore(filepath.Join(t.TempDir(), "runs"))
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatal(err)
	}
	return store, rs
}

func withReleaseAPI(t *testing.T, srv *httptest.Server) {
	t.Helper()
	// preflight.ResolveRelease reads its base from a package var; point it
	// at the fake for the duration of the test.
	old := releaseAPIBase()
	setReleaseAPIBase(srv.URL)
	t.Cleanup(func() { setReleaseAPIBase(old) })
}

func TestRunStagesTheServerBuildAndVerifiesItsVersion(t *testing.T) {
	archive := tarGzWith(t, "stalwart", versionScript("0.16.14"), tar.TypeReg)
	srv := releaseServer(t, "v0.16.14", archive, hostAssetName(t))
	withReleaseAPI(t, srv)
	store, rs := newRun(t)
	dest := filepath.Join(t.TempDir(), "stalwart-0.16.14")

	path, err := Run(context.Background(), store, rs, Options{
		TargetVersion: "0.16.14", DestPath: dest, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if path != dest {
		t.Errorf("path = %q, want %q", path, dest)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("staged binary mode = %v, want executable", info.Mode().Perm())
	}
	// The archive it must NOT have taken is the FoundationDB build.
	if _, err := os.Stat(dest + ".tar.gz"); !os.IsNotExist(err) {
		t.Error("the downloaded archive should be cleaned up after extraction")
	}
}

// The release publishes several builds; taking the first plausible one
// would stage a FoundationDB or musl variant and surface as a puzzling
// runtime failure much later.
func TestRunRefusesWhenTheServerBuildIsAbsent(t *testing.T) {
	archive := tarGzWith(t, "stalwart", versionScript("0.16.14"), tar.TypeReg)
	srv := releaseServer(t, "v0.16.14", archive, "stalwart-foundationdb-"+strings.TrimPrefix(hostAssetName(t), "stalwart-"))
	withReleaseAPI(t, srv)
	store, rs := newRun(t)

	_, err := Run(context.Background(), store, rs, Options{
		TargetVersion: "0.16.14", DestPath: filepath.Join(t.TempDir(), "s"), HTTPClient: srv.Client(),
	})
	if err == nil {
		t.Fatal("want a refusal when this host's plain server build isn't published")
	}
	if !strings.Contains(err.Error(), "won't substitute another") {
		t.Errorf("error %q should say it refuses to substitute a different build", err)
	}
}

// The binary's own --version is the only thing that settles what was
// actually fetched; everything upstream is an assumption about someone
// else's release process.
func TestRunRefusesAnArchiveThatDoesNotMatchItsTag(t *testing.T) {
	archive := tarGzWith(t, "stalwart", versionScript("0.16.9"), tar.TypeReg)
	srv := releaseServer(t, "v0.16.14", archive, hostAssetName(t))
	withReleaseAPI(t, srv)
	store, rs := newRun(t)

	_, err := Run(context.Background(), store, rs, Options{
		TargetVersion: "0.16.14", DestPath: filepath.Join(t.TempDir(), "s"), HTTPClient: srv.Client(),
	})
	if err == nil {
		t.Fatal("want a refusal when the asset contains a different version than its tag claims")
	}
	if !strings.Contains(err.Error(), "0.16.9") {
		t.Errorf("error %q should report what the binary actually said", err)
	}
}

func TestRunHonoursAPinnedChecksum(t *testing.T) {
	archive := tarGzWith(t, "stalwart", versionScript("0.16.14"), tar.TypeReg)
	srv := releaseServer(t, "v0.16.14", archive, hostAssetName(t))
	withReleaseAPI(t, srv)
	store, rs := newRun(t)

	_, err := Run(context.Background(), store, rs, Options{
		TargetVersion: "0.16.14", DestPath: filepath.Join(t.TempDir(), "s"),
		SHA256: "0000000000000000000000000000000000000000000000000000000000000000", HTTPClient: srv.Client(),
	})
	if err == nil {
		t.Fatal("want a refusal when the download doesn't match the pin")
	}
	if !strings.Contains(err.Error(), "was pinned") {
		t.Errorf("error %q should say the pin was violated", err)
	}

	// And the matching pin is accepted.
	sum := sha256.Sum256(archive)
	store2, rs2 := newRun(t)
	if _, err := Run(context.Background(), store2, rs2, Options{
		TargetVersion: "0.16.14", DestPath: filepath.Join(t.TempDir(), "s"),
		SHA256: hex.EncodeToString(sum[:]), HTTPClient: srv.Client(),
	}); err != nil {
		t.Fatalf("a matching pin should be accepted: %v", err)
	}
}

// A tarball is untrusted input.
func TestExtractRefusesANonRegularEntry(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(archivePath, tarGzWith(t, "stalwart", "", tar.TypeSymlink), 0o640); err != nil {
		t.Fatal(err)
	}
	err := extractBinary(archivePath, filepath.Join(dir, "out"))
	if err == nil {
		t.Fatal("want a refusal for a symlink entry, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to follow") {
		t.Errorf("error %q should say it refuses to follow it", err)
	}
}

// The separately-versioned CLI ships its binary a directory down, so
// searching by base name rather than exact path is deliberate.
func TestExtractFindsTheBinaryInASubdirectory(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(archivePath, tarGzWith(t, "stalwart-x86_64/stalwart", versionScript("0.16.14"), tar.TypeReg), 0o640); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out")
	if err := extractBinary(archivePath, dest); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if body, _ := os.ReadFile(dest); !strings.Contains(string(body), "0.16.14") {
		t.Errorf("extracted %q, want the binary from the subdirectory", body)
	}
}

// Re-running a completed stage must not re-download.
func TestRunIsSkippedOnResume(t *testing.T) {
	archive := tarGzWith(t, "stalwart", versionScript("0.16.14"), tar.TypeReg)
	var downloads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/download") {
			downloads++
			w.Write(archive)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v0.16.14",
			"assets":   []map[string]any{{"name": hostAssetName(t), "browser_download_url": "http://" + r.Host + "/right/download"}},
		})
	}))
	defer srv.Close()
	withReleaseAPI(t, srv)
	store, rs := newRun(t)
	opts := Options{TargetVersion: "0.16.14", DestPath: filepath.Join(t.TempDir(), "s"), HTTPClient: srv.Client()}

	if _, err := Run(context.Background(), store, rs, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), store, rs, opts); err != nil {
		t.Fatal(err)
	}
	if downloads != 1 {
		t.Errorf("downloaded %d time(s), want 1 - a resumed run must not re-fetch 100 MB", downloads)
	}
}

// hostAssetName is the release asset this machine's architecture calls
// for. Tests name it this way rather than hard-coding x86_64 so the suite
// passes on the arm64 hosts this tool is also expected to run on.
func hostAssetName(t *testing.T) string {
	t.Helper()
	name, err := hostAsset(runtime.GOARCH)
	if err != nil {
		t.Skipf("no server build is selected for this architecture: %v", err)
	}
	return name
}

// Staging the x86_64 build on an arm64 host produced "exec format error"
// from stage's own version check, with nothing in the message to say the
// download had been for the wrong machine. Reported by @kaya-eu, who had
// to fetch the aarch64 archive by hand and pass it with --target-binary.
func TestHostAssetFollowsTheArchitecture(t *testing.T) {
	for arch, want := range map[string]string{
		"amd64": "stalwart-x86_64-unknown-linux-gnu.tar.gz",
		"arm64": "stalwart-aarch64-unknown-linux-gnu.tar.gz",
	} {
		got, err := hostAsset(arch)
		if err != nil {
			t.Fatalf("hostAsset(%q): %v", arch, err)
		}
		if got != want {
			t.Errorf("hostAsset(%q) = %q, want %q", arch, got, want)
		}
	}
}

// An architecture with no unambiguous gnu server build is refused by name
// rather than quietly falling back to x86_64, which is the bug this
// replaced. 32-bit ARM is the real case: GOARCH=arm does not say whether
// the host wants the arm or the armv7 archive.
func TestHostAssetRefusesAnArchitectureItCannotChooseFor(t *testing.T) {
	_, err := hostAsset("arm")
	if err == nil {
		t.Fatal("want a refusal for an architecture with no selected build")
	}
	if !strings.Contains(err.Error(), "--target-binary") {
		t.Errorf("error %q should point at the --target-binary escape hatch", err)
	}
}

// Every selected asset must be the plain server build. The FoundationDB
// and musl archives are a substring away from the right answer and would
// surface as a puzzling runtime failure much later.
func TestSelectedAssetsAreThePlainServerBuilds(t *testing.T) {
	for arch, name := range assetForArch {
		if !strings.HasPrefix(name, "stalwart-") || !strings.HasSuffix(name, "-unknown-linux-gnu.tar.gz") {
			t.Errorf("%s: %q is not a plain Linux gnu server archive", arch, name)
		}
		if strings.Contains(name, "foundationdb") || strings.Contains(name, "musl") {
			t.Errorf("%s: %q is a variant build, not the plain server", arch, name)
		}
	}
}
