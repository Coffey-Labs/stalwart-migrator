// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package stalwartapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMailboxSnapshotImpersonatesAndFetchesCounts(t *testing.T) {
	var apiURL string
	var sessionAuthUser, sessionAuthPass string
	var gotAccountID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/.well-known/jmap":
			sessionAuthUser, sessionAuthPass, _ = r.BasicAuth()
			json.NewEncoder(w).Encode(map[string]any{
				"apiUrl":          apiURL,
				"primaryAccounts": map[string]string{jmapMailCapability: "mail-acct-1"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/jmap-api":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			methodCalls := body["methodCalls"].([]any)
			args := methodCalls[0].([]any)[1].(map[string]any)
			gotAccountID, _ = args["accountId"].(string)
			json.NewEncoder(w).Encode(jmapEnvelope{MethodResponses: []any{
				[]any{"Mailbox/get", map[string]any{"list": []map[string]any{
					{"name": "Inbox", "totalEmails": 42},
					{"name": "Sent", "totalEmails": 7},
				}}, "m"},
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	apiURL = srv.URL + "/jmap-api"

	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "hunter2"}
	counts, err := client.MailboxSnapshot(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("MailboxSnapshot: %v", err)
	}

	if sessionAuthUser != "alice@example.com%admin" || sessionAuthPass != "hunter2" {
		t.Errorf("session discovery auth = (%s, %s), want (alice@example.com%%admin, hunter2)", sessionAuthUser, sessionAuthPass)
	}
	if gotAccountID != "mail-acct-1" {
		t.Errorf("Mailbox/get accountId = %s, want mail-acct-1", gotAccountID)
	}
	if len(counts) != 2 || counts[0].Mailbox != "Inbox" || counts[0].Messages != 42 || counts[1].Mailbox != "Sent" || counts[1].Messages != 7 {
		t.Errorf("counts = %+v, want [{Inbox 42} {Sent 7}]", counts)
	}
}

func TestMailboxSnapshotFailsWhenImpersonationRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, Username: "notasuperuser", Password: "x"}
	_, err := client.MailboxSnapshot(context.Background(), "alice@example.com")
	if err == nil {
		t.Fatal("MailboxSnapshot should error when session discovery (impersonation) is rejected")
	}
}

func TestMailboxSnapshotFailsWhenSessionHasNoMailAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"apiUrl":          "http://unused/",
			"primaryAccounts": map[string]string{},
		})
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "x"}
	_, err := client.MailboxSnapshot(context.Background(), "alice@example.com")
	if err == nil {
		t.Fatal("MailboxSnapshot should error when the session has no jmap:mail primary account")
	}
}

func TestMailboxSnapshotPropagatesMailboxGetError(t *testing.T) {
	var apiURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/.well-known/jmap":
			json.NewEncoder(w).Encode(map[string]any{
				"apiUrl":          apiURL,
				"primaryAccounts": map[string]string{jmapMailCapability: "mail-acct-1"},
			})
		case r.URL.Path == "/jmap-api":
			json.NewEncoder(w).Encode(jmapEnvelope{MethodResponses: []any{
				[]any{"error", map[string]any{"type": "accountNotFound"}, "m"},
			}})
		}
	}))
	defer srv.Close()
	apiURL = srv.URL + "/jmap-api"

	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "x"}
	_, err := client.MailboxSnapshot(context.Background(), "alice@example.com")
	if err == nil {
		t.Fatal("MailboxSnapshot should propagate a JMAP-level error from Mailbox/get")
	}
}
