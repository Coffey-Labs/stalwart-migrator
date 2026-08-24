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
