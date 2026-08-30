// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"syscall"
)

// matchOwnership gives paths the uid and gid of reference.
//
// It exists for the container path, where this tool writes the migrated
// config onto the host side of a volume and something inside the container
// has to read it. That something is not root: the official Stalwart image
// runs as uid 2000, so a config written root-owned and 0640 is a config
// the server cannot open, and the failure arrives as a recovery boot that
// will not come up rather than as a permissions error anyone would
// recognise.
//
// The reference is the data directory itself, whose ownership is already
// whatever the container writes as. Copying it is more reliable than
// resolving a user out of the image, and it is the same move
// cutover.installConfig makes for the systemd path with
// ConfigOwnerReference.
//
// ARCHITECTURE.md §4.8 is the reason this is not left to chance: the
// rollback that was removed from this tool restored every byte correctly
// and lost ownership, and reported success.
func matchOwnership(reference string, paths ...string) (string, error) {
	info, err := os.Stat(reference)
	if err != nil {
		return "", fmt.Errorf("stat %s to copy its ownership: %w", reference, err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "ownership unchanged (this platform does not report it)", nil
	}
	uid, gid := int(sys.Uid), int(sys.Gid)
	for _, p := range paths {
		if err := os.Chown(p, uid, gid); err != nil {
			return "", fmt.Errorf(
				"set ownership on %s to %d:%d, which is what owns %s - the server in the container runs as that user and "+
					"cannot read a config it does not own: %w", p, uid, gid, reference, err)
		}
	}
	return fmt.Sprintf("uid %d, gid %d, copied from %s", uid, gid, reference), nil
}
