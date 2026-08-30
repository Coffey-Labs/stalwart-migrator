// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/stalwartapi"
)

// runTenants prints who owns what, read-only.
//
// preflight already refuses a layout v0.16 cannot represent, but it reports
// the problem, not the shape of the instance around it. Deciding how to
// resolve one — give a tenant its own domains, or collapse everything into
// one — needs the whole picture: which domains have an owner, which accounts
// reach across to a domain owned by somebody else, and which of those the
// converter would have repaired by itself.
func runTenants(args []string) error {
	fs := flag.NewFlagSet("tenants", flag.ExitOnError)
	adminURL := fs.String("admin-url", "", "base URL for the instance's admin API (required)")
	adminUser := fs.String("admin-user", "", "admin username")
	adminPassword := fs.String("admin-password", os.Getenv("STALWART_MIGRATE_ADMIN_PASSWORD"),
		"admin password (or set STALWART_MIGRATE_ADMIN_PASSWORD)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *adminURL == "" {
		return fmt.Errorf("--admin-url is required")
	}

	client := &stalwartapi.Client{
		BaseURL: *adminURL, Username: *adminUser, Password: *adminPassword, HTTPClient: &http.Client{},
	}
	layout, err := client.FetchTenantLayout(context.Background())
	if err != nil {
		return fmt.Errorf("read the tenant layout: %w", err)
	}

	if len(layout.Tenants) == 0 {
		fmt.Println("single-tenant: this instance declares no tenant principals, so no account can mismatch its domain.")
		return nil
	}

	tenants := append([]string(nil), layout.Tenants...)
	sort.Strings(tenants)
	fmt.Printf("tenants (%d): %v\n", len(tenants), tenants)

	domains := make([]string, 0, len(layout.DomainTenant))
	for d := range layout.DomainTenant {
		domains = append(domains, d)
	}
	// Domains that exist only inside an address have no entry of their own.
	seen := map[string]bool{}
	for _, p := range layout.Principals {
		for _, d := range p.Domains {
			if _, declared := layout.DomainTenant[d]; !declared && !seen[d] {
				seen[d] = true
				domains = append(domains, d)
			}
		}
	}
	sort.Strings(domains)

	fmt.Println("\ndomains:")
	for _, d := range domains {
		owner, declared := layout.DomainTenant[d]
		switch {
		case !declared:
			fmt.Printf("  %-28s (not declared as a domain principal - the converter infers it, with no tenant)\n", d)
		case owner == "":
			fmt.Printf("  %-28s no tenant\n", d)
		default:
			fmt.Printf("  %-28s tenant %s\n", d, owner)
		}
	}

	fmt.Println("\naccounts:")
	principals := append([]stalwartapi.PrincipalTenancy(nil), layout.Principals...)
	sort.Slice(principals, func(i, j int) bool { return principals[i].Name < principals[j].Name })
	for _, p := range principals {
		tenant := p.Tenant
		if tenant == "" {
			tenant = "(global)"
		}
		fmt.Printf("  %-32s %-10s tenant %-14s uses %v\n", p.Name, p.Type, tenant, p.Domains)
		// The rule v0.16 enforces, spelled out per account.
		for _, d := range p.Domains {
			owner, declared := layout.DomainTenant[d]
			if p.Tenant != "" && declared && owner != "" && owner != p.Tenant {
				fmt.Printf("      ^ %s is owned by tenant %s - v0.16 rejects this\n", d, owner)
			}
		}
	}

	plan := layout.Analyze()
	fmt.Println("\nwhat happens on migration:")
	if len(plan.Adoptions) == 0 && len(plan.Problems) == 0 {
		fmt.Println("  nothing to repair - this layout converts as it stands.")
		return nil
	}
	for _, a := range plan.Adoptions {
		fmt.Printf("  REPAIRED  %s has no tenant of its own and only one tenant's accounts use it; it adopts that tenant\n", a)
	}
	for _, p := range plan.Problems {
		fmt.Printf("  BLOCKED   %s: %s\n", p.Domain, p.Detail)
	}
	if len(plan.Problems) > 0 {
		fmt.Println("\nResolve a BLOCKED domain in v0.15 before migrating: give the tenant its own")
		fmt.Println("domains, or move the accounts involved into the tenant that owns the domain.")
		fmt.Println("Collapsing every account and domain into a single tenant also resolves it, as")
		fmt.Println("does removing the tenant principals entirely.")
	}
	return nil
}
