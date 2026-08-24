// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package stalwartapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// jmapSession is the subset of RFC 8620 §2's Session object this package
// needs: apiUrl (where to POST standard JMAP method calls - a different
// endpoint from Stalwart's /api management API, confirmed in
// docs/ref/object/account.md) and primaryAccounts (which accountId the
// urn:ietf:params:jmap:mail capability maps to for the authenticated
// identity).
type jmapSession struct {
	APIURL          string            `json:"apiUrl"`
	PrimaryAccounts map[string]string `json:"primaryAccounts"`
}

const jmapMailCapability = "urn:ietf:params:jmap:mail"

// fetchSession performs JMAP session discovery (RFC 8620 §2,
// /.well-known/jmap) with the given credentials.
func (c *Client) fetchSession(ctx context.Context, username, password string) (*jmapSession, error) {
	url := strings.TrimRight(c.BaseURL, "/") + "/.well-known/jmap"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(username, password)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("session discovery at %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("session discovery at %s returned %s", url, resp.Status)
	}
	var session jmapSession
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return nil, fmt.Errorf("parse session document from %s: %w", url, err)
	}
	if session.APIURL == "" {
		return nil, fmt.Errorf("session document from %s has no apiUrl", url)
	}
	return &session, nil
}

type mailboxGetEntry struct {
	Name        string `json:"name"`
	TotalEmails int    `json:"totalEmails"`
}

// MailboxSnapshot captures every mailbox's message count for one account,
// authenticating as that account via Stalwart's documented impersonation
// mechanism rather than assuming this Client's own credentials get direct
// cross-account access. That assumption would be wrong: Stalwart's JMAP
// session `accounts`/`primaryAccounts` map is built only from the
// authenticated identity's own membership and sharing grants - it is NOT
// expanded for a superuser (confirmed against
// crates/jmap/src/api/session.rs), so a plain Mailbox/get call for an
// arbitrary accountId under this Client's own login would be rejected.
//
// Instead, this Client's Username must hold Stalwart's `impersonate`
// permission (see docs/auth/authorization/administrator.md), and this
// method logs in AS the target account using the documented composite
// login format "<target>%<impersonator>" with the impersonator's password,
// then calls standard RFC 8621 Mailbox/get - reading the exact wire
// property name `totalEmails` - against the URL that account's own JMAP
// session document reports as apiUrl (not the /api management endpoint
// x:Account/* uses; confirmed as a distinct endpoint in
// docs/ref/object/account.md).
func (c *Client) MailboxSnapshot(ctx context.Context, targetEmail string) ([]MailboxCount, error) {
	impersonatedUser := fmt.Sprintf("%s%%%s", targetEmail, c.Username)

	session, err := c.fetchSession(ctx, impersonatedUser, c.Password)
	if err != nil {
		return nil, fmt.Errorf("stalwartapi: impersonate %s: %w", targetEmail, err)
	}
	accountID, ok := session.PrimaryAccounts[jmapMailCapability]
	if !ok || accountID == "" {
		return nil, fmt.Errorf("stalwartapi: impersonated session for %s has no %s account", targetEmail, jmapMailCapability)
	}

	responses, err := c.callAs(ctx, impersonatedUser, c.Password, session.APIURL,
		[]string{"urn:ietf:params:jmap:core", jmapMailCapability},
		[]any{[]any{"Mailbox/get", map[string]any{"accountId": accountID, "properties": []string{"name", "totalEmails"}}, "m"}},
	)
	if err != nil {
		return nil, fmt.Errorf("stalwartapi: Mailbox/get for %s: %w", targetEmail, err)
	}
	if len(responses) == 0 {
		return nil, fmt.Errorf("stalwartapi: Mailbox/get for %s returned no method responses", targetEmail)
	}
	if responses[0].Name == "error" {
		return nil, fmt.Errorf("stalwartapi: Mailbox/get for %s error: %s", targetEmail, responses[0].Args)
	}

	var result struct {
		List []mailboxGetEntry `json:"list"`
	}
	if err := json.Unmarshal(responses[0].Args, &result); err != nil {
		return nil, fmt.Errorf("stalwartapi: parse Mailbox/get response for %s: %w", targetEmail, err)
	}
	counts := make([]MailboxCount, len(result.List))
	for i, m := range result.List {
		counts[i] = MailboxCount{Mailbox: m.Name, Messages: m.TotalEmails}
	}
	return counts, nil
}
