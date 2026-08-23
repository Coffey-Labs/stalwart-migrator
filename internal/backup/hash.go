package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// hashFile returns a file's SHA256 and size, for recording as a
// checkpoint.Artifact.
func hashFile(path string) (sha256Hex string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("backup: hash %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("backup: hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
