// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package preflight

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// knownBackends is the set of store/blob/FTS backend identifiers Stalwart's
// documentation names: RocksDB, SQLite, FoundationDB, PostgreSQL, MySQL,
// S3-compatible storage, and Elasticsearch.
var knownBackends = map[string]bool{
	"rocksdb": true, "sqlite": true, "foundationdb": true,
	"postgresql": true, "mysql": true, "s3": true, "elasticsearch": true,
}

// BackendMatch is one "type = <backend>" assignment found in a config
// file, tagged with where it was found (the enclosing TOML section, or the
// dotted JSON key path).
type BackendMatch struct {
	Path    string
	Backend string
}

// DetectStoreBackends scans a Stalwart config file for store/blob/FTS
// backend declarations. It deliberately does not assume one fixed schema
// path: the exact TOML (pre-0.16) or JSON (0.16+) layout has already
// changed once between those versions and may change again, so this
// searches structurally for `type = "<known backend>"` assignments wherever
// they appear and reports every match with its location. Treat the result
// as "here's what to confirm before an unattended run", not ground truth.
func DetectStoreBackends(configPath string) ([]BackendMatch, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("preflight: read config %s: %w", configPath, err)
	}
	if json.Valid(data) {
		return scanJSONBackends(data)
	}
	return scanTOMLBackends(data)
}

var (
	tomlSectionRe = regexp.MustCompile(`^\[(.+)\]$`)
	tomlKVRe      = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\s*=\s*"([^"]*)"$`)
)

func scanTOMLBackends(data []byte) ([]BackendMatch, error) {
	var matches []BackendMatch
	section := ""
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := tomlSectionRe.FindStringSubmatch(line); m != nil {
			section = m[1]
			continue
		}
		if m := tomlKVRe.FindStringSubmatch(line); m != nil {
			key, value := m[1], strings.ToLower(m[2])
			if !knownBackends[value] {
				continue
			}
			// Two spellings, both real. A config written with sections
			// declares `type = "rocksdb"` under `[store.rocksdb]`; one
			// written flat declares `store.rocksdb.type = "rocksdb"` with
			// no sections at all. A real production instance uses the
			// second form exclusively - checking only for a bare `type`
			// key found nothing there, and an undetected backend makes the
			// backup phase skip the filesystem snapshot entirely.
			switch {
			case key == "type":
				matches = append(matches, BackendMatch{Path: section, Backend: value})
			case strings.HasSuffix(key, ".type"):
				path := strings.TrimSuffix(key, ".type")
				if section != "" {
					path = section + "." + path
				}
				matches = append(matches, BackendMatch{Path: path, Backend: value})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("preflight: scan toml config: %w", err)
	}
	return matches, nil
}

func scanJSONBackends(data []byte) ([]BackendMatch, error) {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("preflight: parse json config: %w", err)
	}
	var matches []BackendMatch
	walkJSONForBackends(root, "", &matches)
	return matches, nil
}

func walkJSONForBackends(node any, path string, matches *[]BackendMatch) {
	switch v := node.(type) {
	case map[string]any:
		if t, ok := v["type"].(string); ok && knownBackends[strings.ToLower(t)] {
			*matches = append(*matches, BackendMatch{Path: path, Backend: strings.ToLower(t)})
		}
		for k, child := range v {
			childPath := k
			if path != "" {
				childPath = path + "." + k
			}
			walkJSONForBackends(child, childPath, matches)
		}
	case []any:
		for i, child := range v {
			walkJSONForBackends(child, fmt.Sprintf("%s[%d]", path, i), matches)
		}
	}
}
