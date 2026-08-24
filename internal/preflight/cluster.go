// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package preflight

import (
	"fmt"
	"os"
	"strings"
)

// LooksClustered does a conservative, heuristic scan for cluster-related
// configuration. It exists to force a manual confirmation gate (see
// ARCHITECTURE.md §4.1's cluster gate: one live node on the old version
// during migration corrupts a shared store), not to enumerate peers
// precisely. A false positive just costs an extra confirmation prompt; a
// false negative is the dangerous direction, so this errs toward matching
// broadly rather than requiring an exact schema match.
func LooksClustered(configPath string) (bool, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return false, fmt.Errorf("preflight: read config %s: %w", configPath, err)
	}
	return strings.Contains(strings.ToLower(string(data)), "cluster"), nil
}
