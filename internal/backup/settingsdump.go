// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// DefaultMigrationScriptURL is Stalwart's own v0.15->v0.16 settings
// converter, referenced directly from UPGRADING/v0_16.md. It's an external,
// Stalwart-owned dependency this tool doesn't vendor a copy of - see the
// pinning discussion on DownloadFile and ARCHITECTURE.md §8.
const DefaultMigrationScriptURL = "https://raw.githubusercontent.com/stalwartlabs/stalwart/main/resources/scripts/migrate_v016.py"

// DownloadFile fetches url to destPath and returns its SHA256. If
// expectedSHA256 is non-empty, a mismatching download is rejected (and the
// partial file removed) - this is how a pinned migration-script hash is
// enforced, so a run never silently executes a different version of a
// script than the one it was reviewed against. If expectedSHA256 is empty,
// the download is accepted unconditionally and its hash is returned so the
// caller can record it as the pin for next time; first-run trust-on-first-use
// is a known gap, flagged in ARCHITECTURE.md §8.
func DownloadFile(ctx context.Context, httpClient *http.Client, url, destPath, expectedSHA256 string) (sha256Hex string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("backup: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("backup: fetch %s: unexpected status %s", url, resp.Status)
	}

	f, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return "", fmt.Errorf("backup: create %s: %w", destPath, err)
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(destPath)
		return "", fmt.Errorf("backup: download %s: %w", url, copyErr)
	}
	if closeErr != nil {
		os.Remove(destPath)
		return "", fmt.Errorf("backup: close %s: %w", destPath, closeErr)
	}

	sha256Hex = hex.EncodeToString(h.Sum(nil))
	if expectedSHA256 != "" && sha256Hex != expectedSHA256 {
		os.Remove(destPath)
		return "", fmt.Errorf(
			"backup: %s checksum mismatch: got %s, want %s (refusing to run an unexpected version of a script that irreversibly wipes settings on first v0.16 start)",
			url, sha256Hex, expectedSHA256,
		)
	}
	return sha256Hex, nil
}

// SettingsDumpOptions configures a migrate_v016.py `dump` invocation
// against a live v0.15.x instance - see UPGRADING/v0_16.md.
type SettingsDumpOptions struct {
	PythonPath     string // defaults to "python3"
	ScriptPath     string // local path to the already-downloaded, checksum-verified script
	URL            string // the running v0.15.x instance's base URL
	Username       string
	Password       string
	SettingsPath   string
	PrincipalsPath string
}

// RunSettingsDump runs migrate_v016.py's dump subcommand, which reads the
// live v0.15.x server's settings and principals over its admin API and
// writes them to SettingsPath/PrincipalsPath for the later convert step
// (ARCHITECTURE.md §4.3). This step is read-only against the server, so
// it's safe to run well before cutover - ARCHITECTURE.md §4.2 calls for it
// both at preflight time and again immediately before cutover, since the
// live settings may have changed in between.
func RunSettingsDump(ctx context.Context, o SettingsDumpOptions) error {
	python := o.PythonPath
	if python == "" {
		python = "python3"
	}
	args := []string{
		o.ScriptPath, "dump",
		"--url", o.URL,
		"--username", o.Username,
		"--password", o.Password,
		"--settings", o.SettingsPath,
		"--principals", o.PrincipalsPath,
	}
	cmd := exec.CommandContext(ctx, python, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("backup: migrate_v016.py dump failed: %w (output: %s)", err, out)
	}
	return nil
}

// SettingsConvertOptions configures a migrate_v016.py `convert` invocation,
// which turns the settings/principals dump into the v0.16 config.json and
// export.json that recovery mode consumes - see UPGRADING/v0_16.md.
type SettingsConvertOptions struct {
	PythonPath     string // defaults to "python3"
	ScriptPath     string
	SettingsPath   string
	PrincipalsPath string
	ConfigPath     string // output: config.json for the new binary's --config flag
	OutputPath     string // output: export.json for `stalwart-cli apply`

	// PatchPaths rewrites path prefixes in the generated config (documented
	// for Docker deployments as "--patch-paths /opt/stalwart=/var/lib/stalwart",
	// e.g. old-path -> new-path). This is the officially documented
	// mechanism a dry-run relies on to point the generated config at a
	// sandbox data directory instead of the production one - see
	// ARCHITECTURE.md's dry-run design - rather than this tool editing
	// config.json's contents directly, which would require depending on its
	// exact schema.
	PatchPaths map[string]string
}

// RunSettingsConvert runs migrate_v016.py's convert subcommand.
func RunSettingsConvert(ctx context.Context, o SettingsConvertOptions) error {
	python := o.PythonPath
	if python == "" {
		python = "python3"
	}
	args := []string{
		o.ScriptPath, "convert",
		"--settings", o.SettingsPath,
		"--principals", o.PrincipalsPath,
		"--config", o.ConfigPath,
		"--output", o.OutputPath,
	}
	if len(o.PatchPaths) > 0 {
		pairs := make([]string, 0, len(o.PatchPaths))
		for old, new := range o.PatchPaths {
			pairs = append(pairs, old+"="+new)
		}
		sort.Strings(pairs) // deterministic argv, easier to test and to log
		args = append(args, "--patch-paths", strings.Join(pairs, ","))
	}
	cmd := exec.CommandContext(ctx, python, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("backup: migrate_v016.py convert failed: %w (output: %s)", err, out)
	}
	return nil
}
