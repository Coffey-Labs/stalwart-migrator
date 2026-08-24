// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package validate

import (
	"fmt"
	"strings"
)

type Status string

const (
	StatusOK   Status = "ok"
	StatusFail Status = "fail"
	// StatusSkip is a check that could not be performed. It is deliberately
	// not StatusOK: "every account survived" and "we were unable to look"
	// are different answers, and reporting the second as the first is the
	// failure mode ARCHITECTURE.md §4.7 warns about.
	StatusSkip Status = "skip"
)

type CheckResult struct {
	Name   string
	Status Status
	Detail string
}

type Report struct {
	Results []CheckResult
}

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
		fmt.Fprintf(&b, "[%-4s] %-16s %s\n", strings.ToUpper(string(res.Status)), res.Name, res.Detail)
	}
	return b.String()
}
