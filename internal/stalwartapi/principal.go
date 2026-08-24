// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package stalwartapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Stalwart 0.15.x - the version this tool migrates *from* - has no
// urn:stalwart:jmap capability and no JMAP management endpoint: POST /api
// returns 404 there. Its management API is REST, at GET /api/principal,
// and that is the only way to enumerate the pre-migration directory.
//
// This was found by running preflight against a real 0.15.5 instance, not
// from documentation: the published schema reference documents 0.16, and
// following it alone produced a tool that could not read the source
// version it exists to migrate. The shapes below were confirmed against
// that live server.
//
//	GET /api/principal?types=individual&limit=100&page=1
//	{"data":{"items":[{"id":4,"type":"individual","name":"alice",
//	                   "emails":["alice@smoke.test"],"usedQuota":9207}],
//	         "total":3}}
//
// Two limits of the 0.15 side are worth stating plainly, because they
// decide what the post-migration comparison can actually assert:
//
//   - There is no per-mailbox message count anywhere in this API, and the
//     impersonation mechanism 0.16 offers (the `<target>%<impersonator>`
//     composite login MailboxSnapshot uses) returns 401 on 0.15.5. So a
//     pre-migration snapshot cannot carry message counts, and a
//     before/after comparison of them is impossible for the boundary
//     migration this tool is built for.
//   - What both versions do expose per account is used quota in bytes
//     (`usedQuota` here, `usedDiskQuota` on 0.16's x:Account). That is a
//     real content measure - it moves when mail is lost - so it is what
//     the integrity comparison uses across the boundary.
const restPrincipalPageSize = 100

// stalwartManagementCapability is advertised by instances whose management
// API is the JMAP one (0.16+). Its absence is what distinguishes a 0.15.x
// instance, and is a positive signal rather than an inference from a failed
// call.
const stalwartManagementCapability = "urn:stalwart:jmap"

// hasJMAPManagement reports whether this instance speaks the 0.16+ JMAP
// management API, by reading the capability list from its session
// document. It deliberately does its own request rather than reusing
// fetchSession: that helper also requires an apiUrl and a mail account,
// which are needed for impersonated mailbox reads but have nothing to do
// with which management API to use - failing dispatch over a missing
// apiUrl would misroute an instance that is perfectly readable.
func (c *Client) hasJMAPManagement(ctx context.Context) (bool, error) {
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/.well-known/jmap"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	req.SetBasicAuth(c.Username, c.Password)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return false, fmt.Errorf("stalwartapi: reach %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("stalwartapi: session discovery at %s returned %s", endpoint, resp.Status)
	}
	var session struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return false, fmt.Errorf("stalwartapi: parse session document from %s: %w", endpoint, err)
	}
	_, ok := session.Capabilities[stalwartManagementCapability]
	return ok, nil
}

type restPrincipal struct {
	ID        int      `json:"id"`
	Type      string   `json:"type"`
	Name      string   `json:"name"`
	Emails    []string `json:"emails"`
	UsedQuota int64    `json:"usedQuota"`
}

type restPrincipalPage struct {
	Data struct {
		Items []restPrincipal `json:"items"`
		Total int             `json:"total"`
	} `json:"data"`
}

// restPrincipals fetches every principal of the given type, following the
// API's 1-based page/limit pagination rather than assuming one request
// returns everything - an install with more accounts than the page size
// would otherwise be silently truncated, and a truncated "before" snapshot
// would make the post-migration comparison claim more than it checked.
func (c *Client) restPrincipals(ctx context.Context, principalType string) ([]restPrincipal, error) {
	var all []restPrincipal
	for page := 1; ; page++ {
		q := url.Values{}
		if principalType != "" {
			q.Set("types", principalType)
		}
		q.Set("limit", fmt.Sprint(restPrincipalPageSize))
		q.Set("page", fmt.Sprint(page))
		endpoint := strings.TrimRight(c.BaseURL, "/") + "/api/principal?" + q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(c.Username, c.Password)
		resp, err := c.httpClient().Do(req)
		if err != nil {
			return nil, fmt.Errorf("stalwartapi: list principals: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("stalwartapi: GET %s returned %s: %s", endpoint, resp.Status, strings.TrimSpace(string(body)))
		}
		if readErr != nil {
			return nil, fmt.Errorf("stalwartapi: read principal list: %w", readErr)
		}
		var parsed restPrincipalPage
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("stalwartapi: parse principal list: %w", err)
		}
		all = append(all, parsed.Data.Items...)
		if len(parsed.Data.Items) == 0 || len(all) >= parsed.Data.Total {
			return all, nil
		}
	}
}

// principalSnapshotREST builds a Snapshot from the 0.15.x REST management
// API. MailboxCounts is deliberately left empty: see this file's opening
// comment for why counts cannot be obtained from a 0.15 instance at all.
func (c *Client) principalSnapshotREST(ctx context.Context) (*Snapshot, error) {
	individuals, err := c.restPrincipals(ctx, "individual")
	if err != nil {
		return nil, err
	}
	domainPrincipals, err := c.restPrincipals(ctx, "domain")
	if err != nil {
		return nil, err
	}

	snap := &Snapshot{
		AccountCount: len(individuals),
		UsedQuota:    make(map[string]int64, len(individuals)),
	}
	for _, p := range individuals {
		snap.UsedQuota[accountKey(p)] = p.UsedQuota
	}

	domainSet := map[string]bool{}
	for _, d := range domainPrincipals {
		if d.Name != "" {
			domainSet[d.Name] = true
		}
	}
	// Fall back to the domains implied by account addresses if the
	// instance has no explicit domain principals.
	for _, p := range individuals {
		for _, email := range p.Emails {
			if at := strings.LastIndex(email, "@"); at >= 0 && at+1 < len(email) {
				domainSet[email[at+1:]] = true
			}
		}
	}
	for d := range domainSet {
		snap.Domains = append(snap.Domains, d)
	}
	sort.Strings(snap.Domains)
	return snap, nil
}

// accountKey identifies an account the same way both API generations can:
// by its primary email address where it has one, falling back to the bare
// login name. The v0.16 migration rewrites bare names into addresses, which
// is exactly why the comparison side matches on local part as well.
func accountKey(p restPrincipal) string {
	if len(p.Emails) > 0 && p.Emails[0] != "" {
		return p.Emails[0]
	}
	return p.Name
}
