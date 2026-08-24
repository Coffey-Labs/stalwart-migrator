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
