// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package cutover

import (
	"fmt"
	"strings"
)

type Status string

const (
	StatusOK      Status = "ok"
	StatusWarn    Status = "warn"
	StatusSkipped Status = "skip"
	StatusFail    Status = "fail"
)

type CheckResult struct {
	Name   string
	Status Status
	Detail string
}

type Report struct {
	Results []CheckResult
}

// Blocking reports whether anything failed outright. A warning does not
// block: the one step allowed to warn is quota recalculation, which leaves
// counters stale rather than mail unreachable (see Run).
func (r Report) Blocking() bool {
	for _, res := range r.Results {
		if res.Status == StatusFail {
			return true
		}
	}
	return false
}

func (r Report) String() string {
	var b strings.Builder
	for _, res := range r.Results {
		fmt.Fprintf(&b, "[%-4s] %-24s %s\n", strings.ToUpper(string(res.Status)), res.Name, res.Detail)
	}
	return b.String()
}
