// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package stalwartapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Quota recalculation is the last step of ARCHITECTURE.md §4.5, and the one
// post-migration task Stalwart's own v0.16 upgrade guide is emphatic about:
// "Disk quotas were reset to zero during the wipe and need to be rebuilt
// from the actual mailbox contents."
//
// The guide only documents doing this through the WebUI's Tasks panel, so
// the wire format below comes from Stalwart's schema reference for the
// x:Task object (docs/ref/object/task/) rather than from the upgrade guide:
// x:Task/set with an "AccountMaintenance" variant whose maintenanceType is
// "recalculateQuota", one per account, exactly as the WebUI's own
// "Recalculate disk quotas" fans out into one subtask per user account.
//
// Two things about this remain unverified against a running server, and
// callers are expected to treat a failure here as non-fatal for that
// reason:
//
//   - The schema reference annotates accountId and maintenanceType as
//     read-only. Read-only in that reference means immutable after
//     creation (they have to be settable at create time or the variant
//     couldn't be created at all), but that reading hasn't been confirmed
//     against a live instance.
//   - TaskStatus documents Pending, Retry and Failed, with no state for
//     "finished successfully" - so completion is inferred from a task
//     disappearing from the queue, which is the natural reading of a work
//     queue whose entries are consumed, not a documented guarantee.
const (
	taskTypeAccountMaintenance  = "AccountMaintenance"
	taskTypeTenantMaintenance   = "TenantMaintenance"
	maintenanceRecalculateQuota = "recalculateQuota"
)

// TaskFailure is one task that reached the terminal Failed state.
type TaskFailure struct {
	TaskID string
	Reason string
}

func (f TaskFailure) String() string {
	if f.Reason == "" {
		return f.TaskID
	}
	return f.TaskID + ": " + f.Reason
}

// AccountIDs returns every account's id, without the per-mailbox walk
// AccountSnapshot does. Quota recalculation needs the ids and nothing else,
// and on a large install the mailbox walk is the expensive part.
func (c *Client) AccountIDs(ctx context.Context) ([]string, error) {
	responses, err := c.call(ctx, managementCapabilities, []any{
		[]any{"x:Account/query", map[string]any{"filter": map[string]any{}}, "q"},
	})
	if err != nil {
		return nil, fmt.Errorf("stalwartapi: Account/query: %w", err)
	}
	return accountQueryIDs(responses)
}

// CreateQuotaRecalculationTasks schedules one recalculateQuota maintenance
// task per account id, in a single x:Task/set call, and returns the ids of
// the tasks the server said it created.
//
// A creation the server rejects is returned as an error naming the account
// and the server's own reason, rather than being counted as scheduled -
// silently reporting "quotas recalculated" for an account whose task was
// never accepted is precisely the sort of thing this tool exists not to do.
func (c *Client) CreateQuotaRecalculationTasks(ctx context.Context, accountIDs []string) ([]string, error) {
	if len(accountIDs) == 0 {
		return nil, nil
	}
	create := make(map[string]any, len(accountIDs))
	creationIDForAccount := make(map[string]string, len(accountIDs))
	for i, accountID := range accountIDs {
		creationID := fmt.Sprintf("q%d", i)
		creationIDForAccount[creationID] = accountID
		create[creationID] = map[string]any{
			"@type":           taskTypeAccountMaintenance,
			"accountId":       accountID,
			"maintenanceType": maintenanceRecalculateQuota,
			"status":          map[string]any{"@type": "Pending"},
		}
	}
	return c.setTasks(ctx, create, creationIDForAccount, "account")
}

// CreateTenantQuotaRecalculationTasks does the same for tenant-level
// counters. Stalwart's upgrade guide is explicit that this runs *after*
// per-account recalculation has finished for every user, since it
// aggregates those per-account totals - so callers must wait on
// CreateQuotaRecalculationTasks before calling this, not run both at once.
func (c *Client) CreateTenantQuotaRecalculationTasks(ctx context.Context, tenantIDs []string) ([]string, error) {
	if len(tenantIDs) == 0 {
		return nil, nil
	}
	create := make(map[string]any, len(tenantIDs))
	creationIDForTenant := make(map[string]string, len(tenantIDs))
	for i, tenantID := range tenantIDs {
		creationID := fmt.Sprintf("t%d", i)
		creationIDForTenant[creationID] = tenantID
		create[creationID] = map[string]any{
			"@type":           taskTypeTenantMaintenance,
			"tenantId":        tenantID,
			"maintenanceType": maintenanceRecalculateQuota,
			"status":          map[string]any{"@type": "Pending"},
		}
	}
	return c.setTasks(ctx, create, creationIDForTenant, "tenant")
}

func (c *Client) setTasks(ctx context.Context, create map[string]any, subjectFor map[string]string, subjectKind string) ([]string, error) {
	responses, err := c.call(ctx, managementCapabilities, []any{
		[]any{"x:Task/set", map[string]any{"create": create}, "s"},
	})
	if err != nil {
		return nil, fmt.Errorf("stalwartapi: Task/set: %w", err)
	}
	if len(responses) == 0 {
		return nil, fmt.Errorf("stalwartapi: Task/set returned no method responses")
	}
	r := responses[0]
	if r.Name == "error" {
		return nil, describeJMAPError("Task/set", r.Args)
	}
	var result struct {
		Created map[string]struct {
			ID string `json:"id"`
		} `json:"created"`
		NotCreated map[string]json.RawMessage `json:"notCreated"`
	}
	if err := json.Unmarshal(r.Args, &result); err != nil {
		return nil, fmt.Errorf("stalwartapi: parse Task/set response: %w", err)
	}

	if len(result.NotCreated) > 0 {
		rejected := make([]string, 0, len(result.NotCreated))
		for creationID, reason := range result.NotCreated {
			rejected = append(rejected, fmt.Sprintf("%s %s: %s", subjectKind, subjectFor[creationID], reason))
		}
		sort.Strings(rejected)
		return nil, fmt.Errorf("stalwartapi: Task/set refused %d of %d quota recalculation task(s): %s",
			len(result.NotCreated), len(create), strings.Join(rejected, "; "))
	}

	ids := make([]string, 0, len(result.Created))
	for _, created := range result.Created {
		ids = append(ids, created.ID)
	}
	sort.Strings(ids)
	if len(ids) != len(create) {
		return ids, fmt.Errorf("stalwartapi: Task/set created %d task(s) but %d were requested, and none were reported as refused",
			len(ids), len(create))
	}
	return ids, nil
}

// WaitForTasks polls x:Task/get until none of the given tasks are still in
// the queue, or timeout elapses. A task that has left the queue is treated
// as finished (see this file's opening comment on why that inference is
// necessary); one still present in the terminal Failed state is collected
// and reported rather than waited on forever.
//
// It returns the failures it observed. A non-nil error means the polling
// itself couldn't be completed - the queue couldn't be read, or the timeout
// elapsed while tasks were still pending - which is a different thing from
// "the tasks ran and some failed", and callers report them differently.
func (c *Client) WaitForTasks(ctx context.Context, taskIDs []string, timeout time.Duration) (failures []TaskFailure, err error) {
	if len(taskIDs) == 0 {
		return nil, nil
	}
	remaining := make(map[string]bool, len(taskIDs))
	for _, id := range taskIDs {
		remaining[id] = true
	}
	seenFailure := map[string]bool{}

	deadline := time.Now().Add(timeout)
	for {
		pending := make([]string, 0, len(remaining))
		for id := range remaining {
			pending = append(pending, id)
		}
		sort.Strings(pending)

		found, err := c.taskStatuses(ctx, pending)
		if err != nil {
			return failures, err
		}
		for id := range remaining {
			status, stillQueued := found[id]
			if !stillQueued {
				delete(remaining, id) // consumed by the queue: finished
				continue
			}
			if status.Type == "Failed" && !seenFailure[id] {
				seenFailure[id] = true
				failures = append(failures, TaskFailure{TaskID: id, Reason: status.FailureReason})
				delete(remaining, id)
			}
		}
		if len(remaining) == 0 {
			sort.Slice(failures, func(i, j int) bool { return failures[i].TaskID < failures[j].TaskID })
			return failures, nil
		}
		if !time.Now().Before(deadline) {
			return failures, fmt.Errorf("stalwartapi: %d quota recalculation task(s) were still queued after %s - they may simply need longer on a large install; check the Tasks panel rather than assuming they failed",
				len(remaining), timeout)
		}
		select {
		case <-ctx.Done():
			return failures, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

type taskStatus struct {
	Type          string
	FailureReason string
}

// taskStatuses fetches the given tasks, returning only those the server
// still knows about, keyed by id.
func (c *Client) taskStatuses(ctx context.Context, ids []string) (map[string]taskStatus, error) {
	responses, err := c.call(ctx, managementCapabilities, []any{
		[]any{"x:Task/get", map[string]any{"ids": ids, "properties": []string{"id", "status"}}, "g"},
	})
	if err != nil {
		return nil, fmt.Errorf("stalwartapi: Task/get: %w", err)
	}
	if len(responses) == 0 {
		return nil, fmt.Errorf("stalwartapi: Task/get returned no method responses")
	}
	r := responses[0]
	if r.Name == "error" {
		return nil, describeJMAPError("Task/get", r.Args)
	}
	var result struct {
		List []struct {
			ID     string `json:"id"`
			Status struct {
				Type          string `json:"@type"`
				FailureReason string `json:"failureReason"`
			} `json:"status"`
		} `json:"list"`
	}
	if err := json.Unmarshal(r.Args, &result); err != nil {
		return nil, fmt.Errorf("stalwartapi: parse Task/get response: %w", err)
	}
	statuses := make(map[string]taskStatus, len(result.List))
	for _, t := range result.List {
		statuses[t.ID] = taskStatus{Type: t.Status.Type, FailureReason: t.Status.FailureReason}
	}
	return statuses, nil
}

// WaitForPing polls until the instance accepts an authenticated session
// request or timeout elapses. Cutover uses it after starting the migrated
// service, which came up seconds earlier - so early failures are expected
// rather than meaningful.
func (c *Client) WaitForPing(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := c.Ping(ctx)
		if err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// WaitForResponse polls until the instance answers an HTTP request at all,
// whatever the status, or timeout elapses.
//
// This is the liveness question, and it is deliberately separate from
// WaitForPing's "and my credentials work". A 401 proves the server is up,
// listening and routing - which is exactly what a caller waiting for a
// restarted service needs to know. Conflating the two failed a cutover
// that had in fact succeeded: the credentials supplied for the
// pre-migration instance were a config fallback-admin, which does not
// survive into v0.16 (its config is a store pointer, so the old
// [authentication.fallback-admin] block is simply gone), so every poll came
// back 401 and the phase reported the service as never having answered.
func (c *Client) WaitForResponse(ctx context.Context, timeout time.Duration) error {
	url := strings.TrimRight(c.BaseURL, "/") + "/.well-known/jmap"
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.SetBasicAuth(c.Username, c.Password)
		resp, err := c.httpClient().Do(req)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		lastErr = err
		if !time.Now().Before(deadline) {
			return fmt.Errorf("stalwartapi: %s did not respond within %s: %w", url, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
