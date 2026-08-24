// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package plan

import (
	"fmt"
	"regexp"
	"strconv"
)

// semver is a minimal major.minor.patch version - package-local like the
// equivalent in internal/preflight, since this comparison is the only thing
// plan needs from it and duplicating ~20 lines keeps phase packages
// independent per ARCHITECTURE.md §7.
type semver struct{ Major, Minor, Patch int }

var versionPattern = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)

func parseSemver(s string) (semver, error) {
	m := versionPattern.FindStringSubmatch(s)
	if m == nil {
		return semver{}, fmt.Errorf("plan: no version number found in %q", s)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return semver{major, minor, patch}, nil
}

func (v semver) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

func (v semver) Compare(o semver) int {
	if v.Major != o.Major {
		return cmp(v.Major, o.Major)
	}
	if v.Minor != o.Minor {
		return cmp(v.Minor, o.Minor)
	}
	return cmp(v.Patch, o.Patch)
}

func cmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// PhaseName identifies one phase in an ordered migration plan - the phase
// packages this names are internal/preflight, internal/backup,
// internal/recovery, internal/cutover and internal/validate.
type PhaseName string

const (
	PhasePreflight PhaseName = "preflight"
	PhaseBackup    PhaseName = "backup"
	PhaseRecovery  PhaseName = "recovery" // only present when crossing the 0.15/0.16 boundary
	PhaseCutover   PhaseName = "cutover"
	PhaseValidate  PhaseName = "validate"
)

// Plan is the ordered list of phases one migration run needs, decided once
// from the source and target versions. See ARCHITECTURE.md §4.6: crossing
// the 0.15/0.16 boundary needs the full recovery-mode migration (§4.4); a
// same-boundary patch bump (every 0.16.1-0.16.14 release so far, per
// Stalwart's own changelog) is the fast path with no recovery phase at all.
type Plan struct {
	Phases               []PhaseName
	CrossesMajorBoundary bool
	SourceVersion        string
	TargetVersion        string
	Reason               string
}

// HasPhase reports whether name appears in the plan.
func (p *Plan) HasPhase(name PhaseName) bool {
	for _, ph := range p.Phases {
		if ph == name {
			return true
		}
	}
	return false
}

// Decide returns the plan for migrating from sourceVersion to
// targetVersion. It refuses (rather than guesses) if the source is already
// at or beyond the target, since that's not a migration this tool should
// attempt to run.
func Decide(sourceVersion, targetVersion string) (*Plan, error) {
	src, err := parseSemver(sourceVersion)
	if err != nil {
		return nil, fmt.Errorf("plan: parse source version %q: %w", sourceVersion, err)
	}
	tgt, err := parseSemver(targetVersion)
	if err != nil {
		return nil, fmt.Errorf("plan: parse target version %q: %w", targetVersion, err)
	}
	if src.Compare(tgt) >= 0 {
		return nil, fmt.Errorf("plan: source %s is already at or beyond target %s - nothing to migrate", src, tgt)
	}

	crosses := src.Major == 0 && src.Minor < 16 && (tgt.Major > 0 || tgt.Minor >= 16)
	if crosses {
		return &Plan{
			Phases:               []PhaseName{PhasePreflight, PhaseBackup, PhaseRecovery, PhaseCutover, PhaseValidate},
			CrossesMajorBoundary: true,
			SourceVersion:        src.String(),
			TargetVersion:        tgt.String(),
			Reason:               fmt.Sprintf("%s -> %s crosses the 0.15/0.16 major boundary: full recovery-mode migration required", src, tgt),
		}, nil
	}
	return &Plan{
		Phases:               []PhaseName{PhasePreflight, PhaseBackup, PhaseCutover, PhaseValidate},
		CrossesMajorBoundary: false,
		SourceVersion:        src.String(),
		TargetVersion:        tgt.String(),
		Reason:               fmt.Sprintf("%s -> %s is a same-boundary patch upgrade: fast path applies (no recovery-mode phase)", src, tgt),
	}, nil
}
