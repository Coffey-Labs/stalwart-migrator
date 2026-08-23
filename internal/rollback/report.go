package rollback

import (
	"fmt"
	"strings"
)

type Status string

const (
	StatusOK      Status = "ok"
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

// Blocking reports whether anything failed. A rollback report that isn't
// clean means the instance is in an unknown state - never quietly "rolled
// back".
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
		fmt.Fprintf(&b, "[%-4s] %-22s %s\n", strings.ToUpper(string(res.Status)), res.Name, res.Detail)
	}
	return b.String()
}
