// SPDX-FileCopyrightText: 2026 Coffey Labs
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

// TenantLayout is who-belongs-to-which-tenant on a v0.15.x instance, in
// enough detail to predict whether the v0.16 conversion will produce a plan
// the new server accepts.
//
// v0.16 requires a tenant-scoped account to sit on a domain owned by that
// same tenant, for its primary domain and for every alias. v0.15 imposed no
// such rule, so an install can be perfectly valid today and unconvertible
// tomorrow. Establishing that here - while the server is still running and
// nothing has been stopped - is the whole point: the alternative is finding
// out during the recovery-mode apply, which is the one moment in the run
// with no way forward and no way back.
type TenantLayout struct {
	// Tenants is every tenant principal's name.
	Tenants []string
	// DomainTenant maps a declared domain name to its tenant name. Domains
	// that exist only inside an email address are absent, which mirrors the
	// converter: it infers those domains and gives them no tenant.
	DomainTenant map[string]string
	// Principals is every account, group and mailing list, with the tenant
	// it belongs to and the domains it touches.
	Principals []PrincipalTenancy
}

// PrincipalTenancy is one account's tenant and the domains it references.
type PrincipalTenancy struct {
	Name    string
	Type    string
	Tenant  string   // "" for a global principal
	Domains []string // primary plus alias domains, lowercased
}

// flexString reads a field that a v0.15 instance may return either as a
// plain string or wrapped - migrate_v016.py's own pv_string tolerates the
// same shapes, and a preflight that only understood one of them would
// silently see every account as global.
func flexString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, key := range []string{"string", "name", "id"} {
			if v, ok := obj[key]; ok {
				var inner string
				if json.Unmarshal(v, &inner) == nil && inner != "" {
					return inner
				}
			}
		}
		return ""
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err == nil && len(list) > 0 {
		return flexString(list[0])
	}
	return ""
}

// detailedPrincipal is the per-principal view, which carries the tenant the
// paginated list does not reliably include.
type detailedPrincipal struct {
	Type   json.RawMessage `json:"type"`
	Name   json.RawMessage `json:"name"`
	Tenant json.RawMessage `json:"tenant"`
	Emails []string        `json:"emails"`
}

// principalDetail fetches one principal by name. The response may or may
// not be wrapped in a "data" envelope depending on the point release, so
// both are accepted.
func (c *Client) principalDetail(ctx context.Context, name string) (detailedPrincipal, error) {
	var out detailedPrincipal
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/api/principal/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return out, err
	}
	req.SetBasicAuth(c.Username, c.Password)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return out, fmt.Errorf("stalwartapi: fetch principal %q: %w", name, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("stalwartapi: GET %s returned %s", endpoint, resp.Status)
	}
	if readErr != nil {
		return out, fmt.Errorf("stalwartapi: read principal %q: %w", name, readErr)
	}

	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	payload := body
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Data) > 0 {
		payload = envelope.Data
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return out, fmt.Errorf("stalwartapi: parse principal %q: %w", name, err)
	}
	return out, nil
}

// domainsOf returns every domain a principal touches: the domain in its
// name, if it is an address, plus one per email.
func domainsOf(name string, emails []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(addr string) {
		at := strings.LastIndex(addr, "@")
		if at < 0 || at == len(addr)-1 {
			return
		}
		d := strings.ToLower(strings.TrimSpace(addr[at+1:]))
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		out = append(out, d)
	}
	add(name)
	for _, e := range emails {
		add(e)
	}
	sort.Strings(out)
	return out
}

// FetchTenantLayout builds a TenantLayout from a v0.15.x instance.
//
// It returns an empty layout, and no error, on a single-tenant install:
// there is nothing that can mismatch, and that is the common case.
func (c *Client) FetchTenantLayout(ctx context.Context) (*TenantLayout, error) {
	tenants, err := c.TenantNames(ctx)
	if err != nil {
		return nil, err
	}
	layout := &TenantLayout{Tenants: tenants, DomainTenant: map[string]string{}}
	if len(tenants) == 0 {
		return layout, nil
	}

	domains, err := c.restPrincipals(ctx, "domain")
	if err != nil {
		return nil, err
	}
	for _, d := range domains {
		if d.Name == "" {
			continue
		}
		detail, err := c.principalDetail(ctx, d.Name)
		if err != nil {
			return nil, err
		}
		layout.DomainTenant[strings.ToLower(d.Name)] = flexString(detail.Tenant)
	}

	for _, pType := range []string{"individual", "group", "list"} {
		principals, err := c.restPrincipals(ctx, pType)
		if err != nil {
			return nil, err
		}
		for _, p := range principals {
			if p.Name == "" {
				continue
			}
			detail, err := c.principalDetail(ctx, p.Name)
			if err != nil {
				return nil, err
			}
			emails := p.Emails
			if len(detail.Emails) > 0 {
				emails = detail.Emails
			}
			layout.Principals = append(layout.Principals, PrincipalTenancy{
				Name:    p.Name,
				Type:    pType,
				Tenant:  flexString(detail.Tenant),
				Domains: domainsOf(p.Name, emails),
			})
		}
	}
	return layout, nil
}

// TenancyProblem is one domain whose users cannot all be represented in
// v0.16.
type TenancyProblem struct {
	Domain string
	Detail string
}

// TenancyPlan is what the conversion will have to do to this layout.
type TenancyPlan struct {
	// Adoptions are domains with no tenant of their own that will be given
	// one, because the only tenant-scoped accounts using them agree.
	Adoptions []string
	// Problems are the domains v0.16 cannot represent at all.
	Problems []TenancyProblem
}

// Analyze predicts whether this layout converts cleanly, applying exactly
// the rule the server enforces and the same repair applyplan performs.
func (l *TenantLayout) Analyze() TenancyPlan {
	var plan TenancyPlan
	if len(l.Tenants) == 0 {
		return plan
	}

	// domain -> tenant -> an example principal requiring it
	required := map[string]map[string]string{}
	for _, p := range l.Principals {
		if p.Tenant == "" {
			continue // a global principal constrains nothing
		}
		for _, d := range p.Domains {
			if required[d] == nil {
				required[d] = map[string]string{}
			}
			if _, ok := required[d][p.Tenant]; !ok {
				required[d][p.Tenant] = p.Name
			}
		}
	}

	domains := make([]string, 0, len(required))
	for d := range required {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	for _, d := range domains {
		wanted := required[d]
		names := make([]string, 0, len(wanted))
		for t := range wanted {
			names = append(names, t)
		}
		sort.Strings(names)

		if len(names) > 1 {
			var parts []string
			for _, t := range names {
				parts = append(parts, fmt.Sprintf("%s (e.g. %s)", t, wanted[t]))
			}
			plan.Problems = append(plan.Problems, TenancyProblem{
				Domain: d,
				Detail: fmt.Sprintf("used by accounts from more than one tenant: %s - v0.16 allows a domain "+
					"to belong to at most one tenant, and requires each tenant-scoped account to sit on its "+
					"own tenant's domain", strings.Join(parts, ", ")),
			})
			continue
		}

		want := names[0]
		switch declared, isDeclared := l.DomainTenant[d]; {
		case !isDeclared || declared == "":
			// Either inferred from an address, or declared with no tenant.
			// Either way the conversion gives it no tenant and the account
			// is rejected - this is the case that broke production.
			plan.Adoptions = append(plan.Adoptions, d)
		case declared != want:
			plan.Problems = append(plan.Problems, TenancyProblem{
				Domain: d,
				Detail: fmt.Sprintf("belongs to tenant %s, but %s (tenant %s) uses it - v0.16 rejects an "+
					"account whose tenant differs from its domain's", declared, wanted[want], want),
			})
		}
	}
	return plan
}
