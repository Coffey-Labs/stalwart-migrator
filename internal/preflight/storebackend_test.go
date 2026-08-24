// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package preflight

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectStoreBackendsTOML(t *testing.T) {
	toml := `
[server]
hostname = "mail.example.com"

[store."rocksdb"]
type = "rocksdb"
path = "/var/lib/stalwart/data"

[store."blob"]
type = "s3"
bucket = "stalwart-blobs"
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	matches, err := DetectStoreBackends(path)
	if err != nil {
		t.Fatalf("DetectStoreBackends: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2: %+v", len(matches), matches)
	}
	got := map[string]string{}
	for _, m := range matches {
		got[m.Path] = m.Backend
	}
	if got[`store."rocksdb"`] != "rocksdb" {
		t.Errorf(`store."rocksdb" backend = %q, want rocksdb`, got[`store."rocksdb"`])
	}
	if got[`store."blob"`] != "s3" {
		t.Errorf(`store."blob" backend = %q, want s3`, got[`store."blob"`])
	}
}

func TestDetectStoreBackendsJSON(t *testing.T) {
	jsonCfg := `{
		"store": {
			"data": {"type": "postgresql", "host": "db.internal"},
			"blob": {"type": "s3", "bucket": "stalwart-blobs"}
		}
	}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(jsonCfg), 0o644); err != nil {
		t.Fatal(err)
	}

	matches, err := DetectStoreBackends(path)
	if err != nil {
		t.Fatalf("DetectStoreBackends: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2: %+v", len(matches), matches)
	}
	backends := map[string]bool{}
	for _, m := range matches {
		backends[m.Backend] = true
	}
	if !backends["postgresql"] || !backends["s3"] {
		t.Errorf("matches = %+v, want postgresql and s3", matches)
	}
}

func TestDetectStoreBackendsNoMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[server]\nhostname = \"mail.example.com\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	matches, err := DetectStoreBackends(path)
	if err != nil {
		t.Fatalf("DetectStoreBackends: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("got %d matches, want 0: %+v", len(matches), matches)
	}
}

// A real production config declares its backend with flat dotted keys and
// has no section headers at all. Checking only for a bare `type` key found
// nothing there, and an undetected backend makes the backup phase skip the
// filesystem snapshot - the artifact it exists to produce.
func TestDetectStoreBackendsHandlesFlatDottedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// Shape taken from a real 0.15.5 instance: no [sections] anywhere.
	body := `storage.blob = "rocksdb"
storage.data = "rocksdb"
storage.directory = "internal"
store.rocksdb.compression = "lz4"
store.rocksdb.path = "/opt/stalwart/data"
store.rocksdb.type = "rocksdb"
directory.internal.type = "internal"
`
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	matches, err := DetectStoreBackends(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d backend(s), want 1: %+v", len(matches), matches)
	}
	if matches[0].Backend != "rocksdb" {
		t.Errorf("backend = %q, want rocksdb", matches[0].Backend)
	}
	if matches[0].Path != "store.rocksdb" {
		t.Errorf("path = %q, want store.rocksdb", matches[0].Path)
	}
}

// The sectioned form must keep working; both spellings are real.
func TestDetectStoreBackendsStillHandlesSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[store.rocksdb]\ntype = \"rocksdb\"\npath = \"/var/lib/stalwart\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	matches, err := DetectStoreBackends(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Backend != "rocksdb" || matches[0].Path != "store.rocksdb" {
		t.Errorf("matches = %+v, want one rocksdb at store.rocksdb", matches)
	}
}
