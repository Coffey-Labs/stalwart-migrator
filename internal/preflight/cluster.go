// SPDX-FileCopyrightText: 2026 Coffey Labs
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
	locations, err := ClusterMentions(configPath)
	if err != nil {
		return false, err
	}
	return len(locations) > 0, nil
}

// ClusterMentions returns the setting keys whose name or value mentions
// clustering, so a warning can say *where* it matched.
//
// The matching stays deliberately broad - a missed cluster is the dangerous
// direction - but a bare "config mentions clustering" leaves an operator
// with a whole config to search. Against a real instance the single match
// was inside the value of an unrelated key, which is obvious in one glance
// when the warning names it and needs investigation when it doesn't.
func ClusterMentions(configPath string) ([]string, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("preflight: read config %s: %w", configPath, err)
	}
	var locations []string
	section := ""
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			// A [cluster] header is itself the declaration; don't let
			// recording the section swallow the match.
			if strings.Contains(strings.ToLower(section), "cluster") {
				locations = append(locations, "["+section+"]")
			}
			continue
		}
		if !strings.Contains(strings.ToLower(line), "cluster") {
			continue
		}
		key := line
		if eq := strings.Index(line, "="); eq >= 0 {
			key = strings.TrimSpace(line[:eq])
		}
		where := key
		if section != "" {
			where = section + "." + key
		}
		// Say whether it was the setting itself or only its value, since
		// that is the difference between "this is clustered" and "this
		// mentions the word".
		if !strings.Contains(strings.ToLower(key), "cluster") {
			where += " (in its value, not the setting name)"
		}
		locations = append(locations, where)
	}
	return locations, nil
}
