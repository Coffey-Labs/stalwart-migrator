// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package validate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
)

// A run that never captured a "before" cannot be checked against one. The
// point of these two is that such a run reports as unchecked rather than as
// passing: ARCHITECTURE.md §4.7 is explicit that this suite must not imply a
// guarantee it did not measure.

func TestRunLiveSkipsWithoutSnapshot(t *testing.T) {
	store, rs := newRun(t)
	report, err := RunLive(context.Background(), store, rs, LiveOptions{AdminURL: "https://mail.example.org", Before: nil})
	if err != nil {
		t.Fatalf("RunLive: %v", err)
	}
	if got := report.Results[0].Status; got != StatusSkip {
		t.Fatalf("status = %q, want %q", got, StatusSkip)
	}
	if report.Blocking() {
		t.Fatal("a skipped check must not block the run")
	}
	if !strings.Contains(report.Results[0].Detail, "nothing to compare") {
		t.Fatalf("detail should say why it was skipped, got %q", report.Results[0].Detail)
	}
}

func TestRunLiveSkipsWithoutAdminURL(t *testing.T) {
	store, rs := newRun(t)
	report, err := RunLive(context.Background(), store, rs, LiveOptions{Before: &checkpoint.PreflightSnapshot{}})
	if err != nil {
		t.Fatalf("RunLive: %v", err)
	}
	if got := report.Results[0].Status; got != StatusSkip {
		t.Fatalf("status = %q, want %q", got, StatusSkip)
	}
}

func TestRunLivePassesWhenEverythingSurvived(t *testing.T) {
	srv := fakeInstance(t, []string{"example.org"}, map[string]float64{"ann@example.org": 10, "bob@example.org": 20})
	defer srv.Close()

	store, rs := newRun(t)
	report, err := RunLive(context.Background(), store, rs, LiveOptions{
		AdminURL: srv.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: srv.Client(),
		Before: &checkpoint.PreflightSnapshot{
			Domains:   []string{"example.org"},
			UsedQuota: map[string]int64{"ann@example.org": 1, "bob@example.org": 2},
		},
	})
	if err != nil {
		t.Fatalf("RunLive: %v", err)
	}
	if report.Blocking() {
		t.Fatalf("expected a pass, got: %s", report.String())
	}
	if got := report.Results[0].Status; got != StatusOK {
		t.Fatalf("status = %q, want %q", got, StatusOK)
	}
}

func TestRunLiveFailsWhenAnAccountIsMissing(t *testing.T) {
	// bob did not make it across.
	srv := fakeInstance(t, []string{"example.org"}, map[string]float64{"ann@example.org": 10})
	defer srv.Close()

	store, rs := newRun(t)
	report, err := RunLive(context.Background(), store, rs, LiveOptions{
		AdminURL: srv.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: srv.Client(),
		Before: &checkpoint.PreflightSnapshot{
			Domains:   []string{"example.org"},
			UsedQuota: map[string]int64{"ann@example.org": 1, "bob@example.org": 2},
		},
	})
	// The comparison ran and found something: that is a finding, not an
	// error, so the step completes and the report carries the verdict.
	if err != nil {
		t.Fatalf("RunLive returned an error for a completed comparison: %v", err)
	}
	if !report.Blocking() {
		t.Fatalf("a missing account must block, got: %s", report.String())
	}
	if !strings.Contains(report.Results[0].Detail, "bob@example.org") {
		t.Fatalf("the report should name the missing account, got %q", report.Results[0].Detail)
	}
	// And it must be recorded, so `report <run-id>` can say so afterwards.
	step := rs.Outcome(checkpoint.PhaseValidate, "content-integrity")
	if step.Verdict != string(StatusFail) {
		t.Fatalf("checkpoint verdict = %q, want %q", step.Verdict, StatusFail)
	}
}

func TestRunLiveWarnsButDoesNotBlockWhenADomainIsMissing(t *testing.T) {
	// What the two versions call a domain differs across the 0.15/0.16
	// boundary - principals on one side, Domain objects on the other, with
	// aliases counted differently - and INBUXA's own before-list was
	// inflated with alias domains that the after-list structurally cannot
	// contain. Failing the migration on that would abort a run that lost
	// nothing, so it is reported and not treated as data loss.
	srv := fakeInstance(t, []string{"example.org"}, map[string]float64{"ann@example.org": 10})
	defer srv.Close()

	store, rs := newRun(t)
	report, err := RunLive(context.Background(), store, rs, LiveOptions{
		AdminURL: srv.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: srv.Client(),
		Before: &checkpoint.PreflightSnapshot{
			Domains:   []string{"example.org", "alias.example"},
			UsedQuota: map[string]int64{"ann@example.org": 1},
		},
	})
	if err != nil {
		t.Fatalf("RunLive: %v", err)
	}
	if report.Blocking() {
		t.Fatalf("a domain-only difference must not abort the migration, got: %s", report.String())
	}
	if got := report.Results[0].Status; got != StatusWarn {
		t.Fatalf("status = %q, want %q", got, StatusWarn)
	}
	if !strings.Contains(report.Results[0].Detail, "alias.example") {
		t.Fatalf("the operator still needs to be told which domain, got %q", report.Results[0].Detail)
	}
}

func TestRunLiveStillBlocksWhenAnAccountAndADomainAreMissing(t *testing.T) {
	// A lost account is a lost account, whatever the domain list says.
	srv := fakeInstance(t, []string{"example.org"}, map[string]float64{"ann@example.org": 10})
	defer srv.Close()

	store, rs := newRun(t)
	report, _ := RunLive(context.Background(), store, rs, LiveOptions{
		AdminURL: srv.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: srv.Client(),
		Before: &checkpoint.PreflightSnapshot{
			Domains:   []string{"example.org", "alias.example"},
			UsedQuota: map[string]int64{"ann@example.org": 1, "bob@example.org": 2},
		},
	})
	if !report.Blocking() {
		t.Fatalf("a missing account must still block, got: %s", report.String())
	}
}

func TestRunLiveFailsWhenTheInstanceCannotBeRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	store, rs := newRun(t)
	report, err := RunLive(context.Background(), store, rs, LiveOptions{
		AdminURL: srv.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: srv.Client(),
		Before: &checkpoint.PreflightSnapshot{Domains: []string{"example.org"}},
	})
	if err == nil {
		t.Fatal("expected an error when the instance cannot be read")
	}
	if !report.Blocking() {
		t.Fatalf("being unable to look must not read as a pass, got: %s", report.String())
	}
}

func newRun(t *testing.T) (*checkpoint.Store, *checkpoint.RunState) {
	t.Helper()
	store := checkpoint.NewStore(t.TempDir())
	rs, err := store.Create("0.15.5", "0.16.14")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return store, rs
}

// fakeInstance answers the principal listing the snapshot is built from.
func fakeInstance(t *testing.T, domains []string, accounts map[string]float64) *httptest.Server {
	t.Helper()
	items := make([]map[string]any, 0, len(domains)+len(accounts))
	for _, d := range domains {
		items = append(items, map[string]any{"type": "domain", "name": d})
	}
	for name, quota := range accounts {
		items = append(items, map[string]any{"type": "individual", "name": name, "usedQuota": quota})
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": items, "total": len(items)}})
	}))
}
