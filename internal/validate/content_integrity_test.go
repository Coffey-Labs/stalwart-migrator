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
		if r.Method == http.MethodGet && r.URL.Path == "/.well-known/jmap" {
			user, _, _ := r.BasicAuth()
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
