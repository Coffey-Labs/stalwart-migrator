// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package stalwartapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
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
