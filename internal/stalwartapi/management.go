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

// call POSTs one JMAP-style request to the management API (Stalwart's /api
// endpoint - see docs/ref/object/account.md, which is distinct from /jmap)
// using this Client's own credentials, and returns its parsed method
// responses in order.
func (c *Client) call(ctx context.Context, using []string, methodCalls []any) ([]methodResponse, error) {
	return c.callAs(ctx, c.Username, c.Password, strings.TrimRight(c.BaseURL, "/")+"/api", using, methodCalls)
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
	ids, err := c.AccountIDs(ctx)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return &Snapshot{}, nil
	}

	getResp, err := c.call(ctx, managementCapabilities, []any{
		[]any{"x:Account/get", map[string]any{"ids": ids, "properties": []string{"id", "name", "domainId"}}, "g"},
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

	domainSet := map[string]bool{}
	for _, a := range accounts {
		if a.DomainID != "" {
			domainSet[a.DomainID] = true
		}
	}
	domains := make([]string, 0, len(domainSet))
	for d := range domainSet {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	return &Snapshot{
		AccountCount:  len(accounts),
		Domains:       domains,
		MailboxCounts: mailboxCounts,
		MailboxErrors: mailboxErrors,
	}, nil
}

func accountQueryIDs(responses []methodResponse) ([]string, error) {
	if len(responses) == 0 {
		return nil, fmt.Errorf("stalwartapi: Account/query returned no method responses")
	}
	r := responses[0]
	if r.Name == "error" {
		return nil, fmt.Errorf("stalwartapi: Account/query error: %s", r.Args)
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
		return nil, fmt.Errorf("stalwartapi: Account/get error: %s", r.Args)
	}
	var result struct {
		List []account `json:"list"`
	}
	if err := json.Unmarshal(r.Args, &result); err != nil {
		return nil, fmt.Errorf("stalwartapi: parse Account/get response: %w", err)
	}
	return result.List, nil
}
