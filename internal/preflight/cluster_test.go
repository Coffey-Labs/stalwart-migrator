package preflight

import (
	"os"
	"path/filepath"
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
