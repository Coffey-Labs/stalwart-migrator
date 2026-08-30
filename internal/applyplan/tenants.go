// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package applyplan

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// This file repairs one specific defect in Stalwart's own migrate_v016.py,
// which this tool downloads rather than vendors and therefore cannot fix at
// the source.
//
// In v0.15 a domain's tenant and a principal's tenant were independent
// facts. A tenant-scoped user could sit on a domain owned by no tenant at
// all - which is the normal outcome whenever the domain was never declared
// as its own `domain` principal, because migrate_v016.py then infers the
// domain from an email address, and inferred domains carry no tenant.
//
// v0.16 rejects that arrangement. A tenant-scoped Account may only
// reference a Domain owned by the same tenant, as a primary domain or as an
// alias, and the server answers:
//
//	invalidForeignKey |   Object id:   Domain#d
//
// on the Domain reference otherwise. This was confirmed against a real
// 0.16.14 in recovery mode, not inferred: the reverse direction (a global,
// tenant-less account on a tenant-owned domain) applies without complaint,
// and only the tenant-scoped-account-to-tenant-less-domain direction fails.
//
// That error cost a production migration a full restore, because it
// surfaced during apply - after the old service was already stopped, and
// after the store had been irreversibly upgraded to schema v6. Hence both
// halves of the fix: repair the plan here, and refuse the genuinely
// unrepresentable cases in preflight, before anything is stopped.
//
// The same fix has been prepared for migrate_v016.py upstream; this stays
// until a released migrate_v016.py carries it, and is written to be a no-op
// against a plan that is already consistent.

// TenantAdoption records one domain that took on a tenant it did not
// previously declare, so the operator is told rather than silently given a
// different ownership model than they had.
type TenantAdoption struct {
	Domain  string // domain name, e.g. "example.com"
	Tenant  string // tenant name, or the raw reference if the name is unknown
	Because string // the principal whose membership forced it
}

// TenantConflict is a domain whose users disagree about which tenant owns
// it. v0.16 cannot represent this, so it is reported rather than repaired.
type TenantConflict struct {
	Domain  string
	Detail  string
	Tenants []string
}

// TenantReconcileResult is what a reconciliation changed and what it could
// not change.
type TenantReconcileResult struct {
	Adoptions []TenantAdoption
	Conflicts []TenantConflict
}

// OK reports whether the plan is now internally consistent.
func (r TenantReconcileResult) OK() bool { return len(r.Conflicts) == 0 }

// String renders a short operator-facing summary.
func (r TenantReconcileResult) String() string {
	if len(r.Adoptions) == 0 && len(r.Conflicts) == 0 {
		return "no domain/tenant mismatches"
	}
	var b strings.Builder
	for _, a := range r.Adoptions {
		fmt.Fprintf(&b, "domain %s adopted tenant %s (required by %s)\n", a.Domain, a.Tenant, a.Because)
	}
	for _, c := range r.Conflicts {
		fmt.Fprintf(&b, "domain %s: %s\n", c.Domain, c.Detail)
	}
	return strings.TrimRight(b.String(), "\n")
}

// PlanOp is one line of an apply plan, kept in the generic form it was
// read in.
//
// It is deliberately not the typed Operation above. A real export.json
// mixes shapes: `create` operations map a client-id to an object, while
// `update` operations - SystemSettings, BlobStore, SearchStore - carry a
// flat object with no client-id layer at all. Parsing a whole plan into the
// typed form fails on the first `update` line, and a reconciliation that
// cannot read the plan it is meant to repair is worse than none.
type PlanOp map[string]any

// ReadPlan parses an apply plan - migrate_v016.py's export.json, or one
// this tool generated.
//
// Numbers are decoded as json.Number so they survive the round-trip
// unchanged. Decoding into float64 would rewrite a quota of 10737418240 as
// 1.073741824e+10, which is a different document than the one the
// conversion produced.
func ReadPlan(path string) ([]PlanOp, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("applyplan: open %s: %w", path, err)
	}
	defer f.Close()

	var ops []PlanOp
	sc := bufio.NewScanner(f)
	// Plans carry certificates and DKIM keys inline, so lines run well past
	// bufio.Scanner's default 64KiB limit.
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for line := 1; sc.Scan(); line++ {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		var op PlanOp
		if err := dec.Decode(&op); err != nil {
			return nil, fmt.Errorf("applyplan: %s line %d: %w", path, line, err)
		}
		ops = append(ops, op)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("applyplan: read %s: %w", path, err)
	}
	return ops, nil
}

// WritePlan writes plan operations atomically, so a failure part-way
// through can never leave a truncated plan that would apply half a
// migration.
func WritePlan(path string, ops []PlanOp) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("applyplan: create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	enc := json.NewEncoder(tmp)
	for _, op := range ops {
		if err := enc.Encode(op); err != nil {
			tmp.Close()
			return fmt.Errorf("applyplan: write %s: %w", tmpName, err)
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("applyplan: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("applyplan: close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return fmt.Errorf("applyplan: chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("applyplan: replace %s: %w", path, err)
	}
	return nil
}

// objectName is the operation's target type, e.g. "Domain".
func (op PlanOp) objectName() string {
	s, _ := op["object"].(string)
	return s
}

// createBodies returns the client-id-keyed objects a `create` operation
// carries. Operations whose value is a flat object - every `update` - yield
// nothing, which is what keeps them untouched.
func (op PlanOp) createBodies() map[string]map[string]any {
	value, ok := op["value"].(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]map[string]any{}
	for cid, body := range value {
		if b, ok := body.(map[string]any); ok {
			out[cid] = b
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// refCID strips the leading "#" from a plan back-reference. Values that are
// not back-references (an already-resolved server id, say) are returned
// unchanged, which is what makes comparing two references safe.
func refCID(v any) string {
	s, ok := v.(string)
	if !ok || s == "" {
		return ""
	}
	return strings.TrimPrefix(s, "#")
}

// domainRefs returns every domain reference an account or mailing list
// makes: its primary domain, plus one per alias. Aliases matter as much as
// the primary - a tenant-scoped account with an alias on a tenant-less
// domain is rejected exactly like one whose primary domain mismatches.
func domainRefs(body map[string]any) []string {
	var refs []string
	if c := refCID(body["domainId"]); c != "" {
		refs = append(refs, c)
	}
	aliases, ok := body["aliases"].(map[string]any)
	if !ok {
		return refs
	}
	for _, a := range aliases {
		alias, ok := a.(map[string]any)
		if !ok {
			continue
		}
		if c := refCID(alias["domainId"]); c != "" {
			refs = append(refs, c)
		}
	}
	return refs
}

func stringField(body map[string]any, key string) string {
	s, _ := body[key].(string)
	return s
}

// ReconcileDomainTenants makes every domain's tenant agree with the
// accounts and mailing lists that use it, editing ops in place.
//
// Where a domain has no tenant but every tenant-scoped principal using it
// agrees on one, the domain adopts that tenant: it is the only assignment
// that lets the plan apply while keeping every account, and it is safe in
// the other direction because a global account on a tenant-owned domain is
// accepted.
//
// Where the principals disagree, nothing is changed and the disagreement is
// reported. Forcing such a plan through would mean dropping accounts, and
// dropping accounts silently is how mailboxes get lost.
func ReconcileDomainTenants(ops []PlanOp) TenantReconcileResult {
	var result TenantReconcileResult

	domains := map[string]map[string]any{} // cid -> domain body
	tenantNames := map[string]string{}     // cid -> tenant name
	for _, op := range ops {
		bodies := op.createBodies()
		switch op.objectName() {
		case "Domain":
			for cid, body := range bodies {
				domains[cid] = body
			}
		case "Tenant":
			for cid, body := range bodies {
				tenantNames[cid] = stringField(body, "name")
			}
		}
	}
	if len(domains) == 0 || len(tenantNames) == 0 {
		return result // single-tenant install: nothing can mismatch
	}

	tenantName := func(ref string) string {
		if n := tenantNames[refCID(ref)]; n != "" {
			return n
		}
		return refCID(ref)
	}

	// domain cid -> tenant ref -> an example principal requiring it.
	required := map[string]map[string]string{}
	for _, op := range ops {
		object := op.objectName()
		if object != "Account" && object != "MailingList" {
			continue
		}
		for _, body := range op.createBodies() {
			tRef, _ := body["memberTenantId"].(string)
			if tRef == "" {
				continue // a global principal constrains nothing
			}
			label := fmt.Sprintf("%s %q", object, stringField(body, "name"))
			for _, dCID := range domainRefs(body) {
				if required[dCID] == nil {
					required[dCID] = map[string]string{}
				}
				if _, seen := required[dCID][tRef]; !seen {
					required[dCID][tRef] = label
				}
			}
		}
	}

	for _, dCID := range sortedKeys(required) {
		wanted := required[dCID]
		dom, ok := domains[dCID]
		if !ok {
			continue // reference to something this plan does not create
		}
		dName := stringField(dom, "name")
		if dName == "" {
			dName = dCID
		}

		if len(wanted) > 1 {
			var names, parts []string
			for _, tRef := range sortedKeys(wanted) {
				names = append(names, tenantName(tRef))
				parts = append(parts, fmt.Sprintf("%s (e.g. %s)", tenantName(tRef), wanted[tRef]))
			}
			result.Conflicts = append(result.Conflicts, TenantConflict{
				Domain:  dName,
				Tenants: names,
				Detail: fmt.Sprintf("used by principals from more than one tenant: %s. "+
					"v0.16 requires every tenant-scoped account to sit on a domain owned by its own "+
					"tenant, and a domain can belong to at most one tenant", strings.Join(parts, ", ")),
			})
			continue
		}

		tRef := sortedKeys(wanted)[0]
		because := wanted[tRef]
		current, _ := dom["memberTenantId"].(string)
		if refCID(current) == refCID(tRef) {
			continue // already consistent
		}
		if current != "" {
			result.Conflicts = append(result.Conflicts, TenantConflict{
				Domain:  dName,
				Tenants: []string{tenantName(current), tenantName(tRef)},
				Detail: fmt.Sprintf("belongs to tenant %s, but %s belongs to tenant %s and uses it. "+
					"v0.16 rejects an account whose tenant differs from its domain's",
					tenantName(current), because, tenantName(tRef)),
			})
			continue
		}

		dom["memberTenantId"] = tRef
		result.Adoptions = append(result.Adoptions, TenantAdoption{
			Domain: dName, Tenant: tenantName(tRef), Because: because,
		})
	}

	return result
}

// ReconcileDomainTenantsFile reads a plan, reconciles it, and rewrites it
// only if something actually changed and nothing conflicted.
func ReconcileDomainTenantsFile(path string) (TenantReconcileResult, error) {
	ops, err := ReadPlan(path)
	if err != nil {
		return TenantReconcileResult{}, err
	}
	result := ReconcileDomainTenants(ops)
	if !result.OK() {
		return result, fmt.Errorf("applyplan: %s cannot be made consistent with v0.16's "+
			"tenant rules:\n%s", path, result.String())
	}
	if len(result.Adoptions) == 0 {
		return result, nil
	}
	if err := WritePlan(path, ops); err != nil {
		return result, err
	}
	return result, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
