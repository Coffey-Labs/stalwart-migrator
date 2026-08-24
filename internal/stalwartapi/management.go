// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package stalwartapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// managementCapabilities are the JMAP capability URNs Stalwart requires for
// its management object calls (x:Account/*): standard JMAP core plus its
// own urn:stalwart:jmap extension. Confirmed against
// docs/ref/object/account.md and crates/jmap-proto/src/request/capability.rs
// in stalwartlabs/stalwart.
var managementCapabilities = []string{"urn:ietf:params:jmap:core", "urn:stalwart:jmap"}

type jmapRequest struct {
	Using       []string `json:"using"`
	MethodCalls []any    `json:"methodCalls"`
}

type jmapRawResponse struct {
	MethodResponses []json.RawMessage `json:"methodResponses"`
}

// methodResponse is one [name, args, callId] triple from a JMAP response,
// per RFC 8620 §3.2 - Stalwart's management API follows the same envelope
// shape as its regular JMAP methods.
type methodResponse struct {
	Name   string
	Args   json.RawMessage
	CallID string
}

// call POSTs one JMAP-style request to the management API using this
// Client's own credentials, and returns its parsed method responses in
// order. The endpoint is discovered from the instance's session document -
// see apiEndpoint for why it is not simply BaseURL + "/api".
func (c *Client) call(ctx context.Context, using []string, methodCalls []any) ([]methodResponse, error) {
	endpoint, err := c.apiEndpoint(ctx)
	if err != nil {
		return nil, err
	}
	return c.callAs(ctx, c.Username, c.Password, endpoint, using, methodCalls)
}

// callAs is call's underlying primitive: it accepts an explicit
// username/password/URL rather than always using this Client's own
// credentials and the management endpoint. MailboxSnapshot uses this to
// call standard JMAP methods (not Stalwart's x: management objects) against
// the URL a JMAP session document says to use, authenticated as an
// impersonated identity rather than this Client's own.
func (c *Client) callAs(ctx context.Context, username, password, url string, using []string, methodCalls []any) ([]methodResponse, error) {
	reqBody, err := json.Marshal(jmapRequest{Using: using, MethodCalls: methodCalls})
	if err != nil {
		return nil, fmt.Errorf("stalwartapi: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(username, password)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("stalwartapi: call %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("stalwartapi: %s returned %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}

	var raw jmapRawResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("stalwartapi: decode response from %s: %w", url, err)
	}
	responses := make([]methodResponse, 0, len(raw.MethodResponses))
	for _, r := range raw.MethodResponses {
		var triple [3]json.RawMessage
		if err := json.Unmarshal(r, &triple); err != nil {
			return nil, fmt.Errorf("stalwartapi: parse method response envelope: %w", err)
		}
		var name, callID string
		if err := json.Unmarshal(triple[0], &name); err != nil {
			return nil, fmt.Errorf("stalwartapi: parse method response name: %w", err)
		}
		if err := json.Unmarshal(triple[2], &callID); err != nil {
			return nil, fmt.Errorf("stalwartapi: parse method response call id: %w", err)
		}
		responses = append(responses, methodResponse{Name: name, Args: triple[1], CallID: callID})
	}
	return responses, nil
}

// account is the subset of x:Account/get's response fields this tool needs,
// confirmed against Stalwart's own docs/ref/object/account.md (whose
// stalwart-cli example is `query Account --fields id,name,domainId,usedDiskQuota`).
type account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	DomainID string `json:"domainId"`
	// UsedDiskQuota is 0.16's name for what 0.15's REST API calls
	// usedQuota - the same per-account byte count, which is what makes a
	// cross-boundary content comparison possible at all.
	UsedDiskQuota int64 `json:"usedDiskQuota"`
}

// AccountSnapshot enumerates every account on the instance via Stalwart's
// management API - x:Account/query to list ids, then x:Account/get to fetch
// their name/domainId - and returns the account count, set of domains in
// use, and (via MailboxSnapshot, per account) every mailbox's message
// count. This is what preflight's snapshot (ARCHITECTURE.md §4.1) and
// validate's directory-integrity and content-integrity checks (§4.7)
// compare before and after migration - the latter being the actual
// no-data-loss guarantee.
//
// A per-account mailbox-count failure (most likely: this Client's Username
// lacks the `impersonate` permission MailboxSnapshot depends on) does not
// fail the whole snapshot - the account/domain enumeration above is already
// useful on its own, and one account's failure shouldn't hide a working
// result for every other account. Instead it's recorded in
// Snapshot.MailboxErrors, keyed by account email, so callers can report
// exactly what's missing rather than silently treating an unreachable
// account's mailboxes as having zero messages.
func (c *Client) AccountSnapshot(ctx context.Context) (*Snapshot, error) {
	isREST, err := c.hasRESTManagement(ctx)
	if err != nil {
		return nil, fmt.Errorf("stalwartapi: discover which management API this instance speaks: %w", err)
	}
	if isREST {
		// v0.15.x: REST at /api/principal. See principal.go.
		return c.principalSnapshotREST(ctx)
	}
	return c.accountSnapshotJMAP(ctx)
}

// accountSnapshotJMAP is the 0.16+ path, via Stalwart's JMAP management
// objects.
func (c *Client) accountSnapshotJMAP(ctx context.Context) (*Snapshot, error) {
	ids, err := c.AccountIDs(ctx)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return &Snapshot{}, nil
	}

	getResp, err := c.call(ctx, managementCapabilities, []any{
		[]any{"x:Account/get", map[string]any{"ids": ids, "properties": []string{"id", "name", "domainId", "usedDiskQuota"}}, "g"},
	})
	if err != nil {
		return nil, fmt.Errorf("stalwartapi: Account/get: %w", err)
	}
	accounts, err := accountGetList(getResp)
	if err != nil {
		return nil, err
	}

	mailboxCounts := map[string][]MailboxCount{}
	mailboxErrors := map[string]string{}
	for _, a := range accounts {
		if a.Name == "" {
			continue // no login/email to impersonate against
		}
		counts, err := c.MailboxSnapshot(ctx, a.Name)
		if err != nil {
			mailboxErrors[a.Name] = err.Error()
			continue
		}
		mailboxCounts[a.Name] = counts
	}

	// Resolve ids to names so this snapshot is comparable with one taken
	// from a v0.15 instance, which records names.
	domainNames, err := c.domainNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("stalwartapi: resolve domain names (an unresolved id would make every domain look missing): %w", err)
	}
	domainSet := map[string]bool{}
	for _, a := range accounts {
		if a.DomainID == "" {
			continue
		}
		if name, ok := domainNames[a.DomainID]; ok && name != "" {
			domainSet[name] = true
			continue
		}
		// Keep the raw id rather than dropping the domain entirely: a
		// domain that can't be named is still a domain that exists.
		domainSet[a.DomainID] = true
	}
	domains := make([]string, 0, len(domainSet))
	for d := range domainSet {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	usedQuota := make(map[string]int64, len(accounts))
	for _, a := range accounts {
		if a.Name != "" {
			usedQuota[a.Name] = a.UsedDiskQuota
		}
	}

	return &Snapshot{
		AccountCount:  len(accounts),
		Domains:       domains,
		MailboxCounts: mailboxCounts,
		MailboxErrors: mailboxErrors,
		UsedQuota:     usedQuota,
	}, nil
}

// describeJMAPError turns a JMAP method-level error into something an
// operator can act on.
//
// "forbidden" gets special handling because of where it shows up: against a
// freshly migrated instance, an account that held the admin role before the
// migration was refused x:Account/query afterwards. Whether the role failed
// to carry over or v0.16 requires different permissions was not isolated,
// but the operator's situation is the same either way - they have an admin
// account that can no longer administer - and a bare "forbidden" gives them
// nothing to go on.
func describeJMAPError(method string, args json.RawMessage) error {
	var parsed struct {
		Type        string `json:"type"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(args, &parsed); err != nil || parsed.Type == "" {
		return fmt.Errorf("stalwartapi: %s error: %s", method, args)
	}
	if parsed.Type == "forbidden" {
		return fmt.Errorf("stalwartapi: %s was refused (forbidden): %s - this account is authenticated but not "+
			"permitted to perform management operations. After a v0.16 migration this is worth checking first: an "+
			"account that held the admin role beforehand may not have it afterwards",
			method, parsed.Description)
	}
	if parsed.Description != "" {
		return fmt.Errorf("stalwartapi: %s error (%s): %s", method, parsed.Type, parsed.Description)
	}
	return fmt.Errorf("stalwartapi: %s error: %s", method, parsed.Type)
}

// domainNames resolves v0.16 domain ids to domain names.
//
// x:Account.domainId is an internal id ("b"), not a name. A pre-migration
// snapshot taken from a v0.15 instance records domains as names
// ("smoke.test"), so comparing the two directly reports every domain as
// having vanished - a false alarm on the check that is supposed to prove
// nothing was lost.
//
// The query and the get travel in one request using a JMAP back-reference
// (RFC 8620 §3.7), confirmed against a live 0.16.14:
//
//	["x:Domain/get", {"list":[{"name":"smoke.test","id":"b"}]}, "g"]
func (c *Client) domainNames(ctx context.Context) (map[string]string, error) {
	responses, err := c.call(ctx, managementCapabilities, []any{
		[]any{"x:Domain/query", map[string]any{"filter": map[string]any{}}, "q"},
		[]any{"x:Domain/get", map[string]any{
			"#ids":       map[string]any{"resultOf": "q", "name": "x:Domain/query", "path": "/ids"},
			"properties": []string{"id", "name"},
		}, "g"},
	})
	if err != nil {
		return nil, fmt.Errorf("stalwartapi: Domain/get: %w", err)
	}
	for _, r := range responses {
		if r.Name == "error" {
			return nil, describeJMAPError("Domain/get", r.Args)
		}
		if r.CallID != "g" {
			continue
		}
		var result struct {
			List []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"list"`
		}
		if err := json.Unmarshal(r.Args, &result); err != nil {
			return nil, fmt.Errorf("stalwartapi: parse Domain/get response: %w", err)
		}
		names := make(map[string]string, len(result.List))
		for _, d := range result.List {
			names[d.ID] = d.Name
		}
		return names, nil
	}
	return nil, fmt.Errorf("stalwartapi: Domain/get returned no matching method response")
}

func accountQueryIDs(responses []methodResponse) ([]string, error) {
	if len(responses) == 0 {
		return nil, fmt.Errorf("stalwartapi: Account/query returned no method responses")
	}
	r := responses[0]
	if r.Name == "error" {
		return nil, describeJMAPError("Account/query", r.Args)
	}
	var result struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(r.Args, &result); err != nil {
		return nil, fmt.Errorf("stalwartapi: parse Account/query response: %w", err)
	}
	return result.IDs, nil
}

func accountGetList(responses []methodResponse) ([]account, error) {
	if len(responses) == 0 {
		return nil, fmt.Errorf("stalwartapi: Account/get returned no method responses")
	}
	r := responses[0]
	if r.Name == "error" {
		return nil, describeJMAPError("Account/get", r.Args)
	}
	var result struct {
		List []account `json:"list"`
	}
	if err := json.Unmarshal(r.Args, &result); err != nil {
		return nil, fmt.Errorf("stalwartapi: parse Account/get response: %w", err)
	}
	return result.List, nil
}
