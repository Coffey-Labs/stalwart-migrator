package validate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/johnellis/stalwart-migrator/internal/checkpoint"
	"github.com/johnellis/stalwart-migrator/internal/stalwartapi"
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
// snapshot against a freshly captured post-migration one - the actual
// no-data-loss check described in ARCHITECTURE.md §4.7.
type ContentIntegrityResult struct {
	AccountsChecked        int
	MailboxesChecked       int
	MissingAccounts        []string       // present before, not found after (even accounting for the email-address rewrite)
	MessageCountMismatches []MailboxDelta // present both before and after, but with a different message count
}

// OK reports whether every account and mailbox the pre-migration snapshot
// knew about was found afterward with an identical message count.
func (r ContentIntegrityResult) OK() bool {
	return len(r.MissingAccounts) == 0 && len(r.MessageCountMismatches) == 0
}

func (r ContentIntegrityResult) String() string {
	if r.OK() {
		return fmt.Sprintf("content integrity: %d account(s), %d mailbox(es) checked, all message counts match", r.AccountsChecked, r.MailboxesChecked)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "content integrity: %d account(s), %d mailbox(es) checked", r.AccountsChecked, r.MailboxesChecked)
	for _, a := range r.MissingAccounts {
		fmt.Fprintf(&b, "; MISSING ACCOUNT %s", a)
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

	result := &ContentIntegrityResult{}

	beforeAccounts := make([]string, 0, len(before.MailboxCounts))
	for a := range before.MailboxCounts {
		beforeAccounts = append(beforeAccounts, a)
	}
	sort.Strings(beforeAccounts)

	for _, beforeAccount := range beforeAccounts {
		result.AccountsChecked++
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

func findByLocalPart(mailboxCounts map[string][]stalwartapi.MailboxCount, beforeAccount string) ([]stalwartapi.MailboxCount, bool) {
	local := strings.SplitN(beforeAccount, "@", 2)[0]
	for afterAccount, mb := range mailboxCounts {
		if strings.SplitN(afterAccount, "@", 2)[0] == local {
			return mb, true
		}
	}
	return nil, false
}
