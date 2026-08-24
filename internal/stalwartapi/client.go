// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package stalwartapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client is the shared JMAP/management-API client every migration phase
// uses to talk to a Stalwart instance. It stays deliberately thin: phases
// that need Stalwart-specific behavior (recovery-mode control, apply-plan
// replay, account/mailbox introspection) get methods added here only as
// their wire-level details are confirmed against Stalwart's actual source
// and documentation, never guessed - see management.go and mailbox.go for
// what that grounding looked like for account enumeration and mailbox
// counts respectively.
type Client struct {
	BaseURL    string // e.g. "https://mail.example.com"
	Username   string
	Password   string
	HTTPClient *http.Client

	mu       sync.Mutex
	endpoint string // resolved JMAP endpoint; see apiEndpoint
}

// apiEndpoint returns the URL to POST JMAP method calls to, discovered from
// the instance's own session document rather than assumed.
//
// This used to be hardcoded as BaseURL + "/api", which is wrong for the
// version this tool migrates *to*: a fully configured, serving 0.16.14
// returns 404 for /api, and advertises its JMAP endpoint through the
// session document's apiUrl instead (RFC 8620 §2 - discovery is how a
// client is *supposed* to find it).
//
// The path is taken from apiUrl but re-based onto BaseURL's scheme and
// host. A real instance advertises its canonical public URL - observed:
// "https://mail.smoke.test/jmap/" - which frequently isn't reachable from
// where this tool runs, over a hostname that may not resolve or a
// certificate that may not validate. The operator told us how to reach
// this server when they passed --admin-url; the session is only authoritative
// about *where on it* the API lives.
func (c *Client) apiEndpoint(ctx context.Context) (string, error) {
	c.mu.Lock()
	cached := c.endpoint
	c.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	session, err := c.fetchSession(ctx, c.Username, c.Password)
	if err != nil {
		return "", fmt.Errorf("stalwartapi: discover the JMAP endpoint: %w", err)
	}
	resolved, err := c.rebaseOntoBaseURL(session.APIURL)
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	c.endpoint = resolved
	c.mu.Unlock()
	return resolved, nil
}

// rebaseOntoBaseURL keeps the advertised path but the operator's host.
func (c *Client) rebaseOntoBaseURL(apiURL string) (string, error) {
	base, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil {
		return "", fmt.Errorf("stalwartapi: parse base URL %q: %w", c.BaseURL, err)
	}
	if apiURL == "" {
		return "", fmt.Errorf("stalwartapi: the instance's session document advertises no apiUrl, so there is no JMAP endpoint to call")
	}
	advertised, err := url.Parse(apiURL)
	if err != nil {
		return "", fmt.Errorf("stalwartapi: parse advertised apiUrl %q: %w", apiURL, err)
	}
	base.Path = advertised.Path
	base.RawQuery = advertised.RawQuery
	return base.String(), nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Ping confirms the instance is reachable and the given credentials are
// accepted, via JMAP session discovery (RFC 8620 §2, the well-known
// /.well-known/jmap endpoint) over HTTP Basic auth. This is preflight's
// "dry-run" reachability check (ARCHITECTURE.md §4.1): it does nothing but
// read the session document, so it's safe to run against a live production
// server before anything else in the migration happens.
func (c *Client) Ping(ctx context.Context) error {
	url := strings.TrimRight(c.BaseURL, "/") + "/.well-known/jmap"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.Username, c.Password)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("stalwartapi: reach %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stalwartapi: session request to %s returned %s", url, resp.Status)
	}
	return nil
}

// Snapshot mirrors checkpoint.PreflightSnapshot's shape without this
// package depending on the checkpoint package's format.
type Snapshot struct {
	AccountCount  int
	Domains       []string
	MailboxCounts map[string][]MailboxCount // account email -> its mailboxes
	// UsedQuota is each account's used storage in bytes. Unlike
	// MailboxCounts it is available from both API generations - 0.15.x's
	// REST principal list and 0.16's x:Account both report it - which
	// makes it the only per-account content measure that can be compared
	// across the 0.15/0.16 boundary. See principal.go.
	UsedQuota map[string]int64
	// MailboxErrors records, per account email, why that account's mailbox
	// counts couldn't be captured (e.g. impersonation not permitted for
	// that account). A non-empty entry here means MailboxCounts has no
	// entry for that account - it's not silently treated as zero messages.
	MailboxErrors map[string]string
}

type MailboxCount struct {
	Mailbox  string
	Messages int
}
