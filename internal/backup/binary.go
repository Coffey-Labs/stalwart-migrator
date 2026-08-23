package backup

import (
	"fmt"
	"os"
)

// PreserveBinary moves the currently-installed binary aside to
// "<binaryPath>.v<sourceVersion>" so rollback can restart the exact old
// binary without re-downloading anything, and cutover can install the new
// one at the original path. It never deletes the old binary, and it's
// idempotent: if a prior attempt at this run already preserved it, calling
// this again just returns the existing preserved path rather than erroring
// on a missing source file.
func PreserveBinary(binaryPath, sourceVersion string) (preservedPath string, err error) {
	if sourceVersion == "" {
		return "", fmt.Errorf("backup: cannot preserve %s without a source version to suffix it with", binaryPath)
	}
	preservedPath = binaryPath + ".v" + sourceVersion

	if _, statErr := os.Stat(preservedPath); statErr == nil {
		return preservedPath, nil
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("backup: stat %s: %w", preservedPath, statErr)
	}

	if err := os.Rename(binaryPath, preservedPath); err != nil {
		return "", fmt.Errorf("backup: preserve %s as %s: %w", binaryPath, preservedPath, err)
	}
	return preservedPath, nil
}
