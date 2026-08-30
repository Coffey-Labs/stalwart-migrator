// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package stage

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Coffey-Labs/stalwart-migrator/internal/checkpoint"
	"github.com/Coffey-Labs/stalwart-migrator/internal/preflight"
)

// assetForArch maps a Go architecture to the release asset holding the
// plain Linux server binary built for it. Stalwart publishes a couple of
// dozen builds per release, and these are matched by exact name rather
// than by anything that looks close - picking the FoundationDB build or a
// musl variant by accident is the kind of mistake that surfaces as a
// puzzling runtime failure much later, and "stalwart-foundationdb-aarch64-
// unknown-linux-gnu.tar.gz" is a substring match away from the right
// answer.
//
// Only the two architectures with an unambiguous gnu server build are
// here. The 32-bit ARM releases come in arm and armv7 flavours that
// GOARCH=arm does not distinguish between, and guessing which one a host
// wants is how it ends up with a binary that runs until it doesn't.
var assetForArch = map[string]string{
	"amd64": "stalwart-x86_64-unknown-linux-gnu.tar.gz",
	"arm64": "stalwart-aarch64-unknown-linux-gnu.tar.gz",
}

// hostAsset names the asset for the architecture this tool is running on.
//
// It is the host's architecture, not a configured one, because the binary
// staged here is executed here: stage runs it to confirm its version, the
// recovery cycle runs it to migrate the store, and cutover installs it as
// the service. A mismatch surfaces as "exec format error" at the first of
// those - early, and before anything has stopped, but with nothing in the
// message to say the download was for the wrong machine.
// It takes the architecture rather than reading runtime.GOARCH itself so
// a test can ask what an arm64 host would have downloaded without being
// one.
func hostAsset(goarch string) (string, error) {
	name, ok := assetForArch[goarch]
	if !ok {
		return "", fmt.Errorf(
			"stage: no Stalwart server build is selected for %s/%s - this tool stages the x86_64 and aarch64 Linux "+
				"builds and won't guess at another. Download the right release archive yourself and pass it with "+
				"--target-binary, and pin it with --target-binary-sha256",
			runtime.GOOS, goarch)
	}
	return name, nil
}

// binaryNameInArchive is the file to extract from that tarball.
const binaryNameInArchive = "stalwart"

// maxBinaryBytes caps extraction. The 0.16.14 server binary is ~100 MB; a
// limit an order of magnitude above that stops a malformed or hostile
// archive from filling the disk while leaving ample room for growth.
const maxBinaryBytes = 1 << 30

// Options configures staging.
type Options struct {
	// TargetVersion is the release to fetch ("0.16.14", or "latest").
	TargetVersion string
	// DestPath is where the extracted binary is written. It must not be
	// the running binary's path: staging installs *alongside*, and cutover
	// is what moves it into place.
	DestPath string
	// SHA256, when set, is the expected checksum of the downloaded archive.
	// Recommended: the release process does not always publish a checksum
	// manifest, so pinning is how a second run gets the guarantee the
	// first one couldn't have.
	SHA256     string
	HTTPClient *http.Client
}

// Run downloads the target release, verifies what it can, extracts the
// server binary to DestPath, and confirms the binary reports the version
// that was asked for.
//
// That last check is the point of the phase. Everything upstream of it -
// the tag lookup, the asset name, the archive layout - is an assumption
// about someone else's release process, and the binary's own --version
// output is the only thing that actually settles what was fetched. Cutover
// checks it again before installing, deliberately: this phase and that one
// can be separated by a long time and an operator's own file management.
func Run(ctx context.Context, store *checkpoint.Store, rs *checkpoint.RunState, opts Options) (string, error) {
	if opts.DestPath == "" {
		return "", fmt.Errorf("stage: no destination path for the target binary")
	}

	outcome, err := store.RunStep(rs, checkpoint.PhaseStage, "stage-binary", func() (checkpoint.StepOutcome, error) {
		release, err := preflight.ResolveRelease(ctx, opts.HTTPClient, opts.TargetVersion)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		wantAsset, err := hostAsset(runtime.GOARCH)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		asset := serverAsset(release, wantAsset)
		if asset == nil {
			names := make([]string, 0, len(release.Assets))
			for _, a := range release.Assets {
				names = append(names, a.Name)
			}
			return checkpoint.StepOutcome{}, fmt.Errorf(
				"stage: release %s publishes no %s asset, which is the Linux server build for this host's %s (found: %s) - "+
					"this tool won't substitute another",
				release.TagName, wantAsset, runtime.GOARCH, strings.Join(names, ", "))
		}

		archivePath := opts.DestPath + ".tar.gz"
		sum, err := download(ctx, opts.HTTPClient, asset.DownloadURL, archivePath)
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		defer os.Remove(archivePath)

		if opts.SHA256 != "" && !strings.EqualFold(sum, opts.SHA256) {
			return checkpoint.StepOutcome{}, fmt.Errorf(
				"stage: downloaded %s has sha256 %s but %s was expected - refusing to stage a binary that isn't the one that was pinned",
				asset.Name, sum, opts.SHA256)
		}

		if err := extractBinary(archivePath, opts.DestPath); err != nil {
			return checkpoint.StepOutcome{}, err
		}

		got, err := preflight.DetectVersion(ctx, opts.DestPath)
		if err != nil {
			return checkpoint.StepOutcome{}, fmt.Errorf("stage: staged binary at %s won't report its version: %w", opts.DestPath, err)
		}
		wanted := strings.TrimPrefix(release.TagName, "v")
		if got != wanted {
			return checkpoint.StepOutcome{}, fmt.Errorf(
				"stage: staged binary reports %s but release %s was fetched - the release asset does not contain what its tag claims",
				got, release.TagName)
		}

		pinNote := ""
		if opts.SHA256 == "" {
			pinNote = fmt.Sprintf("; no checksum was pinned - record sha256 %s to pin this download for future runs", sum)
		}
		return checkpoint.StepOutcome{
			Detail: fmt.Sprintf("staged %s at %s, which reports %s%s", asset.Name, opts.DestPath, got, pinNote),
			Extra:  got,
		}, nil
	})
	if err != nil {
		return "", err
	}
	_ = outcome
	return opts.DestPath, nil
}

func serverAsset(release *preflight.Release, name string) *preflight.ReleaseAsset {
	for i := range release.Assets {
		if release.Assets[i].Name == name {
			return &release.Assets[i]
		}
	}
	return nil
}

// download fetches url to path and returns its SHA256.
func download(ctx context.Context, client *http.Client, url, path string) (string, error) {
	if client == nil {
		client = &http.Client{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("stage: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("stage: download %s returned %s", url, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("stage: create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return "", fmt.Errorf("stage: create %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxBinaryBytes)); err != nil {
		return "", fmt.Errorf("stage: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("stage: sync %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractBinary pulls the server binary out of a release tarball.
//
// It searches by base name rather than by a fixed path, because the archive
// layout is the release process's business and has already varied between
// products (the separately-versioned CLI ships its binary a directory
// down). Anything that isn't a regular file is refused rather than
// followed: a tarball is untrusted input, and an entry that is a symlink or
// carries a path escaping the destination has no legitimate reason to be
// there.
func extractBinary(archivePath, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("stage: open %s: %w", archivePath, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("stage: %s is not gzip: %w", archivePath, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("stage: %s contains no %q entry", archivePath, binaryNameInArchive)
		}
		if err != nil {
			return fmt.Errorf("stage: read %s: %w", archivePath, err)
		}
		if filepath.Base(header.Name) != binaryNameInArchive {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("stage: %q in %s is not a regular file (type %q) - refusing to follow it",
				header.Name, archivePath, string(header.Typeflag))
		}
		out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return fmt.Errorf("stage: create %s: %w", destPath, err)
		}
		defer out.Close()
		if _, err := io.Copy(out, io.LimitReader(tr, maxBinaryBytes)); err != nil {
			return fmt.Errorf("stage: extract to %s: %w", destPath, err)
		}
		if err := out.Sync(); err != nil {
			return fmt.Errorf("stage: sync %s: %w", destPath, err)
		}
		return nil
	}
}

func releaseAPIBase() string        { return preflight.ReleaseAPIBase() }
func setReleaseAPIBase(base string) { preflight.SetReleaseAPIBase(base) }
