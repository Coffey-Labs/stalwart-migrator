// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Manifest records the per-file checksums produced by CopyDataDir, so a
// later Verify pass can detect corruption or truncation introduced by the
// copy itself. It is NOT authentication that the copied store is a valid,
// openable Stalwart database - confirming that would mean booting the old
// binary read-only against the backup, which needs config/CLI details this
// tool doesn't verify yet (see the parallel caveat on
// stalwartapi.Client.AccountSnapshot). Treat a clean Verify as "the bytes we
// wrote match the bytes we copied", not "Stalwart can open this".
type Manifest struct {
	SourceDir  string          `json:"source_dir"`
	Files      []ManifestEntry `json:"files"`
	TotalBytes int64           `json:"total_bytes"`
}

type ManifestEntry struct {
	RelPath string `json:"rel_path"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// Checksum returns a single content hash identifying this manifest (and
// transitively, the exact set of files and bytes it describes), suitable
// for recording as a checkpoint.Artifact's SHA256 - a directory doesn't
// have one natural hash, so this stands in for it.
func (m *Manifest) Checksum() (string, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("backup: marshal manifest: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// WriteManifest persists a manifest as JSON next to the backup it describes.
func WriteManifest(path string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: marshal manifest: %w", err)
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return fmt.Errorf("backup: write manifest %s: %w", path, err)
	}
	return nil
}

// ReadManifest loads a manifest previously written by WriteManifest.
func ReadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("backup: read manifest %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("backup: parse manifest %s: %w", path, err)
	}
	return &m, nil
}

// CopyDataDir copies srcDir to dstDir the way `cp -a` would (directories,
// regular files, and symlinks, preserving regular-file permissions),
// hashing every regular file as it's written. dstDir is cleared first if it
// already exists, so a retried step after a partial failure produces a
// clean copy rather than a mix of old and new files - filesystem copies
// aren't resumable at the byte level in any way worth building here, so a
// retry just redoes the whole thing.
func CopyDataDir(srcDir, dstDir string) (*Manifest, error) {
	srcAbs, err := filepath.Abs(srcDir)
	if err != nil {
		return nil, fmt.Errorf("backup: resolve source %s: %w", srcDir, err)
	}
	dstAbs, err := filepath.Abs(dstDir)
	if err != nil {
		return nil, fmt.Errorf("backup: resolve destination %s: %w", dstDir, err)
	}
	if srcAbs == dstAbs {
		return nil, fmt.Errorf("backup: source and destination are the same path: %s", srcAbs)
	}
	if strings.HasPrefix(dstAbs+string(filepath.Separator), srcAbs+string(filepath.Separator)) {
		return nil, fmt.Errorf("backup: destination %s is inside source %s - refusing to copy a directory into itself", dstAbs, srcAbs)
	}
	if err := os.RemoveAll(dstAbs); err != nil {
		return nil, fmt.Errorf("backup: clear stale destination %s: %w", dstAbs, err)
	}
	if err := os.MkdirAll(dstAbs, 0o750); err != nil {
		return nil, fmt.Errorf("backup: create destination %s: %w", dstAbs, err)
	}

	manifest := &Manifest{SourceDir: srcAbs}
	walkErr := filepath.WalkDir(srcAbs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcAbs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dstPath := filepath.Join(dstAbs, rel)

		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", path, err)
			}
			return os.Symlink(target, dstPath)
		}
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(dstPath, info.Mode().Perm())
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("backup: unsupported file type at %s (mode %s)", path, info.Mode())
		}
		sum, size, err := copyFileWithChecksum(path, dstPath, info.Mode().Perm())
		if err != nil {
			return fmt.Errorf("copy %s: %w", path, err)
		}
		manifest.Files = append(manifest.Files, ManifestEntry{RelPath: rel, SHA256: sum, Size: size})
		manifest.TotalBytes += size
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("backup: copy %s to %s: %w", srcAbs, dstAbs, walkErr)
	}
	return manifest, nil
}

func copyFileWithChecksum(src, dst string, perm fs.FileMode) (sha256Hex string, size int64, err error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return "", 0, err
	}
	defer out.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		return "", 0, err
	}
	if err := out.Sync(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// VerifyDataDirBackup recomputes every file's checksum under dstDir and
// compares it against the manifest CopyDataDir produced, returning a single
// error describing every mismatch found (not just the first) so a human
// sees the full extent of any corruption in one pass.
func VerifyDataDirBackup(dstDir string, m *Manifest) error {
	var problems []string
	for _, entry := range m.Files {
		path := filepath.Join(dstDir, entry.RelPath)
		f, err := os.Open(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", entry.RelPath, err))
			continue
		}
		h := sha256.New()
		size, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: read error: %v", entry.RelPath, err))
			continue
		}
		if size != entry.Size {
			problems = append(problems, fmt.Sprintf("%s: size %d, want %d", entry.RelPath, size, entry.Size))
			continue
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != entry.SHA256 {
			problems = append(problems, fmt.Sprintf("%s: sha256 %s, want %s", entry.RelPath, got, entry.SHA256))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("backup: verification failed for %d of %d file(s):\n%s", len(problems), len(m.Files), strings.Join(problems, "\n"))
	}
	return nil
}
