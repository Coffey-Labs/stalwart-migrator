// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package stalwartapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFlexStringAcceptsEveryShapeAV015InstanceReturns(t *testing.T) {
	cases := map[string]string{
		`"acme"`:              "acme",
		`{"string":"acme"}`:   "acme",
		`{"name":"acme"}`:     "acme",
		`["acme","other"]`:    "acme",
		`null`:                "",
		`{}`:                  "",
		`[]`:                  "",
		`{"other":"ignored"}`: "",
	}
	for raw, want := range cases {
		if got := flexString(json.RawMessage(raw)); got != want {
			t.Errorf("flexString(%s) = %q, want %q", raw, got, want)
		}
	}
	if got := flexString(nil); got != "" {
		t.Errorf("flexString(nil) = %q", got)
	}
}

func TestDomainsOfCollectsNameAndAliasDomains(t *testing.T) {
	got := domainsOf("bob@example.com", []string{"bob@example.com", "bob@Alias.NET", "malformed", "trailing@"})
	want := []string{"alias.net", "example.com"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("domainsOf = %v, want %v", got, want)
	}
}

func TestAnalyzeIsEmptyForSingleTenant(t *testing.T) {
	l := &TenantLayout{DomainTenant: map[string]string{}}
	plan := l.Analyze()
	if len(plan.Adoptions) != 0 || len(plan.Problems) != 0 {
		t.Errorf("single-tenant layout produced %+v", plan)
	}
}

func TestAnalyzeFlagsUndeclaredDomainForAdoption(t *testing.T) {
	// The production failure: a tenant account with an address on a domain
	// that was never declared, so the conversion gives it no tenant.
	l := &TenantLayout{
		Tenants:      []string{"acme"},
		DomainTenant: map[string]string{"acme-corp.test": "acme"},
		Principals: []PrincipalTenancy{
			{Name: "bob", Tenant: "acme", Domains: []string{"acme-corp.test", "inferred.test"}},
		},
	}
	plan := l.Analyze()
	if len(plan.Problems) != 0 {
		t.Fatalf("unexpected problems: %+v", plan.Problems)
	}
	if len(plan.Adoptions) != 1 || plan.Adoptions[0] != "inferred.test" {
		t.Errorf("adoptions = %v, want [inferred.test]", plan.Adoptions)
	}
}

func TestAnalyzeIgnoresGlobalAccountsOnTenantDomains(t *testing.T) {
	// Verified against 0.16.14: this direction applies cleanly, so it must
	// not be reported as anything.
	l := &TenantLayout{
		Tenants:      []string{"acme"},
		DomainTenant: map[string]string{"acme-corp.test": "acme"},
		Principals: []PrincipalTenancy{
			{Name: "admin", Tenant: "", Domains: []string{"acme-corp.test"}},
		},
	}
	plan := l.Analyze()
	if len(plan.Adoptions) != 0 || len(plan.Problems) != 0 {
		t.Errorf("global account on a tenant domain produced %+v", plan)
	}
}

func TestAnalyzeReportsDomainSharedByTwoTenants(t *testing.T) {
	l := &TenantLayout{
		Tenants:      []string{"alpha", "beta"},
		DomainTenant: map[string]string{},
		Principals: []PrincipalTenancy{
			{Name: "u1", Tenant: "alpha", Domains: []string{"shared.test"}},
			{Name: "u2", Tenant: "beta", Domains: []string{"shared.test"}},
		},
	}
	plan := l.Analyze()
	if len(plan.Problems) != 1 {
		t.Fatalf("problems = %+v, want one", plan.Problems)
	}
	if len(plan.Adoptions) != 0 {
		t.Errorf("a conflicting domain was also queued for adoption: %v", plan.Adoptions)
	}
	for _, want := range []string{"alpha", "beta", "u1", "u2"} {
		if !strings.Contains(plan.Problems[0].Detail, want) {
			t.Errorf("detail %q omits %q", plan.Problems[0].Detail, want)
		}
	}
}

func TestAnalyzeReportsAccountOnAnotherTenantsDomain(t *testing.T) {
	l := &TenantLayout{
		Tenants:      []string{"alpha", "beta"},
		DomainTenant: map[string]string{"owned.test": "alpha"},
		Principals: []PrincipalTenancy{
			{Name: "u2", Tenant: "beta", Domains: []string{"owned.test"}},
		},
	}
	plan := l.Analyze()
	if len(plan.Problems) != 1 || plan.Problems[0].Domain != "owned.test" {
		t.Fatalf("problems = %+v", plan.Problems)
	}
}

func TestAnalyzeAcceptsAConsistentMultiTenantLayout(t *testing.T) {
	l := &TenantLayout{
		Tenants: []string{"alpha", "beta"},
		DomainTenant: map[string]string{
			"alpha.test": "alpha",
			"beta.test":  "beta",
		},
		Principals: []PrincipalTenancy{
			{Name: "u1", Tenant: "alpha", Domains: []string{"alpha.test"}},
			{Name: "u2", Tenant: "beta", Domains: []string{"beta.test"}},
			{Name: "admin", Tenant: "", Domains: []string{"alpha.test"}},
		},
	}
	plan := l.Analyze()
	if len(plan.Adoptions) != 0 || len(plan.Problems) != 0 {
		t.Errorf("a already-consistent multi-tenant layout produced %+v", plan)
	}
}
