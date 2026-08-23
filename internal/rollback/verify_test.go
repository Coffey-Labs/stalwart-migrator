package rollback

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
)

// restoredInstance is a fake Stalwart answering the two things the reduced
// post-rollback suite asks of it: a JMAP session document (reachability)
// and the x:Account/* management calls behind the directory comparison.
// Per-account mailbox impersonation is refused, which is deliberate - the
// reduced suite must not depend on it, since a rollback's guarantee comes
// from the verified manifest, not from re-walking every mailbox.
func restoredInstance(t *testing.T, accounts []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/.well-known/jmap" {
			if user, _, _ := r.BasicAuth(); strings.Contains(user, "%") {
				w.WriteHeader(http.StatusForbidden) // no impersonate grant
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"apiUrl": "/api"})
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		calls := body["methodCalls"].([]any)
		name := calls[0].([]any)[0].(string)

		switch name {
		case "x:Account/query":
			ids := make([]string, len(accounts))
			for i, a := range accounts {
				ids[i] = a["id"].(string)
			}
			json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
				[]any{"x:Account/query", map[string]any{"ids": ids}, "q"},
			}})
		case "x:Account/get":
			json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
				[]any{"x:Account/get", map[string]any{"list": accounts}, "g"},
			}})
		default:
			t.Errorf("unexpected method call %s - the reduced suite should not be making it", name)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func resultFor(t *testing.T, results []CheckResult, name string) CheckResult {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no %q result in %+v", name, results)
	return CheckResult{}
}

func TestVerifyPassesOnAProperlyRestoredInstance(t *testing.T) {
	srv := restoredInstance(t, []map[string]any{
		{"id": "a1", "name": "alice@example.com", "domainId": "example.com"},
		{"id": "a2", "name": "bob@example.com", "domainId": "example.com"},
	})
	binDir := withFakeExecutable(t, "stalwart", "#!/bin/sh\necho 'stalwart 0.15.5'\n")

	results, err := Verify(context.Background(), VerifyOptions{
		BinaryPath: filepath.Join(binDir, "stalwart"), ExpectVersion: "0.15.5",
		AdminURL: srv.URL, AdminUser: "admin", AdminPassword: "hunter2",
		Snapshot: &checkpoint.PreflightSnapshot{AccountCount: 2, Domains: []string{"example.com"}},
	})
	if err != nil {
		t.Fatalf("Verify: %v (%+v)", err, results)
	}
	for _, name := range []string{"version", "reachable", "directory-counts"} {
		if got := resultFor(t, results, name); got.Status != StatusOK {
			t.Errorf("%s = %s: %s", name, got.Status, got.Detail)
		}
	}
}

func TestVerifyDetectsTheWrongVersionStillInstalled(t *testing.T) {
	binDir := withFakeExecutable(t, "stalwart", "#!/bin/sh\necho 'stalwart 0.16.14'\n")

	results, err := Verify(context.Background(), VerifyOptions{
		BinaryPath: filepath.Join(binDir, "stalwart"), ExpectVersion: "0.15.5",
	})
	if err == nil {
		t.Fatal("Verify: want error when the new binary is still installed, got nil")
	}
	res := resultFor(t, results, "version")
	if res.Status != StatusFail {
		t.Errorf("version = %s, want fail", res.Status)
	}
	if !strings.Contains(res.Detail, "did not put the original binary back") {
		t.Errorf("detail %q should say plainly what went wrong", res.Detail)
	}
}

func TestVerifyDetectsADirectoryThatDoesNotMatchTheSnapshot(t *testing.T) {
	srv := restoredInstance(t, []map[string]any{
		{"id": "a1", "name": "alice@example.com", "domainId": "example.com"},
	})

	results, err := Verify(context.Background(), VerifyOptions{
		AdminURL: srv.URL, AdminUser: "admin", AdminPassword: "hunter2",
		Snapshot: &checkpoint.PreflightSnapshot{AccountCount: 2, Domains: []string{"example.com", "example.org"}},
	})
	if err == nil {
		t.Fatal("Verify: want error when the restored directory is smaller than the snapshot, got nil")
	}
	res := resultFor(t, results, "directory-counts")
	if res.Status != StatusFail {
		t.Fatalf("directory-counts = %s, want fail", res.Status)
	}
	for _, want := range []string{"2 account(s) before, 1 after", "example.org"} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("detail %q should include %q", res.Detail, want)
		}
	}
}

func TestVerifyReportsAnUnreachableInstance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	results, err := Verify(context.Background(), VerifyOptions{
		AdminURL: srv.URL, AdminUser: "admin", Timeout: 300 * time.Millisecond,
		Snapshot: &checkpoint.PreflightSnapshot{AccountCount: 1},
	})
	if err == nil {
		t.Fatal("Verify: want error when the restored instance never answers, got nil")
	}
	if got := resultFor(t, results, "reachable"); got.Status != StatusFail {
		t.Errorf("reachable = %s, want fail", got.Status)
	}
	// Reporting "directory matches" against an instance that never answered
	// would be worse than reporting nothing.
	if got := resultFor(t, results, "directory-counts"); got.Status != StatusSkipped {
		t.Errorf("directory-counts = %s, want skip when the instance is unreachable", got.Status)
	}
}

func TestVerifySkipsRatherThanInventsWhatItCannotCheck(t *testing.T) {
	results, err := Verify(context.Background(), VerifyOptions{})
	if err != nil {
		t.Fatalf("Verify with nothing to check should not fail: %v", err)
	}
	for _, name := range []string{"version", "reachable", "directory-counts"} {
		if got := resultFor(t, results, name); got.Status != StatusSkipped {
			t.Errorf("%s = %s, want skip", name, got.Status)
		}
	}
}
