// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package preflight

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/stalwartapi"
)

// Options configures a Checker. Every field has a conservative default
// applied by New except the ones that must name a real path on this host.
type Options struct {
	BinaryPath    string // installed stalwart binary, e.g. /usr/local/bin/stalwart
	ConfigPath    string // its config file (TOML pre-0.16, JSON 0.16+)
	DataDir       string // data directory to size/space-check
	ContainerName string // docker container name, if applicable
	AdminURL      string // base URL for the JMAP reachability check; empty skips it
	AdminUser     string
	AdminPassword string
	TargetVersion string // e.g. "0.16.14" or "latest"
	// TargetBinaryPath, when set, is read for the target version instead of
	// asking the release API - the only way a host with no route out can
	// pass this check.
	TargetBinaryPath string
	MinFreeMultiple  float64
	// CLIPath and PythonPath are the external programs the migration
	// shells out to. Checked before anything is touched - see
	// CheckExternalTools.
	CLIPath    string
	PythonPath string
	// ToolCheckAdvisory downgrades the external-tool checks from blocking
	// to advisory. `rehearse` sets it: that phase never invokes
	// stalwart-cli, and refusing to run the read-only reconnaissance that
	// tells an operator what they need - because they don't yet have it -
	// is backwards. `run` leaves it false, because there the tools are
	// about to be used and a missing one means stopping a mail server to
	// find out.
	ToolCheckAdvisory bool
	// DeploymentCheckAdvisory downgrades the deployment-kind check from
	// blocking to advisory, for the same reason as ToolCheckAdvisory.
	// `rehearse` sets it: it never stops the service or cuts over, so a
	// deployment this tool cannot cut over is still worth rehearsing
	// against - the reconnaissance is exactly what tells an operator what
	// the manual path involves. `run` leaves it false, because there the
	// alternative is finding out after mail is already down.
	DeploymentCheckAdvisory bool
	HTTPClient              *http.Client
}

// Checker runs the preflight checks described in ARCHITECTURE.md §4.1.
type Checker struct {
	opts Options
}

func New(opts Options) *Checker {
	if opts.MinFreeMultiple <= 0 {
		opts.MinFreeMultiple = 2.0
	}
	return &Checker{opts: opts}
}

// Run executes every preflight check, checkpointing each one so a killed
// and re-invoked run skips checks that already completed - see
// checkpoint.Store.RunStep. It never aborts early on a single Fail: the
// point of preflight is to surface every blocking issue in one pass rather
// than fail-stop-fix-retry one at a time. Callers decide what to do with a
// Report whose Blocking() is true. It only returns a non-nil error for a
// genuine execution fault (e.g. the checkpoint store itself can't be
// written to) - a check finding a real problem is reported via
// Status: StatusFail in the Report, not a Go error.
func (c *Checker) Run(ctx context.Context, store *checkpoint.Store, rs *checkpoint.RunState) (Report, error) {
	var report Report

	// runCheck wraps fn as a checkpointed step and appends its result to
	// report, whether fn actually ran or was skipped because a prior
	// attempt already completed it - either way report ends up with the
	// same entries, and the returned checkpoint.StepOutcome.Extra carries
	// whatever machine-readable value a later check in this same Run needs.
	runCheck := func(name string, fn func() (CheckResult, string)) (checkpoint.StepOutcome, error) {
		outcome, err := store.RunStep(rs, checkpoint.PhasePreflight, name, func() (checkpoint.StepOutcome, error) {
			res, extra := fn()
			return checkpoint.StepOutcome{Verdict: string(res.Status), Detail: res.Detail, Extra: extra}, nil
		})
		if err != nil {
			return checkpoint.StepOutcome{}, err
		}
		report.Results = append(report.Results, CheckResult{Name: name, Status: Status(outcome.Verdict), Detail: outcome.Detail})
		return outcome, nil
	}

	// Detected once, here, because the very first check needs it: what is
	// running is a property of a container's image, not of any binary on
	// this host, and a container-only host has no binary to ask. The
	// deployment-kind check below reports it; this only decides who to put
	// the question to.
	kind := DetectDeploymentKind(ctx, c.opts.ContainerName)

	versionOutcome, err := runCheck("version", func() (CheckResult, string) {
		var (
			cur    string
			err    error
			source string
		)
		if kind == DeploymentDocker {
			cur, err = DetectContainerVersion(ctx, c.opts.ContainerName)
			source = ", read from the image behind container " + containerNameOr(c.opts.ContainerName)
		} else {
			cur, err = DetectVersion(ctx, c.opts.BinaryPath)
		}
		if err != nil {
			return CheckResult{Status: StatusFail, Detail: err.Error()}, ""
		}
		curV, _ := parseSemver(cur)
		if curV.Compare(minSupportedSource) < 0 {
			return CheckResult{
				Status: StatusFail,
				Detail: fmt.Sprintf("current version %s is older than the minimum supported %s - upgrade to 0.15.x first", cur, minSupportedSource),
			}, cur
		}
		return CheckResult{Status: StatusOK, Detail: fmt.Sprintf("current version %s%s", cur, source)}, cur
	})
	if err != nil {
		return report, err
	}

	targetOutcome, err := runCheck("target-release", func() (CheckResult, string) {
		// A binary already on disk answers the question the release API was
		// being asked - which version are we upgrading to - without needing
		// a route to the internet. A host that has none cannot reach the
		// API at all, and failing here would stop it migrating even though
		// everything it needs is present.
		if c.opts.TargetBinaryPath != "" {
			got, err := DetectVersion(ctx, c.opts.TargetBinaryPath)
			if err != nil {
				return CheckResult{Status: StatusFail, Detail: fmt.Sprintf("couldn't read the version of %s: %v", c.opts.TargetBinaryPath, err)}, ""
			}
			want := strings.TrimPrefix(c.opts.TargetVersion, "v")
			if want != "" && want != "latest" && want != got {
				return CheckResult{Status: StatusFail, Detail: fmt.Sprintf(
					"%s reports version %s, but this run targets %s - migrating to a version nobody planned for",
					c.opts.TargetBinaryPath, got, want)}, ""
			}
			return CheckResult{Status: StatusOK, Detail: fmt.Sprintf("target is %s, taken from %s (no release lookup needed)", got, c.opts.TargetBinaryPath)}, got
		}
		rel, err := ResolveRelease(ctx, c.opts.HTTPClient, c.opts.TargetVersion)
		if err != nil {
			return CheckResult{Status: StatusFail, Detail: err.Error()}, ""
		}
		tag := strings.TrimPrefix(rel.TagName, "v")
		detail := fmt.Sprintf("resolved target to %s (%d release assets)", rel.TagName, len(rel.Assets))
		if asset := ChecksumAsset(rel); asset != nil {
			detail += fmt.Sprintf(", checksum manifest available: %s", asset.Name)
		} else {
			detail += "; no published checksum manifest found - integrity relies on the one-time HTTPS download only"
		}
		return CheckResult{Status: StatusOK, Detail: detail}, tag
	})
	if err != nil {
		return report, err
	}

	boundaryOutcome, err := runCheck("upgrade-direction", func() (CheckResult, string) {
		curV, errCur := parseSemver(versionOutcome.Extra)
		tgtV, errTgt := parseSemver(targetOutcome.Extra)
		if errCur != nil || errTgt != nil {
			return CheckResult{Status: StatusWarn, Detail: "could not compare current and target versions (one or both unresolved above)"}, ""
		}
		if curV.Compare(tgtV) >= 0 {
			return CheckResult{
				Status: StatusFail,
				Detail: fmt.Sprintf("current version %s is already at or beyond target %s - nothing to migrate", curV, tgtV),
			}, ""
		}
		if curV.Major == 0 && curV.Minor < 16 && (tgtV.Major > 0 || tgtV.Minor >= 16) {
			return CheckResult{
				Status: StatusOK,
				Detail: fmt.Sprintf("%s -> %s crosses the 0.15/0.16 major boundary: full recovery-mode migration plan required (ARCHITECTURE.md §4.4)", curV, tgtV),
			}, "crosses"
		}
		return CheckResult{
			Status: StatusOK,
			Detail: fmt.Sprintf("%s -> %s is a same-boundary patch upgrade: fast-path plan applies (ARCHITECTURE.md §4.6)", curV, tgtV),
		}, "patch"
	})
	if err != nil {
		return report, err
	}
	crossesBoundary := boundaryOutcome.Extra != "patch"

	// Before anything else that matters: are the tools this migration
	// depends on actually here? Discovering a missing stalwart-cli after
	// the service has been stopped is what this exists to prevent.
	for _, res := range CheckExternalTools(ctx, c.opts.CLIPath, c.opts.PythonPath, crossesBoundary) {
		result := res
		if c.opts.ToolCheckAdvisory && result.Status == StatusFail {
			result.Status = StatusWarn
			result.Detail = "(advisory for a rehearsal; this would block `run`) " + result.Detail
		}
		if _, err := runCheck(result.Name, func() (CheckResult, string) { return result, "" }); err != nil {
			return report, err
		}
	}

	deploymentOutcome, err := runCheck("deployment-kind", func() (CheckResult, string) {
		// Detected once above, since the version check already needed it.
		// Asking twice could report two different answers for one run.
		// Docker has to fail here rather than later. Cutover refuses this
		// deployment - recreating a container from a new image is not
		// swapping a binary and rewriting a unit, and this tool does not
		// automate it - but cutover runs after the service has been
		// stopped. Refusing there means refusing with mail already down,
		// which is how a migration attempt turned into an outage.
		status := StatusOK
		detail := fmt.Sprintf("detected deployment kind: %s", kind)
		switch kind {
		case DeploymentUnknown:
			status = StatusWarn
		case DeploymentDocker:
			// No longer a refusal on its own: a container can be migrated
			// now. What still refuses is specific and checked below -
			// compose, and data that is not on a volume - because those are
			// properties of this container rather than of containers.
			detail += " - the container checks below decide whether this one can be migrated"
		}
		return CheckResult{Status: status, Detail: detail}, string(kind)
	})
	if err != nil {
		return report, err
	}

	// The container checks below only mean anything for a container, and
	// asking docker about one that isn't there would fail for the wrong
	// reason.
	if DeploymentKind(deploymentOutcome.Extra) == DeploymentDocker {
		if err := c.runContainerChecks(ctx, runCheck); err != nil {
			return report, err
		}
	}

	storeOutcome, err := runCheck("store-backend", func() (CheckResult, string) {
		matches, err := DetectStoreBackends(c.opts.ConfigPath)
		if err != nil {
			return CheckResult{Status: StatusFail, Detail: err.Error()}, ""
		}
		if len(matches) == 0 {
			return CheckResult{Status: StatusWarn, Detail: "no known store backend type found in config - confirm manually before proceeding"}, ""
		}
		names := make([]string, len(matches))
		backends := make([]string, len(matches))
		for i, m := range matches {
			names[i] = fmt.Sprintf("%s (%s)", m.Backend, m.Path)
			backends[i] = m.Backend
		}
		return CheckResult{Status: StatusOK, Detail: "found: " + strings.Join(names, ", ")}, strings.Join(backends, ",")
	})
	if err != nil {
		return report, err
	}

	if _, err := runCheck("cluster-config", func() (CheckResult, string) {
		mentions, err := ClusterMentions(c.opts.ConfigPath)
		if err != nil {
			return CheckResult{Status: StatusFail, Detail: err.Error()}, ""
		}
		if len(mentions) > 0 {
			shown := mentions
			if len(shown) > 5 {
				shown = shown[:5]
			}
			detail := fmt.Sprintf("config mentions clustering at %s", strings.Join(shown, ", "))
			if len(mentions) > len(shown) {
				detail += fmt.Sprintf(" (and %d more)", len(mentions)-len(shown))
			}
			detail += " - confirm every peer node is stopped before this run proceeds; the tool does not verify this for you"
			return CheckResult{Status: StatusWarn, Detail: detail}, ""
		}
		return CheckResult{Status: StatusOK, Detail: "no cluster configuration detected"}, ""
	}); err != nil {
		return report, err
	}

	if _, err := runCheck("disk-space", func() (CheckResult, string) {
		size, err := DirSize(c.opts.DataDir)
		if err != nil {
			return CheckResult{Status: StatusFail, Detail: err.Error()}, ""
		}
		free, err := FreeBytes(c.opts.DataDir)
		if err != nil {
			return CheckResult{Status: StatusFail, Detail: err.Error()}, ""
		}
		required := uint64(float64(size) * c.opts.MinFreeMultiple)
		detail := fmt.Sprintf("data dir %s is %s, %s free, need >= %s (%.1fx, for the fs-snapshot backup)",
			c.opts.DataDir, humanBytes(uint64(size)), humanBytes(free), humanBytes(required), c.opts.MinFreeMultiple)
		if free < required {
			return CheckResult{Status: StatusFail, Detail: detail}, ""
		}
		return CheckResult{Status: StatusOK, Detail: detail}, ""
	}); err != nil {
		return report, err
	}

	if c.opts.AdminURL != "" {
		if _, err := runCheck("admin-reachable", func() (CheckResult, string) {
			client := &stalwartapi.Client{
				BaseURL:    c.opts.AdminURL,
				Username:   c.opts.AdminUser,
				Password:   c.opts.AdminPassword,
				HTTPClient: c.opts.HTTPClient,
			}
			if err := client.Ping(ctx); err != nil {
				return CheckResult{Status: StatusFail, Detail: err.Error()}, ""
			}
			return CheckResult{Status: StatusOK, Detail: fmt.Sprintf("JMAP session reachable at %s with the given credentials", c.opts.AdminURL)}, ""
		}); err != nil {
			return report, err
		}

		if _, err := runCheck("account-snapshot", func() (CheckResult, string) {
			client := &stalwartapi.Client{
				BaseURL:    c.opts.AdminURL,
				Username:   c.opts.AdminUser,
				Password:   c.opts.AdminPassword,
				HTTPClient: c.opts.HTTPClient,
			}
			snap, err := client.AccountSnapshot(ctx)
			if err != nil {
				return CheckResult{
					Status: StatusWarn,
					Detail: fmt.Sprintf("could not capture the account/domain snapshot: %v - the post-migration directory-integrity check won't have anything to compare against", err),
				}, ""
			}
			mailboxCounts := make(map[string][]checkpoint.MailboxCount, len(snap.MailboxCounts))
			for account, counts := range snap.MailboxCounts {
				converted := make([]checkpoint.MailboxCount, len(counts))
				for i, mc := range counts {
					converted[i] = checkpoint.MailboxCount{Mailbox: mc.Mailbox, Messages: mc.Messages}
				}
				mailboxCounts[account] = converted
			}
			rs.PreflightSnapshot = &checkpoint.PreflightSnapshot{
				TakenAt:       time.Now().UTC(),
				AccountCount:  snap.AccountCount,
				Domains:       snap.Domains,
				MailboxCounts: mailboxCounts,
				UsedQuota:     snap.UsedQuota,
			}
			detail := fmt.Sprintf("captured snapshot: %d account(s) across %d domain(s), used-quota for %d account(s), mailbox counts for %d account(s)",
				snap.AccountCount, len(snap.Domains), len(snap.UsedQuota), len(mailboxCounts))
			if len(mailboxCounts) == 0 && len(snap.UsedQuota) > 0 {
				// Expected against a 0.15.x source: it exposes no
				// per-mailbox counts, so used-quota is what the
				// post-migration comparison will have to work from.
				detail += " (this source exposes no per-mailbox counts, so the post-migration check compares accounts, domains and used-quota)"
			}
			status := StatusOK
			if len(snap.MailboxErrors) > 0 {
				status = StatusWarn
				accounts := make([]string, 0, len(snap.MailboxErrors))
				for account := range snap.MailboxErrors {
					accounts = append(accounts, account)
				}
				sort.Strings(accounts)
				for i, account := range accounts {
					if i >= 3 {
						detail += fmt.Sprintf(" (and %d more)", len(accounts)-3)
						break
					}
					detail += fmt.Sprintf("; mailbox count failed for %s: %s", account, snap.MailboxErrors[account])
				}
			}
			return CheckResult{Status: status, Detail: detail}, ""
		}); err != nil {
			return report, err
		}

		if _, err := runCheck("admin-account-kind", func() (CheckResult, string) {
			return c.adminAccountKind(rs), ""
		}); err != nil {
			return report, err
		}
	} else {
		report.Results = append(report.Results, CheckResult{
			Name:   "admin-reachable",
			Status: StatusWarn,
			Detail: "no --admin-url configured - skipped; the account/mailbox snapshot validate needs later can't be captured without it",
		})
	}

	// Multi-tenancy gate. migrate_v016.py carries the Tenant and the
	// Domains but leaves every Account's tenantId null, so the apply fails
	// with invalidForeignKey - and it fails during recovery-mode migration,
	// which is after the service has been stopped. That is exactly the
	// shape of failure preflight exists to move earlier.
	if c.opts.AdminURL != "" && crossesBoundary {
		if _, err := runCheck("multi-tenancy", func() (CheckResult, string) {
			client := &stalwartapi.Client{
				BaseURL: c.opts.AdminURL, Username: c.opts.AdminUser,
				Password: c.opts.AdminPassword, HTTPClient: c.opts.HTTPClient,
			}
			layout, err := client.FetchTenantLayout(ctx)
			if err != nil {
				return CheckResult{
					Status: StatusWarn,
					Detail: fmt.Sprintf("couldn't map this instance's tenants: %v - if it is multi-tenant, "+
						"a domain/tenant mismatch would only surface during the conversion", err),
				}, ""
			}
			if len(layout.Tenants) == 0 {
				return CheckResult{Status: StatusOK, Detail: "single-tenant: no tenant principals, so no account can mismatch its domain"}, ""
			}

			plan := layout.Analyze()
			if len(plan.Problems) > 0 {
				details := make([]string, 0, len(plan.Problems))
				for _, p := range plan.Problems {
					details = append(details, fmt.Sprintf("%s: %s", p.Domain, p.Detail))
				}
				return CheckResult{
					Status: StatusFail,
					Detail: fmt.Sprintf("this instance has %d tenant(s) (%s) in an arrangement v0.16 cannot represent - %s. "+
						"Resolve this in v0.15 first: give each tenant its own domains, or move the accounts into one tenant",
						len(layout.Tenants), strings.Join(layout.Tenants, ", "), strings.Join(details, "; ")),
				}, ""
			}
			if len(plan.Adoptions) > 0 {
				return CheckResult{
					Status: StatusWarn,
					Detail: fmt.Sprintf("this instance has %d tenant(s) (%s); %d domain(s) (%s) have no tenant of their own but are "+
						"used only by accounts of a single tenant. v0.16 requires them to match, so the conversion will assign each "+
						"domain to that tenant - the accounts migrate intact, but those domains become tenant-owned",
						len(layout.Tenants), strings.Join(layout.Tenants, ", "),
						len(plan.Adoptions), strings.Join(plan.Adoptions, ", ")),
				}, ""
			}
			return CheckResult{
				Status: StatusOK,
				Detail: fmt.Sprintf("%d tenant(s) (%s), and every account already sits on a domain of its own tenant",
					len(layout.Tenants), strings.Join(layout.Tenants, ", ")),
			}, ""
		}); err != nil {
			return report, err
		}
	}

	rs.Topology = checkpoint.Topology{
		DeploymentKind: deploymentOutcome.Extra,
		StoreBackend:   storeOutcome.Extra,
	}
	if versionOutcome.Extra != "" {
		rs.SourceVersion = versionOutcome.Extra
	}
	if targetOutcome.Extra != "" {
		rs.TargetVersion = targetOutcome.Extra
	}
	if err := store.Save(rs); err != nil {
		return report, fmt.Errorf("preflight: persist topology: %w", err)
	}

	return report, nil
}

// adminAccountKind reports whether the account preflight authenticated as
// still exists after the migration.
func (c *Checker) adminAccountKind(rs *checkpoint.RunState) CheckResult {
	if rs.PreflightSnapshot == nil {
		return CheckResult{Status: StatusWarn, Detail: "no snapshot was captured, so the admin account could not be looked up in the directory"}
	}
	// v0.16 keeps its configuration in the store, so the
	// [authentication.fallback-admin] block a v0.15 config can
	// define simply ceases to exist. An operator who authenticates
	// as that admin gets through every check here - it works fine
	// today - and then finds it rejected the moment the migration
	// completes, taking the quota rebuild and the post-migration
	// content comparison with it. Cheaper to say so now.
	for account := range rs.PreflightSnapshot.UsedQuota {
		if strings.EqualFold(account, c.opts.AdminUser) {
			return CheckResult{Status: StatusOK, Detail: fmt.Sprintf("%s is an account in the directory, so it survives the migration", c.opts.AdminUser)}
		}
		if local, _, ok := strings.Cut(account, "@"); ok && strings.EqualFold(local, c.opts.AdminUser) {
			return CheckResult{Status: StatusOK, Detail: fmt.Sprintf("%s matches directory account %s, which survives the migration", c.opts.AdminUser, account)}
		}
	}
	return CheckResult{Status: StatusFail, Detail: fmt.Sprintf(
		"%s authenticates now but is not an account in this directory - it is a config fallback-admin, and v0.16 keeps its "+
			"config in the store, so the block defining it does not survive. The migration itself would succeed, then the quota "+
			"rebuild and the post-migration content check would both be refused with 401. Re-run as an account that exists in "+
			"the directory (see the README's \"You need a named admin account\")", c.opts.AdminUser)}
}
