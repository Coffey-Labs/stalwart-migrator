// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package validate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/stalwartapi"
)

// MailboxDelta is one mailbox whose message count didn't match between the
// pre- and post-migration snapshots.
type MailboxDelta struct {
	Account string
	Mailbox string
	Before  int
	After   int
}

// ContentIntegrityResult is the outcome of comparing a pre-migration
// snapshot against a freshly captured post-migration one - the
// no-data-loss check described in ARCHITECTURE.md §4.7.
//
// MessageCountsCompared is the field that decides how much this result is
// worth, and it is not always true. Stalwart 0.15.x exposes no per-mailbox
// message counts at any endpoint, and its impersonation login (which 0.16
// offers) returns 401 - so on the 0.15/0.16 boundary migration there are no
// "before" counts to compare and this check can only assert that every
// account and domain survived. Saying so is the whole point: an earlier
// version of this comparison iterated the before-counts map, found it
// empty, and reported "all message counts match" having checked nothing.
type ContentIntegrityResult struct {
	AccountsChecked        int
	MailboxesChecked       int
	MissingAccounts        []string       // present before, not found after (even accounting for the email-address rewrite)
	MessageCountMismatches []MailboxDelta // present both before and after, but with a different message count
	MissingDomains         []string       // present before, absent after
	MessageCountsCompared  bool           // false when the source version could not report counts
}

// OK reports whether everything that must match did: no account and no mail
// went missing. Read it together with MessageCountsCompared: OK with that
// false means "the directory survived", not "no mail was lost".
//
// Domains are deliberately not part of this. What the two versions call a
// domain differs across the 0.15/0.16 boundary — principals on one side,
// Domain objects on the other, with aliases and account-less domains
// counted differently — and we have already been caught once reporting a
// migration that lost nothing as having lost domains. A disagreement there
// is worth showing an operator; it is not worth failing a migration over,
// where a missing account is.
func (r ContentIntegrityResult) OK() bool {
	return len(r.MissingAccounts) == 0 && len(r.MessageCountMismatches) == 0
}

// DomainsOK reports whether every domain seen before the migration is still
// listed after it.
func (r ContentIntegrityResult) DomainsOK() bool {
	return len(r.MissingDomains) == 0
}

func (r ContentIntegrityResult) String() string {
	var b strings.Builder
	if r.MessageCountsCompared {
		fmt.Fprintf(&b, "content integrity: %d account(s), %d mailbox(es) checked", r.AccountsChecked, r.MailboxesChecked)
	} else {
		fmt.Fprintf(&b, "content integrity: %d account(s) and their domains checked; MESSAGE COUNTS NOT COMPARED "+
			"(this migration's source version reports no per-mailbox counts, so no-data-loss is NOT verified here - "+
			"only that every account and domain survived)", r.AccountsChecked)
	}
	// Everything below is a finding, so return early only when there is
	// nothing at all to report - domains included, even though they no
	// longer fail the run. A warning nobody can read is not a warning.
	if r.OK() && r.DomainsOK() {
		if r.MessageCountsCompared {
			b.WriteString(", all message counts match")
		}
		return b.String()
	}
	for _, a := range r.MissingAccounts {
		fmt.Fprintf(&b, "; MISSING ACCOUNT %s", a)
	}
	for _, d := range r.MissingDomains {
		fmt.Fprintf(&b, "; MISSING DOMAIN %s", d)
	}
	for _, d := range r.MessageCountMismatches {
		fmt.Fprintf(&b, "; MESSAGE COUNT MISMATCH %s/%s: %d before, %d after", d.Account, d.Mailbox, d.Before, d.After)
	}
	return b.String()
}

// compareContentIntegrity captures a fresh snapshot via client and compares
// it against before, matching accounts by exact name first and falling
// back to the local part (the text before "@") since Stalwart's v0.16
// migration rewrites bare usernames to full email addresses
// (UPGRADING/v0_16.md: "the migration script automatically assigns the
// default domain to accounts lacking one") - an exact-string comparison
// alone would misreport every rewritten account as missing.
func compareContentIntegrity(ctx context.Context, client *stalwartapi.Client, before *checkpoint.PreflightSnapshot) (*ContentIntegrityResult, error) {
	after, err := client.AccountSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("capture post-migration snapshot: %w", err)
	}

	result := &ContentIntegrityResult{MessageCountsCompared: len(before.MailboxCounts) > 0}

	// The set of accounts to verify comes from whichever pre-migration
	// facts the source version was able to report: mailbox counts when it
	// had them (0.16+), otherwise the used-quota map, which the 0.15.x REST
	// principal list does populate. Deriving it from MailboxCounts alone
	// would silently check nothing on a 0.15 source.
	beforeAccountSet := map[string]bool{}
	for a := range before.MailboxCounts {
		beforeAccountSet[a] = true
	}
	for a := range before.UsedQuota {
		beforeAccountSet[a] = true
	}
	beforeAccounts := make([]string, 0, len(beforeAccountSet))
	for a := range beforeAccountSet {
		beforeAccounts = append(beforeAccounts, a)
	}
	sort.Strings(beforeAccounts)

	// Likewise for the "after" side: an account that exists but whose
	// mailboxes couldn't be read still counts as present.
	afterAccounts := map[string]bool{}
	for a := range after.MailboxCounts {
		afterAccounts[a] = true
	}
	for a := range after.UsedQuota {
		afterAccounts[a] = true
	}
	for a := range after.MailboxErrors {
		afterAccounts[a] = true
	}

	for _, d := range before.Domains {
		if !containsDomain(after.Domains, d) {
			result.MissingDomains = append(result.MissingDomains, d)
		}
	}

	for _, beforeAccount := range beforeAccounts {
		result.AccountsChecked++
		if !accountPresent(afterAccounts, beforeAccount) {
			result.MissingAccounts = append(result.MissingAccounts, beforeAccount)
			continue
		}
		if !result.MessageCountsCompared {
			continue // nothing to compare counts against; presence is all this source could give
		}
		afterMailboxes, found := after.MailboxCounts[beforeAccount]
		if !found {
			afterMailboxes, found = findByLocalPart(after.MailboxCounts, beforeAccount)
		}
		if !found {
			result.MissingAccounts = append(result.MissingAccounts, beforeAccount)
			continue
		}

		afterByName := make(map[string]int, len(afterMailboxes))
		for _, m := range afterMailboxes {
			afterByName[m.Mailbox] = m.Messages
		}

		beforeMailboxes := append([]checkpoint.MailboxCount(nil), before.MailboxCounts[beforeAccount]...)
		sort.Slice(beforeMailboxes, func(i, j int) bool { return beforeMailboxes[i].Mailbox < beforeMailboxes[j].Mailbox })
		for _, bm := range beforeMailboxes {
			result.MailboxesChecked++
			afterCount, ok := afterByName[bm.Mailbox]
			if !ok || afterCount != bm.Messages {
				result.MessageCountMismatches = append(result.MessageCountMismatches, MailboxDelta{
					Account: beforeAccount, Mailbox: bm.Mailbox, Before: bm.Messages, After: afterCount,
				})
			}
		}
	}
	return result, nil
}

// accountPresent matches an account against the post-migration set the
// same way findByLocalPart does, so the v0.16 rewrite of bare usernames
// into full addresses doesn't read as every account having vanished.
func accountPresent(afterAccounts map[string]bool, beforeAccount string) bool {
	if afterAccounts[beforeAccount] {
		return true
	}
	local := strings.SplitN(beforeAccount, "@", 2)[0]
	for a := range afterAccounts {
		if strings.SplitN(a, "@", 2)[0] == local {
			return true
		}
	}
	return false
}

// containsDomain matches domains exactly; unlike account names, the
// migration does not rewrite them.
func containsDomain(domains []string, want string) bool {
	for _, d := range domains {
		if d == want {
			return true
		}
	}
	return false
}

func findByLocalPart(mailboxCounts map[string][]stalwartapi.MailboxCount, beforeAccount string) ([]stalwartapi.MailboxCount, bool) {
	local := strings.SplitN(beforeAccount, "@", 2)[0]
	for afterAccount, mb := range mailboxCounts {
		if strings.SplitN(afterAccount, "@", 2)[0] == local {
			return mb, true
		}
	}
	return nil, false
}
