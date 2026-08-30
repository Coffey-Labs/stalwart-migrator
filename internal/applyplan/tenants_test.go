// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package applyplan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The four cases below are the ones checked against a real Stalwart 0.16.14
// in recovery mode, via `stalwart-cli apply`, before this code was written:
//
//	tenant account  -> tenant-less domain : invalidForeignKey  (repaired here)
//	tenant account  -> other tenant's dom : invalidForeignKey  (unrepresentable)
//	global account  -> tenant-owned domain: accepted           (left alone)
//	tenant account  -> own tenant's domain: accepted           (left alone)
func plan(ops ...PlanOp) []PlanOp { return ops }

func tenantOp(cid, name string) PlanOp {
	return PlanOp{"@type": "create", "object": "Tenant",
		"value": map[string]any{cid: map[string]any{"name": name}}}
}

func domainOp(cid, name string, tenantRef string) PlanOp {
	body := map[string]any{"name": name}
	if tenantRef != "" {
		body["memberTenantId"] = tenantRef
	}
	return PlanOp{"@type": "create", "object": "Domain",
		"value": map[string]any{cid: body}}
}

func accountOp(cid, name, domainRef, tenantRef string, aliasDomainRefs ...string) PlanOp {
	body := map[string]any{"@type": "User", "name": name, "domainId": domainRef}
	if tenantRef != "" {
		body["memberTenantId"] = tenantRef
	}
	if len(aliasDomainRefs) > 0 {
		aliases := map[string]any{}
		for i, r := range aliasDomainRefs {
			aliases[string(rune('0'+i))] = map[string]any{"name": name, "domainId": r}
		}
		body["aliases"] = aliases
	}
	return PlanOp{"@type": "create", "object": "Account",
		"value": map[string]any{cid: body}}
}

func domainTenant(t *testing.T, ops []PlanOp, cid string) string {
	t.Helper()
	for _, op := range ops {
		if op.objectName() != "Domain" {
			continue
		}
		if body, ok := op.createBodies()[cid]; ok {
			s, _ := body["memberTenantId"].(string)
			return s
		}
	}
	t.Fatalf("no domain %q in plan", cid)
	return ""
}

func TestReconcileAdoptsTenantForInferredDomain(t *testing.T) {
	ops := plan(
		tenantOp("t0", "acme"),
		domainOp("d0", "acme-corp.test", "#t0"),
		domainOp("d1", "inferred.test", ""), // never declared in v0.15
		accountOp("a0", "bob", "#d0", "#t0", "#d1"),
	)
	res := ReconcileDomainTenants(ops)
	if !res.OK() {
		t.Fatalf("unexpected conflicts: %s", res.String())
	}
	if got := domainTenant(t, ops, "d1"); got != "#t0" {
		t.Errorf("inferred domain tenant = %q, want %q", got, "#t0")
	}
	if len(res.Adoptions) != 1 || res.Adoptions[0].Domain != "inferred.test" {
		t.Errorf("adoptions = %+v, want one for inferred.test", res.Adoptions)
	}
	if res.Adoptions[0].Tenant != "acme" {
		t.Errorf("adoption tenant = %q, want the tenant name %q", res.Adoptions[0].Tenant, "acme")
	}
}

func TestReconcileLeavesGlobalAccountOnTenantDomainAlone(t *testing.T) {
	// Confirmed accepted by 0.16.14: a global account may live on a
	// tenant-owned domain. Nothing should be invented for it.
	ops := plan(
		tenantOp("t0", "acme"),
		domainOp("d0", "acme-corp.test", "#t0"),
		accountOp("a0", "admin", "#d0", ""),
	)
	res := ReconcileDomainTenants(ops)
	if !res.OK() || len(res.Adoptions) != 0 {
		t.Fatalf("expected no changes, got %s", res.String())
	}
}

func TestReconcileLeavesTenantlessDomainWithOnlyGlobalUsers(t *testing.T) {
	ops := plan(
		tenantOp("t0", "acme"),
		domainOp("d0", "global.test", ""),
		accountOp("a0", "carol", "#d0", ""),
	)
	res := ReconcileDomainTenants(ops)
	if got := domainTenant(t, ops, "d0"); got != "" {
		t.Errorf("global domain gained tenant %q; it has no tenant-scoped users", got)
	}
	if len(res.Adoptions) != 0 {
		t.Errorf("adoptions = %+v, want none", res.Adoptions)
	}
}

func TestReconcileReportsTwoTenantsSharingADomain(t *testing.T) {
	ops := plan(
		tenantOp("t0", "alpha"),
		tenantOp("t1", "beta"),
		domainOp("d0", "shared.test", ""),
		accountOp("a0", "u1", "#d0", "#t0"),
		accountOp("a1", "u2", "#d0", "#t1"),
	)
	res := ReconcileDomainTenants(ops)
	if res.OK() {
		t.Fatal("expected a conflict for a domain shared by two tenants")
	}
	if got := domainTenant(t, ops, "d0"); got != "" {
		t.Errorf("conflicting domain was modified to %q; it must be left untouched", got)
	}
	c := res.Conflicts[0]
	if c.Domain != "shared.test" || len(c.Tenants) != 2 {
		t.Errorf("conflict = %+v, want shared.test across two tenants", c)
	}
	for _, want := range []string{"alpha", "beta"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("conflict detail %q does not name tenant %q", c.Detail, want)
		}
	}
}

func TestReconcileReportsAccountOnAnotherTenantsDomain(t *testing.T) {
	ops := plan(
		tenantOp("t0", "alpha"),
		tenantOp("t1", "beta"),
		domainOp("d0", "owned.test", "#t0"),
		accountOp("a0", "u2", "#d0", "#t1"),
	)
	res := ReconcileDomainTenants(ops)
	if res.OK() {
		t.Fatal("expected a conflict when an account's tenant differs from its domain's")
	}
	if got := domainTenant(t, ops, "d0"); got != "#t0" {
		t.Errorf("domain ownership changed to %q; declared ownership must not be overwritten", got)
	}
}

func TestReconcileIsNoOpWithoutTenants(t *testing.T) {
	ops := plan(
		domainOp("d0", "example.test", ""),
		accountOp("a0", "alice", "#d0", ""),
	)
	res := ReconcileDomainTenants(ops)
	if !res.OK() || len(res.Adoptions) != 0 {
		t.Fatalf("single-tenant plan should be untouched, got %s", res.String())
	}
	if res.String() != "no domain/tenant mismatches" {
		t.Errorf("summary = %q", res.String())
	}
}

func TestReconcileFileRoundTripsAndRewritesOnlyWhenChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "export.json")

	ops := plan(
		tenantOp("t0", "acme"),
		domainOp("d0", "acme-corp.test", "#t0"),
		domainOp("d1", "inferred.test", ""),
		accountOp("a0", "bob", "#d0", "#t0", "#d1"),
	)
	if err := WritePlan(path, ops); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := ReconcileDomainTenantsFile(path)
	if err != nil {
		t.Fatalf("reconcile file: %v", err)
	}
	if len(res.Adoptions) != 1 {
		t.Fatalf("adoptions = %+v", res.Adoptions)
	}

	reread, err := ReadPlan(path)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if got := domainTenant(t, reread, "d1"); got != "#t0" {
		t.Errorf("rewritten plan has tenant %q, want %q", got, "#t0")
	}

	// Second pass must be a no-op: the plan is already consistent.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := ReconcileDomainTenantsFile(path)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(res2.Adoptions) != 0 {
		t.Errorf("second pass adopted %+v, want none", res2.Adoptions)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("second pass rewrote an already-consistent plan")
	}
}

func TestReconcileFileRefusesConflictingPlan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "export.json")
	ops := plan(
		tenantOp("t0", "alpha"),
		tenantOp("t1", "beta"),
		domainOp("d0", "shared.test", ""),
		accountOp("a0", "u1", "#d0", "#t0"),
		accountOp("a1", "u2", "#d0", "#t1"),
	)
	if err := WritePlan(path, ops); err != nil {
		t.Fatalf("write: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileDomainTenantsFile(path); err == nil {
		t.Fatal("expected an error for an unrepresentable plan")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("a plan that could not be repaired was rewritten anyway")
	}
}

func TestReadPlanHandlesLongLines(t *testing.T) {
	// Plans embed certificates and DKIM private keys inline, which blow
	// past bufio.Scanner's default 64KiB line limit.
	dir := t.TempDir()
	path := filepath.Join(dir, "big.json")
	huge := strings.Repeat("x", 200<<10)
	ops := plan(PlanOp{"@type": "update", "object": "Certificate",
		"value": map[string]any{"c0": map[string]any{"cert": huge}}})
	if err := WritePlan(path, ops); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadPlan(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 || got[0].createBodies()["c0"]["cert"] != huge {
		t.Error("long line did not round-trip")
	}
}

// A real export.json mixes `create` operations (client-id keyed) with
// `update` operations carrying a flat object. An earlier version of this
// code modelled every operation the first way and failed to parse a real
// plan at the first `update` line - found by running it against actual
// migrate_v016.py output rather than hand-built fixtures.
func TestReconcileHandlesRealExportShapes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "export.json")
	raw := strings.Join([]string{
		`{"@type":"create","object":"Tenant","value":{"create-0":{"name":"acme","quotas":{}}}}`,
		`{"@type":"create","object":"Domain","value":{"create-1":{"name":"acme-corp.test","memberTenantId":"#create-0"},"create-2":{"name":"inferred.test"}}}`,
		`{"@type":"create","object":"Account","value":{"restore-5":{"@type":"User","name":"bob","domainId":"#create-1","memberTenantId":"#create-0","aliases":{"0":{"name":"bob","domainId":"#create-2"}},"quotas":{"maxDiskQuota":10737418240}}}}`,
		`{"@type":"update","object":"SystemSettings","value":{"defaultDomainId":"#create-1","defaultHostname":"mail.acme-corp.test"}}`,
		`{"@type":"update","object":"BlobStore","value":{"@type":"Default"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(raw), 0o640); err != nil {
		t.Fatal(err)
	}

	res, err := ReconcileDomainTenantsFile(path)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(res.Adoptions) != 1 || res.Adoptions[0].Domain != "inferred.test" {
		t.Fatalf("adoptions = %+v", res.Adoptions)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A large quota must survive as an integer: decoding through float64
	// would rewrite it as 1.073741824e+10 and hand the server a different
	// document than the conversion produced.
	if !strings.Contains(string(out), "10737418240") {
		t.Errorf("quota was reformatted; plan now reads:\n%s", out)
	}
	// The update operations must come back untouched.
	for _, want := range []string{`"defaultHostname":"mail.acme-corp.test"`, `"object":"BlobStore"`} {
		if !strings.Contains(strings.ReplaceAll(string(out), ", ", ","), want) {
			t.Errorf("rewritten plan lost %s:\n%s", want, out)
		}
	}

	reread, err := ReadPlan(path)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if len(reread) != 5 {
		t.Errorf("operation count = %d, want 5", len(reread))
	}
	if got := domainTenant(t, reread, "create-2"); got != "#create-0" {
		t.Errorf("inferred domain tenant = %q, want %q", got, "#create-0")
	}
}
