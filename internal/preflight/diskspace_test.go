package preflight

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.bin"), make([]byte, 250), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := DirSize(dir)
	if err != nil {
		t.Fatalf("DirSize: %v", err)
	}
	if got != 350 {
		t.Errorf("DirSize = %d, want 350", got)
	}
}

func TestFreeBytes(t *testing.T) {
	got, err := FreeBytes(t.TempDir())
	if err != nil {
		t.Fatalf("FreeBytes: %v", err)
	}
	if got == 0 {
		t.Error("FreeBytes = 0, want > 0 for a live filesystem")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{500, "500 B"},
		{1536, "1.5 KiB"},
		{5 * 1024 * 1024, "5.0 MiB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
