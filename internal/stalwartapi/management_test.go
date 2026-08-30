// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package stalwartapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveJMAPSession answers session discovery for a server standing in for
// a 0.16+ instance: what marks it as one is the urn:stalwart:jmap
// capability, which is exactly what AccountSnapshot dispatches on. Returns
// true if it handled the request.
func serveJMAPSession(w http.ResponseWriter, r *http.Request, apiURL string) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/.well-known/jmap" {
		return false
	}
	json.NewEncoder(w).Encode(map[string]any{
		"apiUrl":       apiURL,
		"capabilities": map[string]any{"urn:ietf:params:jmap:core": map[string]any{}, "urn:stalwart:jmap": map[string]any{}},
	})
	return true
}

// jmapEnvelope mirrors the wire shape this package's call() parses: a
// top-level {"methodResponses": [...]} object where each entry is a
// [name, args, callId] triple (RFC 8620 §3.2).
type jmapEnvelope struct {
	MethodResponses []any `json:"methodResponses"`
}

// accountManagementAndMailboxServer builds a fake server that answers both
// the x:Account/* management calls AccountSnapshot makes directly, and the
// session-discovery + Mailbox/get calls it makes indirectly (per account)
// via MailboxSnapshot. mailboxesFor maps an account email to the mailbox
// list its Mailbox/get should return; an account absent from the map gets a
// 403 on session discovery, simulating a missing `impersonate` grant.
func accountManagementAndMailboxServer(t *testing.T, mailboxesFor map[string][]map[string]any) (*httptest.Server, *[]string) {
	t.Helper()
	var gotPaths []string
	var apiURL string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/principal" {
			w.WriteHeader(http.StatusNotFound) // v0.16 shape: no REST management API
			return
		}
		gotPaths = append(gotPaths, r.URL.Path)

		if r.Method == http.MethodGet && r.URL.Path == "/.well-known/jmap" {
			user, _, _ := r.BasicAuth()
			if !strings.Contains(user, "%") {
				// The client's own session request, used to decide which
				// management API this instance speaks.
				serveJMAPSession(w, r, apiURL)
				return
			}
			target := strings.SplitN(user, "%", 2)[0]
			if _, ok := mailboxesFor[target]; !ok {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"apiUrl":          apiURL,
				"primaryAccounts": map[string]string{jmapMailCapability: "mail-" + target},
			})
			return
		}

		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		methodCalls := body["methodCalls"].([]any)
		first := methodCalls[0].([]any)
		methodName := first[0].(string)

		switch methodName {
		case "x:Domain/query":
			json.NewEncoder(w).Encode(jmapEnvelope{MethodResponses: []any{
				[]any{"x:Domain/query", map[string]any{"ids": []string{"d1"}}, "q"},
				[]any{"x:Domain/get", map[string]any{"list": []map[string]any{
					{"id": "d1", "name": "example.com"},
				}}, "g"},
			}})
		case "x:Account/query":
			json.NewEncoder(w).Encode(jmapEnvelope{MethodResponses: []any{
				[]any{"x:Account/query", map[string]any{"ids": []string{"a1", "a2"}}, "q"},
			}})
		case "x:Account/get":
			json.NewEncoder(w).Encode(jmapEnvelope{MethodResponses: []any{
				[]any{"x:Account/get", map[string]any{"list": []map[string]any{
					{"id": "a1", "name": "alice@example.com", "domainId": "example.com"},
					{"id": "a2", "name": "bob@example.org", "domainId": "example.org"},
				}}, "g"},
			}})
		case "Mailbox/get":
			args := first[1].(map[string]any)
			accountID := args["accountId"].(string)
			target := strings.TrimPrefix(accountID, "mail-")
			json.NewEncoder(w).Encode(jmapEnvelope{MethodResponses: []any{
				[]any{"Mailbox/get", map[string]any{"list": mailboxesFor[target]}, "m"},
			}})
		default:
			t.Errorf("unexpected method call: %s", methodName)
		}
	}))
	apiURL = srv.URL + "/api"
	return srv, &gotPaths
}

func TestAccountSnapshotQueriesThenGets(t *testing.T) {
	srv, _ := accountManagementAndMailboxServer(t, map[string][]map[string]any{
		"alice@example.com": {{"name": "Inbox", "totalEmails": 10}},
		"bob@example.org":   {{"name": "Inbox", "totalEmails": 3}, {"name": "Archive", "totalEmails": 100}},
	})
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "hunter2"}
	snap, err := client.AccountSnapshot(context.Background())
	if err != nil {
		t.Fatalf("AccountSnapshot: %v", err)
	}
	if snap.AccountCount != 2 {
		t.Errorf("AccountCount = %d, want 2", snap.AccountCount)
	}
	if len(snap.Domains) != 2 || snap.Domains[0] != "example.com" || snap.Domains[1] != "example.org" {
		t.Errorf("Domains = %v, want [example.com example.org] (sorted)", snap.Domains)
	}
	if len(snap.MailboxErrors) != 0 {
		t.Errorf("MailboxErrors = %v, want none (both accounts should succeed)", snap.MailboxErrors)
	}
	alice := snap.MailboxCounts["alice@example.com"]
	if len(alice) != 1 || alice[0].Mailbox != "Inbox" || alice[0].Messages != 10 {
		t.Errorf("alice's mailboxes = %+v, want [{Inbox 10}]", alice)
	}
	bob := snap.MailboxCounts["bob@example.org"]
	if len(bob) != 2 || bob[1].Mailbox != "Archive" || bob[1].Messages != 100 {
		t.Errorf("bob's mailboxes = %+v, want Inbox then Archive(100)", bob)
	}
}

func TestAccountSnapshotRecordsPerAccountMailboxFailureWithoutFailingOverall(t *testing.T) {
	// bob@example.org is deliberately absent from mailboxesFor, simulating
	// a missing `impersonate` grant for that one account.
	srv, _ := accountManagementAndMailboxServer(t, map[string][]map[string]any{
		"alice@example.com": {{"name": "Inbox", "totalEmails": 10}},
	})
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "hunter2"}
	snap, err := client.AccountSnapshot(context.Background())
	if err != nil {
		t.Fatalf("AccountSnapshot should not fail overall just because one account's mailbox capture failed: %v", err)
	}
	if snap.AccountCount != 2 {
		t.Errorf("AccountCount = %d, want 2 (account enumeration is unaffected by the mailbox-capture failure)", snap.AccountCount)
	}
	if _, ok := snap.MailboxCounts["alice@example.com"]; !ok {
		t.Error("alice's mailbox counts should still be captured")
	}
	if _, ok := snap.MailboxCounts["bob@example.org"]; ok {
		t.Error("bob's mailbox counts should NOT be present - his capture failed")
	}
	if _, ok := snap.MailboxErrors["bob@example.org"]; !ok {
		t.Error("bob's failure should be recorded in MailboxErrors, not silently dropped")
	}
}

func TestAccountSnapshotEmptyInstance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveJMAPSession(w, r, "/api") {
			return
		}
		json.NewEncoder(w).Encode(jmapEnvelope{MethodResponses: []any{
			[]any{"x:Account/query", map[string]any{"ids": []string{}}, "q"},
		}})
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "x"}
	snap, err := client.AccountSnapshot(context.Background())
	if err != nil {
		t.Fatalf("AccountSnapshot: %v", err)
	}
	if snap.AccountCount != 0 {
		t.Errorf("AccountCount = %d, want 0", snap.AccountCount)
	}
}

func TestAccountSnapshotPropagatesJMAPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/principal" {
			w.WriteHeader(http.StatusNotFound) // v0.16 shape: no REST management API
			return
		}
		if serveJMAPSession(w, r, "/api") {
			return
		}
		json.NewEncoder(w).Encode(jmapEnvelope{MethodResponses: []any{
			[]any{"error", map[string]any{"type": "forbidden"}, "q"},
		}})
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "x"}
	_, err := client.AccountSnapshot(context.Background())
	if err == nil {
		t.Fatal("AccountSnapshot should surface a JMAP-level error response")
	}
}

func TestAccountSnapshotPropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("invalid credentials"))
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "wrong"}
	_, err := client.AccountSnapshot(context.Background())
	if err == nil {
		t.Fatal("AccountSnapshot should error on a non-200 response")
	}
}

func TestAccountSnapshotSendsBasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "admin" || pass != "hunter2" {
			t.Errorf("BasicAuth = (%s, %s, %v), want (admin, hunter2, true)", user, pass, ok)
		}
		if serveJMAPSession(w, r, "/api") {
			return
		}
		json.NewEncoder(w).Encode(jmapEnvelope{MethodResponses: []any{
			[]any{"x:Account/query", map[string]any{"ids": []string{}}, "q"},
		}})
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "hunter2"}
	if _, err := client.AccountSnapshot(context.Background()); err != nil {
		t.Fatalf("AccountSnapshot: %v", err)
	}
}
