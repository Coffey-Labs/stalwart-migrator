// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLooksClustered(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"no-cluster", "[server]\nhostname = \"mail.example.com\"\n", false},
		{"has-cluster-section", "[cluster]\nnode-id = 1\npeers = [\"10.0.0.2\"]\n", true},
		{"cluster-mentioned-in-key", "[server]\ncluster-coordinator = \"redis://localhost\"\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := LooksClustered(path)
			if err != nil {
				t.Fatalf("LooksClustered: %v", err)
			}
			if got != tc.want {
				t.Errorf("LooksClustered(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// Against a real instance the only match was inside the value of an
// unrelated setting. A bare "config mentions clustering" left a whole
// config to search; naming the location makes it dismissible at a glance.
func TestClusterMentionsSaysWhereItMatched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "server.hostname = \"mail.example.com\"\nconfig.local-keys.08 = \"cluster.*\"\n"
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	mentions, err := ClusterMentions(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(mentions) != 1 {
		t.Fatalf("mentions = %v, want one", mentions)
	}
	if !strings.Contains(mentions[0], "config.local-keys.08") {
		t.Errorf("mention = %q, should name the setting", mentions[0])
	}
	if !strings.Contains(mentions[0], "in its value") {
		t.Errorf("mention = %q, should distinguish a value match from a real cluster setting", mentions[0])
	}
}
