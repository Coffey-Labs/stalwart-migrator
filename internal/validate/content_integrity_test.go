// SPDX-FileCopyrightText: 2026 Coffey Labs
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
	"github.com/LINUXexpert-org/stalwart-migrator/internal/stalwartapi"
)

// jmapEnvelope mirrors the wire shape stalwartapi.Client.call() parses.
type jmapEnvelope struct {
	MethodResponses []any `json:"methodResponses"`
}

// fakeManagementServer serves x:Account/query + x:Account/get from
// accounts, and, for each of them, session discovery + Mailbox/get from
// mailboxesByEmail (keyed by the account's post-migration email).
func fakeManagementServer(t *testing.T, accounts []map[string]any, mailboxesByEmail map[string][]map[string]any) *httptest.Server {
	t.Helper()
	var apiURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/principal" {
			w.WriteHeader(http.StatusNotFound) // v0.16 shape: no REST management API
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/.well-known/jmap" {
			user, _, _ := r.BasicAuth()
			if !strings.Contains(user, "%") {
				// This instance is a migrated 0.16 one, which is what the
				// urn:stalwart:jmap capability says.
				json.NewEncoder(w).Encode(map[string]any{
					"apiUrl":       apiURL,
					"capabilities": map[string]any{"urn:ietf:params:jmap:core": map[string]any{}, "urn:stalwart:jmap": map[string]any{}},
				})
				return
			}
			target := strings.SplitN(user, "%", 2)[0]
			if _, ok := mailboxesByEmail[target]; !ok {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"apiUrl":          apiURL,
				"primaryAccounts": map[string]string{"urn:ietf:params:jmap:mail": "mail-" + target},
			})
			return
		}

		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		methodCalls := body["methodCalls"].([]any)
		call := methodCalls[0].([]any)
		name := call[0].(string)
		switch name {
		case "x:Domain/query":
			json.NewEncoder(w).Encode(jmapEnvelope{MethodResponses: []any{
				[]any{"x:Domain/query", map[string]any{"ids": []string{"d1"}}, "q"},
				[]any{"x:Domain/get", map[string]any{"list": []map[string]any{
					{"id": "d1", "name": "smoke.test"},
				}}, "g"},
			}})
		case "x:Account/query":
			ids := make([]string, len(accounts))
			for i, a := range accounts {
				ids[i] = a["id"].(string)
			}
			json.NewEncoder(w).Encode(jmapEnvelope{MethodResponses: []any{
				[]any{"x:Account/query", map[string]any{"ids": ids}, "q"},
			}})
		case "x:Account/get":
			json.NewEncoder(w).Encode(jmapEnvelope{MethodResponses: []any{
				[]any{"x:Account/get", map[string]any{"list": accounts}, "g"},
			}})
		case "Mailbox/get":
			args := call[1].(map[string]any)
			accountID := args["accountId"].(string)
			target := strings.TrimPrefix(accountID, "mail-")
			json.NewEncoder(w).Encode(jmapEnvelope{MethodResponses: []any{
				[]any{"Mailbox/get", map[string]any{"list": mailboxesByEmail[target]}, "m"},
			}})
		}
	}))
	apiURL = srv.URL + "/api"
	return srv
}

func TestCompareContentIntegrityMatchesRewrittenBareUsernameByLocalPart(t *testing.T) {
	// Pre-migration, the account was a bare username "alice" (pre-0.16
	// style). Post-migration, v0.16's own conversion rewrote it to a full
	// email address - see UPGRADING/v0_16.md. An exact-string match would
	// wrongly report "alice" as missing.
	srv := fakeManagementServer(t,
		[]map[string]any{{"id": "a1", "name": "alice@example.com", "domainId": "example.com"}},
		map[string][]map[string]any{"alice@example.com": {{"name": "Inbox", "totalEmails": 42}}},
	)
	defer srv.Close()

	before := &checkpoint.PreflightSnapshot{
		MailboxCounts: map[string][]checkpoint.MailboxCount{
			"alice": {{Mailbox: "Inbox", Messages: 42}}, // bare username, pre-migration
		},
	}
	client := &stalwartapi.Client{BaseURL: srv.URL, Username: "admin", Password: "x"}

	result, err := compareContentIntegrity(context.Background(), client, before)
	if err != nil {
		t.Fatalf("compareContentIntegrity: %v", err)
	}
	if !result.OK() {
		t.Errorf("result.OK() = false, want true (local-part match should have found alice@example.com): %s", result.String())
	}
	if len(result.MissingAccounts) != 0 {
		t.Errorf("MissingAccounts = %v, want none", result.MissingAccounts)
	}
}

func TestCompareContentIntegrityNoFalseMatchAcrossUnrelatedAccounts(t *testing.T) {
	// "alice" (before) must not spuriously match "alice-archive@example.com"
	// (after) just because one contains the other - local-part comparison
	// must be an exact match on the part before "@", not a substring check.
	srv := fakeManagementServer(t,
		[]map[string]any{{"id": "a1", "name": "alice-archive@example.com", "domainId": "example.com"}},
		map[string][]map[string]any{"alice-archive@example.com": {{"name": "Inbox", "totalEmails": 1}}},
	)
	defer srv.Close()

	before := &checkpoint.PreflightSnapshot{
		MailboxCounts: map[string][]checkpoint.MailboxCount{
			"alice": {{Mailbox: "Inbox", Messages: 42}},
		},
	}
	client := &stalwartapi.Client{BaseURL: srv.URL, Username: "admin", Password: "x"}

	result, err := compareContentIntegrity(context.Background(), client, before)
	if err != nil {
		t.Fatalf("compareContentIntegrity: %v", err)
	}
	if result.OK() {
		t.Fatal("result.OK() = true, want a missing-account failure - alice-archive@example.com is a different account than alice")
	}
	if len(result.MissingAccounts) != 1 || result.MissingAccounts[0] != "alice" {
		t.Errorf("MissingAccounts = %v, want [alice]", result.MissingAccounts)
	}
}

func TestCompareContentIntegrityMultipleMailboxesPerAccount(t *testing.T) {
	srv := fakeManagementServer(t,
		[]map[string]any{{"id": "a1", "name": "bob@example.org", "domainId": "example.org"}},
		map[string][]map[string]any{"bob@example.org": {
			{"name": "Inbox", "totalEmails": 10},
			{"name": "Archive", "totalEmails": 200},
		}},
	)
	defer srv.Close()

	before := &checkpoint.PreflightSnapshot{
		MailboxCounts: map[string][]checkpoint.MailboxCount{
			"bob@example.org": {
				{Mailbox: "Inbox", Messages: 10},
				{Mailbox: "Archive", Messages: 199}, // one message short
			},
		},
	}
	client := &stalwartapi.Client{BaseURL: srv.URL, Username: "admin", Password: "x"}

	result, err := compareContentIntegrity(context.Background(), client, before)
	if err != nil {
		t.Fatalf("compareContentIntegrity: %v", err)
	}
	if result.AccountsChecked != 1 || result.MailboxesChecked != 2 {
		t.Errorf("AccountsChecked=%d MailboxesChecked=%d, want 1 and 2", result.AccountsChecked, result.MailboxesChecked)
	}
	if len(result.MessageCountMismatches) != 1 {
		t.Fatalf("MessageCountMismatches = %+v, want exactly one (Archive)", result.MessageCountMismatches)
	}
	m := result.MessageCountMismatches[0]
	if m.Mailbox != "Archive" || m.Before != 199 || m.After != 200 {
		t.Errorf("mismatch = %+v, want Archive 199->200", m)
	}
}

// The bug this guards against was found by running preflight against a real
// Stalwart 0.15.5: it reports no per-mailbox counts, so the "before"
// snapshot has none, and the comparison used to iterate that empty map,
// check nothing, and report "all message counts match" - the strongest
// claim this tool makes, made vacuously.
func TestCompareContentIntegrityDoesNotClaimCountsMatchWhenSourceHadNone(t *testing.T) {
	srv := fakeManagementServer(t,
		[]map[string]any{{"id": "a1", "name": "alice@smoke.test", "domainId": "smoke.test"}},
		map[string][]map[string]any{"alice@smoke.test": {{"name": "Inbox", "totalEmails": 3}}},
	)
	defer srv.Close()

	// A 0.15.x-shaped snapshot: accounts and used-quota, no mailbox counts.
	before := &checkpoint.PreflightSnapshot{
		AccountCount: 1,
		Domains:      []string{"smoke.test"},
		UsedQuota:    map[string]int64{"alice@smoke.test": 9207},
	}
	client := &stalwartapi.Client{BaseURL: srv.URL, Username: "admin", Password: "x"}
	result, err := compareContentIntegrity(context.Background(), client, before)
	if err != nil {
		t.Fatal(err)
	}
	if result.MessageCountsCompared {
		t.Error("MessageCountsCompared = true, but the source snapshot had no counts")
	}
	if result.AccountsChecked != 1 {
		t.Errorf("AccountsChecked = %d, want 1 - the account set must still be verified", result.AccountsChecked)
	}
	if strings.Contains(result.String(), "all message counts match") {
		t.Errorf("report claims counts match when none were compared:\n%s", result)
	}
	if !strings.Contains(result.String(), "MESSAGE COUNTS NOT COMPARED") {
		t.Errorf("report must say plainly that no-data-loss was not verified:\n%s", result)
	}
}

// Presence checking still has to work on that path, or it would be no
// better than the vacuous pass it replaced.
func TestCompareContentIntegrityDetectsLostAccountWithoutCounts(t *testing.T) {
	srv := fakeManagementServer(t,
		[]map[string]any{{"id": "a1", "name": "alice@smoke.test", "domainId": "smoke.test"}},
		map[string][]map[string]any{"alice@smoke.test": {{"name": "Inbox", "totalEmails": 3}}},
	)
	defer srv.Close()

	before := &checkpoint.PreflightSnapshot{
		AccountCount: 2,
		Domains:      []string{"smoke.test", "gone.example"},
		UsedQuota:    map[string]int64{"alice@smoke.test": 9207, "bob@smoke.test": 5380},
	}
	client := &stalwartapi.Client{BaseURL: srv.URL, Username: "admin", Password: "x"}
	result, err := compareContentIntegrity(context.Background(), client, before)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK() {
		t.Fatalf("want a failing result when an account and a domain vanished:\n%s", result)
	}
	if len(result.MissingAccounts) != 1 || result.MissingAccounts[0] != "bob@smoke.test" {
		t.Errorf("MissingAccounts = %v, want [bob@smoke.test]", result.MissingAccounts)
	}
	if len(result.MissingDomains) != 1 || result.MissingDomains[0] != "gone.example" {
		t.Errorf("MissingDomains = %v, want [gone.example]", result.MissingDomains)
	}
}
