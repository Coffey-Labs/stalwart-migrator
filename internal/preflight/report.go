// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package preflight

import (
	"fmt"
	"strings"
)

// Status is a single check's verdict.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

// CheckResult is one named check's outcome.
type CheckResult struct {
	Name   string
	Status Status
	Detail string
}

// Report is the full set of preflight check outcomes.
type Report struct {
	Results []CheckResult
}

// Blocking reports whether any check failed hard enough that a run must
// not proceed.
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
		fmt.Fprintf(&b, "[%-4s] %-20s %s\n", strings.ToUpper(string(res.Status)), res.Name, res.Detail)
	}
	return b.String()
}
