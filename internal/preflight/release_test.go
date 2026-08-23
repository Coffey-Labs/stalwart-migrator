package preflight

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func withFakeGithub(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = old })
}

func TestResolveReleaseLatest(t *testing.T) {
	withFakeGithub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/latest" {
			t.Errorf("path = %s, want /latest", r.URL.Path)
		}
		json.NewEncoder(w).Encode(Release{
			TagName: "v0.16.14",
			Assets: []ReleaseAsset{
				{Name: "stalwart-x86_64-linux", DownloadURL: "https://example.com/stalwart"},
				{Name: "checksums.txt", DownloadURL: "https://example.com/checksums.txt"},
			},
		})
	})

	rel, err := ResolveRelease(context.Background(), nil, "latest")
	if err != nil {
		t.Fatalf("ResolveRelease: %v", err)
	}
	if rel.TagName != "v0.16.14" {
		t.Errorf("TagName = %s, want v0.16.14", rel.TagName)
	}
	if asset := ChecksumAsset(rel); asset == nil || asset.Name != "checksums.txt" {
		t.Errorf("ChecksumAsset = %+v, want checksums.txt", asset)
	}
}

func TestResolveReleaseExactTag(t *testing.T) {
	withFakeGithub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tags/v0.16.5" {
			t.Errorf("path = %s, want /tags/v0.16.5", r.URL.Path)
		}
		json.NewEncoder(w).Encode(Release{TagName: "v0.16.5"})
	})

	rel, err := ResolveRelease(context.Background(), nil, "0.16.5")
	if err != nil {
		t.Fatalf("ResolveRelease: %v", err)
	}
	if rel.TagName != "v0.16.5" {
		t.Errorf("TagName = %s, want v0.16.5", rel.TagName)
	}
}

func TestChecksumAssetAbsent(t *testing.T) {
	rel := &Release{Assets: []ReleaseAsset{{Name: "stalwart-x86_64-linux"}}}
	if asset := ChecksumAsset(rel); asset != nil {
		t.Errorf("ChecksumAsset = %+v, want nil", asset)
	}
}
