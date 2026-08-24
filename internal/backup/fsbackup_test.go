// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTree(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.db"), []byte("alpha data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.db"), []byte("bravo data, a bit longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.db", filepath.Join(root, "link-to-a")); err != nil {
		t.Fatal(err)
	}
}

func TestCopyDataDirAndVerify(t *testing.T) {
	src := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTree(t, src)

	dst := filepath.Join(t.TempDir(), "data-backup")
	manifest, err := CopyDataDir(src, dst)
	if err != nil {
		t.Fatalf("CopyDataDir: %v", err)
	}
	if len(manifest.Files) != 2 {
		t.Fatalf("manifest has %d files, want 2 (symlinks aren't hashed): %+v", len(manifest.Files), manifest.Files)
	}
	if manifest.TotalBytes != int64(len("alpha data")+len("bravo data, a bit longer")) {
		t.Errorf("TotalBytes = %d, want %d", manifest.TotalBytes, len("alpha data")+len("bravo data, a bit longer"))
	}

	// The copy should be byte-identical, including the symlink.
	got, err := os.ReadFile(filepath.Join(dst, "sub", "b.db"))
	if err != nil || string(got) != "bravo data, a bit longer" {
		t.Errorf("copied sub/b.db = %q, %v", got, err)
	}
	target, err := os.Readlink(filepath.Join(dst, "link-to-a"))
	if err != nil || target != "a.db" {
		t.Errorf("copied symlink target = %q, %v, want a.db", target, err)
	}

	if err := VerifyDataDirBackup(dst, manifest); err != nil {
		t.Errorf("VerifyDataDirBackup on an untouched copy: %v", err)
	}
}

func TestCopyDataDirRefusesSelfCopy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(dir, 0o755)
	if _, err := CopyDataDir(dir, dir); err == nil {
		t.Fatal("CopyDataDir(dir, dir) should refuse to copy a directory onto itself")
	}
}

func TestCopyDataDirRefusesNestedDestination(t *testing.T) {
	src := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(src, 0o755)
	nested := filepath.Join(src, "backup")
	if _, err := CopyDataDir(src, nested); err == nil {
		t.Fatal("CopyDataDir should refuse a destination nested inside the source")
	}
}

func TestCopyDataDirRetryClearsStaleFiles(t *testing.T) {
	src := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(src, 0o755)
	writeTree(t, src)

	dst := filepath.Join(t.TempDir(), "data-backup")
	// Simulate a stale partial copy from a previous failed attempt.
	os.MkdirAll(dst, 0o755)
	if err := os.WriteFile(filepath.Join(dst, "stale-leftover.tmp"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest, err := CopyDataDir(src, dst)
	if err != nil {
		t.Fatalf("CopyDataDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale-leftover.tmp")); !os.IsNotExist(err) {
		t.Error("stale-leftover.tmp should have been cleared by a fresh copy, but still exists")
	}
	if err := VerifyDataDirBackup(dst, manifest); err != nil {
		t.Errorf("VerifyDataDirBackup after retry: %v", err)
	}
}

func TestVerifyDataDirBackupDetectsTampering(t *testing.T) {
	src := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(src, 0o755)
	writeTree(t, src)

	dst := filepath.Join(t.TempDir(), "data-backup")
	manifest, err := CopyDataDir(src, dst)
	if err != nil {
		t.Fatalf("CopyDataDir: %v", err)
	}

	// Corrupt the backup after the fact - Verify must catch it.
	if err := os.WriteFile(filepath.Join(dst, "a.db"), []byte("corrupted!"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := VerifyDataDirBackup(dst, manifest); err == nil {
		t.Fatal("VerifyDataDirBackup should have detected the tampered file")
	}
}

func TestManifestChecksumIsDeterministic(t *testing.T) {
	src := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(src, 0o755)
	writeTree(t, src)

	dst := filepath.Join(t.TempDir(), "data-backup")
	m1, err := CopyDataDir(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	sum1, err := m1.Checksum()
	if err != nil {
		t.Fatal(err)
	}

	dst2 := filepath.Join(t.TempDir(), "data-backup-2")
	m2, err := CopyDataDir(src, dst2)
	if err != nil {
		t.Fatal(err)
	}
	sum2, err := m2.Checksum()
	if err != nil {
		t.Fatal(err)
	}

	if sum1 != sum2 {
		t.Errorf("two copies of the same source produced different manifest checksums: %s vs %s", sum1, sum2)
	}
}

func TestWriteReadManifestRoundtrip(t *testing.T) {
	m := &Manifest{
		SourceDir:  "/var/lib/stalwart",
		Files:      []ManifestEntry{{RelPath: "a.db", SHA256: "deadbeef", Size: 42}},
		TotalBytes: 42,
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := WriteManifest(path, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	got, err := ReadManifest(path)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.TotalBytes != 42 || len(got.Files) != 1 || got.Files[0].SHA256 != "deadbeef" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}
