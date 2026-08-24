// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package backup

import (
	"fmt"
	"strings"
)

// Status is a single backup step's verdict. Unlike preflight, most backup
// steps that fail are hard failures (Run aborts) rather than advisory - see
// Run's doc comment - but Status still distinguishes "did the thing" from
// "correctly skipped because it doesn't apply to this deployment".
type Status string

const (
	StatusOK      Status = "ok"
	StatusSkipped Status = "skipped"
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

func (r Report) String() string {
	var b strings.Builder
	for _, res := range r.Results {
		fmt.Fprintf(&b, "[%-7s] %-18s %s\n", strings.ToUpper(string(res.Status)), res.Name, res.Detail)
	}
	return b.String()
}
