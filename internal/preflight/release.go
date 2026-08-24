// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// githubAPIBase is a var, not a const, so tests can point it at an
// httptest server instead of hitting the real GitHub API.
var githubAPIBase = "https://api.github.com/repos/stalwartlabs/stalwart/releases"

type ReleaseAsset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
	SizeBytes   int64  `json:"size"`
}

type Release struct {
	TagName string         `json:"tag_name"`
	Assets  []ReleaseAsset `json:"assets"`
}

// ResolveRelease looks up a Stalwart release from the public GitHub API.
// version is either an exact tag like "0.16.14" (a "v" prefix is added if
// missing) or "latest".
func ResolveRelease(ctx context.Context, httpClient *http.Client, version string) (*Release, error) {
	url := githubAPIBase + "/latest"
	if version != "" && version != "latest" {
		tag := version
		if !strings.HasPrefix(tag, "v") {
			tag = "v" + tag
		}
		url = githubAPIBase + "/tags/" + tag
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("preflight: fetch release %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("preflight: fetch release %s: unexpected status %s", url, resp.Status)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("preflight: parse release response from %s: %w", url, err)
	}
	return &rel, nil
}

// ChecksumAsset returns the release asset that looks like a published
// checksum manifest, if any. Its presence isn't guaranteed by Stalwart's
// release process, so callers must treat a nil result as "no independently
// published checksum to verify the download against", not an error.
func ChecksumAsset(rel *Release) *ReleaseAsset {
	for i := range rel.Assets {
		name := strings.ToLower(rel.Assets[i].Name)
		if strings.Contains(name, "sha256") || strings.Contains(name, "checksum") {
			return &rel.Assets[i]
		}
	}
	return nil
}

// ReleaseAPIBase returns the release API endpoint, and SetReleaseAPIBase
// overrides it. Both exist so other packages' tests can point release
// lookups at a local server instead of the real GitHub API.
func ReleaseAPIBase() string        { return githubAPIBase }
func SetReleaseAPIBase(base string) { githubAPIBase = base }
