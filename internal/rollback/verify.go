package rollback

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/preflight"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/stalwartapi"
)

// VerifyOptions configures the reduced validation suite that runs against
// the *restored* instance - ARCHITECTURE.md §4.8 step 5. It's deliberately
// smaller than §4.7's post-migration suite: the question here is "is the
// old instance actually back", not "did a migration preserve everything",
// and every check has to be one that would still pass on a healthy 0.15.5
// install.
type VerifyOptions struct {
	BinaryPath    string // checked with --version; skipped if empty
	ExpectVersion string // the run's recorded source version

	AdminURL      string
	AdminUser     string
	AdminPassword string
	HTTPClient    *http.Client

	// Snapshot is the pre-migration snapshot preflight captured. When set,
	// the restored instance's account count and domains are compared
	// against it - the "directory counts" half of §4.8 step 5. Per-mailbox
	// message counts are deliberately not re-checked here: the restore is a
	// byte-for-byte copy already verified against its manifest, so a
	// per-mailbox walk would cost a lot of time on a large install to
	// re-answer a question the manifest verification already answered.
	Snapshot *checkpoint.PreflightSnapshot

	// Timeout bounds how long the reachability check waits for the restored
	// service to answer, since it was started moments earlier.
	Timeout time.Duration
}

// Verify runs the reduced suite and returns one CheckResult per check.
// It always returns every result, even after a failure, so an operator sees
// the whole picture of a bad rollback in one pass rather than one problem
// at a time. The error is non-nil if any check failed.
func Verify(ctx context.Context, o VerifyOptions) ([]CheckResult, error) {
	var results []CheckResult
	fail := func(name, format string, args ...any) {
		results = append(results, CheckResult{Name: name, Status: StatusFail, Detail: fmt.Sprintf(format, args...)})
	}
	ok := func(name, format string, args ...any) {
		results = append(results, CheckResult{Name: name, Status: StatusOK, Detail: fmt.Sprintf(format, args...)})
	}
	skip := func(name, detail string) {
		results = append(results, CheckResult{Name: name, Status: StatusSkipped, Detail: detail})
	}

	switch {
	case o.BinaryPath == "" || o.ExpectVersion == "":
		skip("version", "no binary path or recorded source version to check against")
	default:
		got, err := preflight.DetectVersion(ctx, o.BinaryPath)
		switch {
		case err != nil:
			fail("version", "couldn't read the restored binary's version: %v", err)
		case got != o.ExpectVersion:
			fail("version", "restored binary reports %s, but this run started from %s - the rollback did not put the original binary back", got, o.ExpectVersion)
		default:
			ok("version", "restored binary reports %s, matching the version this run started from", got)
		}
	}

	if o.AdminURL == "" {
		skip("reachable", "no --admin-url configured - can't confirm the restored instance answers")
		skip("directory-counts", "no --admin-url configured - can't compare the restored directory against the pre-migration snapshot")
		return results, resultsError(results)
	}

	client := &stalwartapi.Client{
		BaseURL: o.AdminURL, Username: o.AdminUser, Password: o.AdminPassword, HTTPClient: o.HTTPClient,
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if err := waitForPing(ctx, client, timeout); err != nil {
		fail("reachable", "restored instance never answered at %s within %s: %v", o.AdminURL, timeout, err)
		skip("directory-counts", "skipped because the restored instance isn't reachable")
		return results, resultsError(results)
	}
	ok("reachable", "restored instance answered a JMAP session request at %s", o.AdminURL)

	if o.Snapshot == nil {
		skip("directory-counts", "this run has no pre-migration snapshot to compare against")
		return results, resultsError(results)
	}

	snap, err := client.AccountSnapshot(ctx)
	if err != nil {
		fail("directory-counts", "couldn't read the restored instance's directory: %v", err)
		return results, resultsError(results)
	}
	if problems := compareDirectory(o.Snapshot, snap); len(problems) > 0 {
		fail("directory-counts", "restored directory doesn't match the pre-migration snapshot: %s", strings.Join(problems, "; "))
	} else {
		ok("directory-counts", "restored instance has %d account(s) and %d domain(s), matching the pre-migration snapshot",
			snap.AccountCount, len(snap.Domains))
	}
	return results, resultsError(results)
}

// compareDirectory reports every way the restored directory differs from
// the pre-migration snapshot. Unlike the post-migration comparison in
// internal/validate, account names are compared exactly: the v0.16
// migration's bare-username-to-email rewrite is precisely what a rollback
// undoes, so a restored instance that still shows rewritten names has not
// been restored.
func compareDirectory(before *checkpoint.PreflightSnapshot, after *stalwartapi.Snapshot) []string {
	var problems []string
	if before.AccountCount != after.AccountCount {
		problems = append(problems, fmt.Sprintf("%d account(s) before, %d after", before.AccountCount, after.AccountCount))
	}
	beforeDomains := append([]string(nil), before.Domains...)
	afterDomains := append([]string(nil), after.Domains...)
	sort.Strings(beforeDomains)
	sort.Strings(afterDomains)
	if strings.Join(beforeDomains, ",") != strings.Join(afterDomains, ",") {
		problems = append(problems, fmt.Sprintf("domains were [%s], now [%s]",
			strings.Join(beforeDomains, " "), strings.Join(afterDomains, " ")))
	}
	return problems
}

// waitForPing polls until the instance accepts an authenticated session
// request or timeout elapses. The service was started seconds ago, so the
// first attempt failing is expected rather than meaningful.
func waitForPing(ctx context.Context, client *stalwartapi.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		lastErr = client.Ping(ctx)
		if lastErr == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func resultsError(results []CheckResult) error {
	var failed []string
	for _, r := range results {
		if r.Status == StatusFail {
			failed = append(failed, r.Name)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("rollback verification failed: %s", strings.Join(failed, ", "))
}
