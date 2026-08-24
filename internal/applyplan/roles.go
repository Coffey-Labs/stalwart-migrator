// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package applyplan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/backup"
)

// AccountRoleOperations restores the account roles a v0.16 migration drops.
//
// migrate_v016.py assigns every migrated account the User role regardless
// of what it had before. Verified on a real migration: an account holding
// the admin role in v0.15 came out the other side with
// `roles: {"@type": "User"}`, authenticated fine, and was refused every
// management call with "forbidden". The instance had no working
// administrator, which is a bad thing to discover after cutting over.
//
// Ordinary users are unaffected - User is what they had and what they get -
// so this deliberately emits operations only for accounts whose role
// actually changes. A plan that rewrote every account would be a much
// larger blast radius for no benefit.
//
// The v0.16 shape comes from the server's own schema document
// (GET /api/schema), where x:UserRoles is a multi-variant type with
// variants User, Admin and Custom, and from confirming the upsert against
// a live 0.16.14: Account is itself multi-variant, so the entry needs its
// own "@type" as well.
func AccountRoleOperations(principals []backup.Principal) (ops []Operation, covered []string, warnings []string, err error) {
	type change struct {
		name    string
		variant string
	}
	var changes []change

	for _, p := range principals {
		if !strings.EqualFold(p.Type, "individual") {
			continue // domains and groups don't carry these roles
		}
		variant, note := roleVariant(p.Roles)
		if note != "" {
			warnings = append(warnings, fmt.Sprintf("account %q: %s", p.Name, note))
		}
		if variant == "" || variant == "User" {
			// User is the migration's own default; re-asserting it would
			// touch every account to no effect.
			continue
		}
		changes = append(changes, change{name: p.Name, variant: variant})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].name < changes[j].name })

	for _, c := range changes {
		ops = append(ops, Operation{
			Type:    "upsert",
			Object:  "Account",
			MatchOn: []string{"name"},
			Value: map[string]map[string]any{
				"role-" + c.name: {
					// Account is a multi-variant object: without its own
					// @type the upsert is rejected outright.
					"@type": "User",
					"name":  c.name,
					"roles": map[string]any{"@type": c.variant},
				},
			},
		})
		covered = append(covered, "principal:"+c.name+":roles")
	}
	return ops, covered, warnings, nil
}

// roleVariant maps a v0.15 role list onto v0.16's x:UserRoles variant.
//
// v0.15 carries a list of role names; v0.16 carries one variant. Where an
// account had several, admin wins - under-privileging an administrator
// locks them out, which is the failure this function exists to prevent -
// and the collapse is reported rather than done silently.
func roleVariant(roles []string) (variant, note string) {
	if len(roles) == 0 {
		return "", ""
	}
	hasAdmin, hasUser := false, false
	var unknown []string
	for _, r := range roles {
		switch strings.ToLower(strings.TrimSpace(r)) {
		case "admin", "administrator":
			hasAdmin = true
		case "user":
			hasUser = true
		default:
			unknown = append(unknown, r)
		}
	}
	if len(unknown) > 0 {
		note = fmt.Sprintf("role(s) %s have no known v0.16 equivalent and are not restored - recreate them by hand",
			strings.Join(unknown, ", "))
	}
	switch {
	case hasAdmin && (hasUser || len(unknown) > 0):
		return "Admin", note + " (collapsed several roles to Admin)"
	case hasAdmin:
		return "Admin", note
	case hasUser:
		return "User", note
	default:
		return "", note
	}
}
