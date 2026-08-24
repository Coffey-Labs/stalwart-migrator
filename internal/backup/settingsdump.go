// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultMigrationScriptURL is Stalwart's own v0.15->v0.16 settings
// converter, referenced directly from UPGRADING/v0_16.md. It's an external,
// Stalwart-owned dependency this tool doesn't vendor a copy of - see the
// pinning discussion on DownloadFile and ARCHITECTURE.md §8.
const DefaultMigrationScriptURL = "https://raw.githubusercontent.com/stalwartlabs/stalwart/main/resources/scripts/migrate_v016.py"

// ProvideFile puts the migration script at destPath, from srcPath when one
// is given and from url otherwise, and returns its SHA256.
//
// A local copy is not only a convenience for testing. A mail server with no
// route to the internet - an air-gapped host, or a clone deliberately cut
// off so it cannot renew certificates or deliver queued mail for the domains
// it was copied from - cannot fetch anything, and could not be migrated at
// all without this.
func ProvideFile(ctx context.Context, httpClient *http.Client, srcPath, url, destPath, expectedSHA256 string) (sha256Hex string, err error) {
	if srcPath == "" {
		return DownloadFile(ctx, httpClient, url, destPath, expectedSHA256)
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", fmt.Errorf("backup: read %s: %w", srcPath, err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(data))
	if expectedSHA256 != "" && !strings.EqualFold(sum, expectedSHA256) {
		return "", fmt.Errorf("backup: %s has sha256 %s, expected %s", srcPath, sum, expectedSHA256)
	}
	if err := os.WriteFile(destPath, data, 0o640); err != nil {
		return "", fmt.Errorf("backup: write %s: %w", destPath, err)
	}
	return sum, nil
}

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

	// WorkDir is where the script runs. It matters more than it looks:
	// migrate_v016.py writes its unmigrated.txt report into the current
	// working directory, so without this the convert either fails outright
	// (an unwritable CWD - which is what happens running as a service from
	// /) or silently drops the single most important output of the whole
	// migration wherever the operator happened to be standing.
	WorkDir string
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
	if o.WorkDir != "" {
		if err := os.MkdirAll(o.WorkDir, 0o750); err != nil {
			return fmt.Errorf("backup: create convert working directory %s: %w", o.WorkDir, err)
		}
		cmd.Dir = o.WorkDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("backup: migrate_v016.py convert failed: %w (output: %s)", err, out)
	}
	return nil
}

// UnmigratedPrefix is one group of v0.15 settings the conversion did not
// carry over, as reported by migrate_v016.py's unmigrated.txt.
type UnmigratedPrefix struct {
	Prefix string
	Keys   int
}

// UnmigratedReport summarizes what a conversion left behind.
//
// This is not a footnote. Against a real production instance - 12,401
// settings - Stalwart's own converter migrated 219 of them, 1.8%, and left
// 12,182 for the operator to recreate by hand: spam-filter rules, DNSBLs,
// trusted-domain and URL-redirector lookups, queue scheduling and TLS
// settings, and server.listener itself, which is why a freshly migrated
// instance answers on none of the ports the old one did. A migration that
// reported success while silently discarding this would be worse than one
// that failed.
type UnmigratedReport struct {
	Path      string
	TotalKeys int
	Prefixes  []UnmigratedPrefix
}

// Summary renders the report for an operator, largest groups first.
func (r *UnmigratedReport) Summary(maxPrefixes int) string {
	if r == nil || r.TotalKeys == 0 {
		return "no unmigrated settings were reported"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d v0.15 setting(s) were NOT migrated and must be recreated by hand (full list: %s)", r.TotalKeys, r.Path)
	prefixes := r.Prefixes
	if len(prefixes) > maxPrefixes {
		prefixes = prefixes[:maxPrefixes]
	}
	for _, p := range prefixes {
		fmt.Fprintf(&b, "\n      %-32s %d keys", p.Prefix, p.Keys)
	}
	if len(r.Prefixes) > len(prefixes) {
		fmt.Fprintf(&b, "\n      ... and %d more prefix(es)", len(r.Prefixes)-len(prefixes))
	}
	return b.String()
}

// unmigratedPattern matches the report's per-prefix lines, e.g.
// "  spam-filter.rule                       424 keys".
var unmigratedPattern = regexp.MustCompile(`^\s+(\S+)\s+(\d+) keys\s*$`)

// ReadUnmigratedReport parses the unmigrated.txt migrate_v016.py writes
// beside its output. A missing file is not an error - an older script, or
// a conversion with nothing left over, simply won't produce one - so
// callers get a nil report rather than a failure.
func ReadUnmigratedReport(path string) (*UnmigratedReport, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("backup: read %s: %w", path, err)
	}
	report := &UnmigratedReport{Path: path}
	for _, line := range strings.Split(string(data), "\n") {
		if m := unmigratedPattern.FindStringSubmatch(line); m != nil {
			n, convErr := strconv.Atoi(m[2])
			if convErr != nil {
				continue
			}
			report.Prefixes = append(report.Prefixes, UnmigratedPrefix{Prefix: m[1], Keys: n})
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "Total unmigrated keys:") {
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				if n, convErr := strconv.Atoi(fields[3]); convErr == nil {
					report.TotalKeys = n
				}
			}
		}
	}
	sort.Slice(report.Prefixes, func(i, j int) bool { return report.Prefixes[i].Keys > report.Prefixes[j].Keys })
	return report, nil
}

// ReadSettingsDump loads the flat {key: value} settings map
// migrate_v016.py's dump step writes.
func ReadSettingsDump(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("backup: read settings dump %s: %w", path, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("backup: parse settings dump %s: %w", path, err)
	}
	settings := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			settings[k] = s
			continue
		}
		settings[k] = fmt.Sprint(v)
	}
	return settings, nil
}

// ReadUnmigratedKeys returns the set of settings keys migrate_v016.py
// reported it did not carry over.
//
// unmigrated.txt lists prefixes and counts rather than individual keys, so
// this expands those prefixes against the settings dump. That is why it
// needs both files: the report says "spam-filter.rule: 424 keys", and only
// the dump knows which 424.
func ReadUnmigratedKeys(reportPath string, settings map[string]string) (map[string]bool, error) {
	report, err := ReadUnmigratedReport(reportPath)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	if report == nil {
		return keys, nil
	}
	for _, p := range report.Prefixes {
		for k := range settings {
			if k == p.Prefix || strings.HasPrefix(k, p.Prefix+".") {
				keys[k] = true
			}
		}
	}
	return keys, nil
}

// Principal is the subset of a v0.15 principals dump this tool needs. The
// shape is what the REST management API returns and what
// migrate_v016.py's dump step writes out verbatim.
type Principal struct {
	ID     int      `json:"id"`
	Type   string   `json:"type"` // "individual", "group", "domain", ...
	Name   string   `json:"name"`
	Emails []string `json:"emails"`
	Roles  []string `json:"roles"`
}

// ReadPrincipalsDump loads the principals dump written alongside the
// settings dump.
func ReadPrincipalsDump(path string) ([]Principal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("backup: read principals dump %s: %w", path, err)
	}
	var principals []Principal
	if err := json.Unmarshal(data, &principals); err != nil {
		return nil, fmt.Errorf("backup: parse principals dump %s: %w", path, err)
	}
	return principals, nil
}
