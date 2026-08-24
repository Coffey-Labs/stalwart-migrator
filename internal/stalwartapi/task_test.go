// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package stalwartapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// taskServer answers x:Task/set and x:Task/get. queue models Stalwart's own
// task queue: a task that has run is *removed* from it, which is how
// completion is detected (see task.go's opening comment).
type taskServer struct {
	mu       sync.Mutex
	queue    map[string]string // task id -> status @type
	created  []map[string]any  // the create objects received, in request order
	notFound bool              // reject every creation
}

func newTaskServer(t *testing.T) (*taskServer, *httptest.Server) {
	t.Helper()
	ts := &taskServer{queue: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/principal" {
			w.WriteHeader(http.StatusNotFound) // v0.16 shape: no REST management API
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/.well-known/jmap" {
			// The endpoint is discovered, not assumed - see apiEndpoint.
			json.NewEncoder(w).Encode(map[string]any{
				"apiUrl":       "/api",
				"capabilities": map[string]any{"urn:stalwart:jmap": map[string]any{}},
			})
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		call := body["methodCalls"].([]any)[0].([]any)
		name := call[0].(string)
		args := call[1].(map[string]any)

		ts.mu.Lock()
		defer ts.mu.Unlock()

		switch name {
		case "x:Task/set":
			create := args["create"].(map[string]any)
			created := map[string]any{}
			notCreated := map[string]any{}
			for creationID, obj := range create {
				ts.created = append(ts.created, obj.(map[string]any))
				if ts.notFound {
					notCreated[creationID] = map[string]any{"type": "forbidden"}
					continue
				}
				id := "task-" + creationID
				ts.queue[id] = "Pending"
				created[creationID] = map[string]any{"id": id}
			}
			json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
				[]any{"x:Task/set", map[string]any{"created": created, "notCreated": notCreated}, "s"},
			}})
		case "x:Task/get":
			var list []any
			for _, raw := range args["ids"].([]any) {
				id := raw.(string)
				status, stillQueued := ts.queue[id]
				if !stillQueued {
					continue // consumed: finished
				}
				entry := map[string]any{"id": id, "status": map[string]any{"@type": status}}
				if status == "Failed" {
					entry["status"].(map[string]any)["failureReason"] = "store unavailable"
				}
				list = append(list, entry)
			}
			json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
				[]any{"x:Task/get", map[string]any{"list": list}, "g"},
			}})
		case "x:Account/query":
			json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
				[]any{"x:Account/query", map[string]any{"ids": []string{"a1", "a2", "a3"}}, "q"},
			}})
		default:
			t.Errorf("unexpected method call %s", name)
		}
	}))
	t.Cleanup(srv.Close)
	return ts, srv
}

func (ts *taskServer) finish(id string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.queue, id)
}

func (ts *taskServer) fail(id string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.queue[id] = "Failed"
}

// The wire shape here comes from Stalwart's x:Task schema reference, so the
// test pins it: an AccountMaintenance variant with maintenanceType
// recalculateQuota, one per account, in a single Task/set call.
func TestCreateQuotaRecalculationTasksSendsOnePerAccount(t *testing.T) {
	ts, srv := newTaskServer(t)
	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "hunter2"}

	ids, err := client.CreateQuotaRecalculationTasks(context.Background(), []string{"a1", "a2"})
	if err != nil {
		t.Fatalf("CreateQuotaRecalculationTasks: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("created %d task ids, want 2: %v", len(ids), ids)
	}
	if len(ts.created) != 2 {
		t.Fatalf("server received %d creations, want 2", len(ts.created))
	}
	seen := map[string]bool{}
	for _, obj := range ts.created {
		if obj["@type"] != "AccountMaintenance" {
			t.Errorf("@type = %v, want AccountMaintenance", obj["@type"])
		}
		if obj["maintenanceType"] != "recalculateQuota" {
			t.Errorf("maintenanceType = %v, want recalculateQuota", obj["maintenanceType"])
		}
		status := obj["status"].(map[string]any)
		if status["@type"] != "Pending" {
			t.Errorf("status.@type = %v, want Pending", status["@type"])
		}
		seen[obj["accountId"].(string)] = true
	}
	if !seen["a1"] || !seen["a2"] {
		t.Errorf("accountIds sent = %v, want a1 and a2", seen)
	}
}

func TestCreateTenantQuotaRecalculationTasksUsesTenantVariant(t *testing.T) {
	ts, srv := newTaskServer(t)
	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "hunter2"}

	if _, err := client.CreateTenantQuotaRecalculationTasks(context.Background(), []string{"t1"}); err != nil {
		t.Fatal(err)
	}
	if got := ts.created[0]["@type"]; got != "TenantMaintenance" {
		t.Errorf("@type = %v, want TenantMaintenance", got)
	}
	if got := ts.created[0]["tenantId"]; got != "t1" {
		t.Errorf("tenantId = %v, want t1", got)
	}
}

// Reporting "quotas recalculated" for an account whose task the server
// refused would be exactly the silent partial success this tool exists to
// catch.
func TestCreateQuotaRecalculationTasksFailsOnRefusedCreations(t *testing.T) {
	ts, srv := newTaskServer(t)
	ts.notFound = true
	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "hunter2"}

	_, err := client.CreateQuotaRecalculationTasks(context.Background(), []string{"a1"})
	if err == nil {
		t.Fatal("want error when the server refuses a creation, got nil")
	}
	if !strings.Contains(err.Error(), "account a1") {
		t.Errorf("error %q should name the account whose task was refused", err)
	}
}

func TestCreateQuotaRecalculationTasksIsANoOpForNoAccounts(t *testing.T) {
	_, srv := newTaskServer(t)
	client := &Client{BaseURL: srv.URL, Username: "admin"}
	ids, err := client.CreateQuotaRecalculationTasks(context.Background(), nil)
	if err != nil || len(ids) != 0 {
		t.Errorf("want no ids and no error for an empty account list, got %v, %v", ids, err)
	}
}

func TestWaitForTasksReturnsOnceTheQueueDrains(t *testing.T) {
	ts, srv := newTaskServer(t)
	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "hunter2"}
	ids, err := client.CreateQuotaRecalculationTasks(context.Background(), []string{"a1", "a2"})
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		for _, id := range ids {
			ts.finish(id)
		}
	}()

	failures, err := client.WaitForTasks(context.Background(), ids, 10*time.Second)
	if err != nil {
		t.Fatalf("WaitForTasks: %v", err)
	}
	if len(failures) != 0 {
		t.Errorf("failures = %v, want none", failures)
	}
}

func TestWaitForTasksCollectsFailedTasksInsteadOfWaitingForever(t *testing.T) {
	ts, srv := newTaskServer(t)
	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "hunter2"}
	ids, err := client.CreateQuotaRecalculationTasks(context.Background(), []string{"a1", "a2"})
	if err != nil {
		t.Fatal(err)
	}
	ts.fail(ids[0])
	ts.finish(ids[1])

	failures, err := client.WaitForTasks(context.Background(), ids, 10*time.Second)
	if err != nil {
		t.Fatalf("a task reaching Failed is a result, not a polling error: %v", err)
	}
	if len(failures) != 1 || failures[0].TaskID != ids[0] {
		t.Fatalf("failures = %v, want just %s", failures, ids[0])
	}
	if !strings.Contains(failures[0].Reason, "store unavailable") {
		t.Errorf("failure reason = %q, want the server's own reason", failures[0].Reason)
	}
}

// "Still running after the timeout" and "ran and failed" are different
// answers for an operator - one means wait longer, the other means
// something is wrong.
func TestWaitForTasksDistinguishesATimeoutFromAFailure(t *testing.T) {
	_, srv := newTaskServer(t)
	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "hunter2"}
	ids, err := client.CreateQuotaRecalculationTasks(context.Background(), []string{"a1"})
	if err != nil {
		t.Fatal(err)
	}

	failures, err := client.WaitForTasks(context.Background(), ids, 100*time.Millisecond)
	if err == nil {
		t.Fatal("want an error when tasks are still queued at the timeout, got nil")
	}
	if len(failures) != 0 {
		t.Errorf("failures = %v, want none - a still-queued task hasn't failed", failures)
	}
	if !strings.Contains(err.Error(), "still queued") {
		t.Errorf("error %q should say the tasks were still queued, not that they failed", err)
	}
}

func TestAccountIDsSkipsTheMailboxWalk(t *testing.T) {
	_, srv := newTaskServer(t)
	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "hunter2"}

	ids, err := client.AccountIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Errorf("AccountIDs = %v, want 3 ids", ids)
	}
}

func TestWaitForPingReturnsOnceTheInstanceAnswers(t *testing.T) {
	var mu sync.Mutex
	up := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !up {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"apiUrl": "/api"})
	}))
	defer srv.Close()

	go func() {
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		up = true
		mu.Unlock()
	}()

	client := &Client{BaseURL: srv.URL, Username: "admin", Password: "hunter2"}
	if err := client.WaitForPing(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("WaitForPing: %v", err)
	}
}
