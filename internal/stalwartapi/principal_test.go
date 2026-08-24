// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package stalwartapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stalwart015Server stands in for a 0.15.x instance: no urn:stalwart:jmap
// capability, POST /api is 404, and the management API is REST at
// /api/principal with 1-based page/limit paging. Every shape here was
// confirmed against a live 0.15.5 server.
func stalwart015Server(t *testing.T, individuals, domains []map[string]any) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)

		if r.URL.Path == "/.well-known/jmap" {
			// 0.15.5 advertises no urn:stalwart:jmap.
			json.NewEncoder(w).Encode(map[string]any{
				"capabilities": map[string]any{"urn:ietf:params:jmap:core": map[string]any{}},
			})
			return
		}
		if r.URL.Path == "/api" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"status":404,"title":"Not Found"}`))
			return
		}
		if r.URL.Path != "/api/principal" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		set := individuals
		if r.URL.Query().Get("types") == "domain" {
			set = domains
		}
		limit, page := 100, 1
		fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)

		start := (page - 1) * limit
		end := start + limit
		if start > len(set) {
			start = len(set)
		}
		if end > len(set) {
			end = len(set)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"items": set[start:end], "total": len(set)},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &paths
}

// The headline case: against the version this tool actually migrates from,
// AccountSnapshot must not fall over on a 404 from the 0.16-only endpoint.
func TestAccountSnapshotUsesRESTAgainst015(t *testing.T) {
	srv, paths := stalwart015Server(t,
		[]map[string]any{
			{"id": 4, "type": "individual", "name": "alice", "emails": []string{"alice@smoke.test"}, "usedQuota": 9207},
			{"id": 5, "type": "individual", "name": "bob", "emails": []string{"bob@smoke.test"}, "usedQuota": 5380},
		},
		[]map[string]any{{"id": 1, "type": "domain", "name": "smoke.test"}},
	)
	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "hunter2"}

	snap, err := client.AccountSnapshot(context.Background())
	if err != nil {
		t.Fatalf("AccountSnapshot against 0.15.x: %v", err)
	}
	if snap.AccountCount != 2 {
		t.Errorf("AccountCount = %d, want 2", snap.AccountCount)
	}
	if len(snap.Domains) != 1 || snap.Domains[0] != "smoke.test" {
		t.Errorf("Domains = %v, want [smoke.test]", snap.Domains)
	}
	if snap.UsedQuota["alice@smoke.test"] != 9207 || snap.UsedQuota["bob@smoke.test"] != 5380 {
		t.Errorf("UsedQuota = %v, want alice 9207 and bob 5380", snap.UsedQuota)
	}
	// Message counts genuinely cannot be had from 0.15.x - see principal.go.
	if len(snap.MailboxCounts) != 0 {
		t.Errorf("MailboxCounts = %v, want empty: 0.15.x exposes no per-mailbox counts", snap.MailboxCounts)
	}
	for _, p := range *paths {
		if strings.HasPrefix(p, "/api?") {
			t.Errorf("the 0.16-only JMAP management endpoint was called against a 0.15.x instance: %s", p)
		}
	}
}

// A truncated "before" snapshot would make the post-migration comparison
// assert less than it claims, silently.
func TestRESTPrincipalsFollowsPagination(t *testing.T) {
	var many []map[string]any
	for i := 0; i < 250; i++ {
		many = append(many, map[string]any{
			"id": i, "type": "individual",
			"name":   fmt.Sprintf("user%03d", i),
			"emails": []string{fmt.Sprintf("user%03d@smoke.test", i)},
		})
	}
	srv, _ := stalwart015Server(t, many, nil)
	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "x"}

	snap, err := client.AccountSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.AccountCount != 250 {
		t.Errorf("AccountCount = %d, want all 250 across pages", snap.AccountCount)
	}
}

// An instance with no explicit domain principals still has domains, implied
// by its accounts' addresses.
func TestRESTSnapshotDerivesDomainsFromAddresses(t *testing.T) {
	srv, _ := stalwart015Server(t,
		[]map[string]any{
			{"id": 1, "type": "individual", "name": "a", "emails": []string{"a@one.example"}},
			{"id": 2, "type": "individual", "name": "b", "emails": []string{"b@two.example"}},
		}, nil)
	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "x"}

	snap, err := client.AccountSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Domains) != 2 || snap.Domains[0] != "one.example" || snap.Domains[1] != "two.example" {
		t.Errorf("Domains = %v, want [one.example two.example] sorted", snap.Domains)
	}
}

func TestRESTSnapshotSurfacesAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/jmap" {
			json.NewEncoder(w).Encode(map[string]any{"capabilities": map[string]any{}})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("invalid credentials"))
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "wrong"}
	_, err := client.AccountSnapshot(context.Background())
	if err == nil {
		t.Fatal("want an error when the principal list rejects the credentials")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q should carry the status it got", err)
	}
}

// 0.16 reports the same per-account measure under a different name; both
// have to land in the same field or the comparison can't span the boundary.
func TestJMAPSnapshotCapturesUsedDiskQuota(t *testing.T) {
	srv, _ := accountManagementAndMailboxServer(t, map[string][]map[string]any{
		"alice@example.com": {{"name": "Inbox", "totalEmails": 10}},
		"bob@example.org":   {{"name": "Inbox", "totalEmails": 3}},
	})
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "hunter2"}
	snap, err := client.AccountSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.UsedQuota) != 2 {
		t.Errorf("UsedQuota = %v, want an entry per account", snap.UsedQuota)
	}
}
