// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package applyplan

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Operation is one entry in a stalwart-cli apply plan. The plan file is
// NDJSON - one operation per line - which is the shape migrate_v016.py's
// own export.json uses and what `stalwart-cli apply --file` consumes.
//
// Generated operations are "upsert" with MatchOn rather than "create",
// so re-running a plan against an instance that already has some of these
// objects updates them instead of failing on a duplicate. An operator will
// run this more than once - after a failed cutover, or while iterating on
// the parts the generator can't cover - and a plan that only works on a
// pristine instance would be a trap.
type Operation struct {
	Type    string                    `json:"@type"`
	Object  string                    `json:"object"`
	MatchOn []string                  `json:"matchOn,omitempty"`
	Value   map[string]map[string]any `json:"value"`
}

// Plan is the generated apply plan plus an account of what it covers.
type Plan struct {
	Operations []Operation
	// Covered are the v0.15 setting keys the operations above account for.
	Covered []string
	// Warnings are per-setting notes where a mapping was possible but
	// lossy or assumed - surfaced rather than silently applied.
	Warnings []string
}

// WriteNDJSON writes the plan in the format `stalwart-cli apply --file`
// reads.
func (p *Plan) WriteNDJSON(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("applyplan: create %s: %w", path, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, op := range p.Operations {
		if err := enc.Encode(op); err != nil {
			return fmt.Errorf("applyplan: write %s: %w", path, err)
		}
	}
	return f.Sync()
}

// Generator turns one family of v0.15 settings into v0.16 objects.
//
// Each generator declares the key prefix it consumes and is handed every
// setting under it. Returning the keys it consumed is what lets Build
// report honest coverage: a generator that quietly skips half its input
// would otherwise look like full coverage of its prefix.
type Generator interface {
	// Prefix is the v0.15 key prefix this generator claims, e.g.
	// "server.listener.".
	Prefix() string
	// Generate maps the settings under Prefix into operations, returning
	// the keys it actually accounted for.
	Generate(settings map[string]string) (ops []Operation, covered []string, warnings []string, err error)
}

// Coverage is the honest accounting of a generated plan: what it handles,
// and what an operator still has to rebuild by hand.
//
// This matters more than the plan itself. ARCHITECTURE.md §4.3 is explicit
// that this generation is best-effort, and against a real instance the
// unmigrated set runs to five figures. A plan that covered a tenth of it
// while implying completeness would be worse than no plan at all.
type Coverage struct {
	TotalKeys     int
	CoveredKeys   int
	Remaining     []PrefixCount
	Warnings      []string
	ObjectsByType map[string]int
}

// PrefixCount is one group of still-unhandled settings.
type PrefixCount struct {
	Prefix string
	Keys   int
}

// Summary renders the coverage for an operator, largest gaps first.
func (c *Coverage) Summary(maxPrefixes int) string {
	var b strings.Builder
	pct := 0.0
	if c.TotalKeys > 0 {
		pct = 100 * float64(c.CoveredKeys) / float64(c.TotalKeys)
	}
	fmt.Fprintf(&b, "generated a plan for %d of %d unmigrated setting(s) (%.1f%%)", c.CoveredKeys, c.TotalKeys, pct)
	if len(c.ObjectsByType) > 0 {
		types := make([]string, 0, len(c.ObjectsByType))
		for t := range c.ObjectsByType {
			types = append(types, t)
		}
		sort.Strings(types)
		parts := make([]string, 0, len(types))
		for _, t := range types {
			parts = append(parts, fmt.Sprintf("%d %s", c.ObjectsByType[t], t))
		}
		fmt.Fprintf(&b, ": %s", strings.Join(parts, ", "))
	}
	if len(c.Remaining) > 0 {
		fmt.Fprintf(&b, "\n      still unhandled, largest first:")
		shown := c.Remaining
		if len(shown) > maxPrefixes {
			shown = shown[:maxPrefixes]
		}
		for _, r := range shown {
			fmt.Fprintf(&b, "\n        %-32s %d keys", r.Prefix, r.Keys)
		}
		if len(c.Remaining) > len(shown) {
			fmt.Fprintf(&b, "\n        ... and %d more prefix(es)", len(c.Remaining)-len(shown))
		}
	}
	return b.String()
}

// Build runs every registered generator over the unmigrated settings and
// returns the plan together with its coverage.
//
// settings is the full v0.15 settings dump; unmigrated is the set of keys
// migrate_v016.py reported it did not carry over. Only keys in unmigrated
// are considered: anything the official script already handles must not be
// generated a second time, or the plan would fight the conversion.
func Build(settings map[string]string, unmigrated map[string]bool, generators []Generator) (*Plan, *Coverage, error) {
	plan := &Plan{}
	coverage := &Coverage{TotalKeys: len(unmigrated), ObjectsByType: map[string]int{}}
	covered := map[string]bool{}

	for _, g := range generators {
		subset := map[string]string{}
		for k, v := range settings {
			if unmigrated[k] && strings.HasPrefix(k, g.Prefix()) {
				subset[k] = v
			}
		}
		if len(subset) == 0 {
			continue
		}
		ops, keys, warnings, err := g.Generate(subset)
		if err != nil {
			return nil, nil, fmt.Errorf("applyplan: %s: %w", g.Prefix(), err)
		}
		plan.Operations = append(plan.Operations, ops...)
		plan.Warnings = append(plan.Warnings, warnings...)
		coverage.Warnings = append(coverage.Warnings, warnings...)
		for _, op := range ops {
			coverage.ObjectsByType[op.Object] += len(op.Value)
		}
		for _, k := range keys {
			covered[k] = true
		}
	}

	for k := range covered {
		plan.Covered = append(plan.Covered, k)
	}
	sort.Strings(plan.Covered)
	coverage.CoveredKeys = len(covered)

	remaining := map[string]int{}
	for k := range unmigrated {
		if covered[k] {
			continue
		}
		remaining[groupPrefix(k)]++
	}
	for prefix, n := range remaining {
		coverage.Remaining = append(coverage.Remaining, PrefixCount{Prefix: prefix, Keys: n})
	}
	sort.Slice(coverage.Remaining, func(i, j int) bool {
		if coverage.Remaining[i].Keys != coverage.Remaining[j].Keys {
			return coverage.Remaining[i].Keys > coverage.Remaining[j].Keys
		}
		return coverage.Remaining[i].Prefix < coverage.Remaining[j].Prefix
	})
	return plan, coverage, nil
}

// groupPrefix reduces a setting key to the first two dotted segments, which
// is how migrate_v016.py's own unmigrated.txt groups them - keeping the two
// reports comparable side by side.
func groupPrefix(key string) string {
	parts := strings.Split(key, ".")
	if len(parts) <= 2 {
		return key
	}
	return parts[0] + "." + parts[1]
}
