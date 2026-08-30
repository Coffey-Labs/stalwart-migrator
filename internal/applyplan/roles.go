// SPDX-FileCopyrightText: 2026 Coffey Labs
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
		localPart string
		domain    string
		variant   string
		source    string
	}
	var changes []change

	// v0.16 stores an account as a local part plus a domain reference, not
	// as the full address v0.15 uses for its principal name: a v0.15
	// account named "john@example.net" becomes name "john" with a domainId.
	// Passing the full address is rejected outright ("Invalid email local
	// part"), which is how this was found - on a production clone, where
	// accounts are named by address. The smoke instance used bare names and
	// never exercised it.
	localPartCount := map[string]int{}
	for _, p := range principals {
		if strings.EqualFold(p.Type, "individual") {
			localPartCount[localPart(p)]++
		}
	}

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
		local := localPart(p)
		// Local parts are unique only within a domain, so an upsert matched
		// on name alone could land on a different account that happens to
		// share it - postmaster@a.com and postmaster@b.com both become
		// "postmaster". Granting Admin to the wrong account is worse than
		// granting it to none, so an ambiguous name is refused and
		// reported rather than guessed at.
		if localPartCount[local] > 1 {
			warnings = append(warnings, fmt.Sprintf(
				"account %q: %d accounts share the local part %q, so this role cannot be restored unambiguously - grant it by hand after migrating",
				p.Name, localPartCount[local], local))
			continue
		}
		changes = append(changes, change{localPart: local, domain: domainOf(p), variant: variant, source: p.Name})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].localPart < changes[j].localPart })

	for _, c := range changes {
		value := map[string]any{
			// Account is a multi-variant object: without its own @type the
			// upsert is rejected outright.
			"@type": "User",
			"name":  c.localPart,
			"roles": map[string]any{"@type": c.variant},
		}
		ops = append(ops, Operation{
			Type:    "upsert",
			Object:  "Account",
			MatchOn: []string{"name"},
			Value:   map[string]map[string]any{"role-" + c.localPart: value},
		})
		covered = append(covered, "principal:"+c.source+":roles")
	}
	return ops, covered, warnings, nil
}

// localPart is the account name v0.16 will hold: the part before "@" when
// the v0.15 principal is named by address, otherwise the name as-is.
func localPart(p backup.Principal) string {
	name := p.Name
	if name == "" && len(p.Emails) > 0 {
		name = p.Emails[0]
	}
	if at := strings.Index(name, "@"); at > 0 {
		return name[:at]
	}
	return name
}

// domainOf reports the domain an account belongs to, for messages.
func domainOf(p backup.Principal) string {
	name := p.Name
	if at := strings.Index(name, "@"); at > 0 && at+1 < len(name) {
		return name[at+1:]
	}
	if len(p.Emails) > 0 {
		if at := strings.Index(p.Emails[0], "@"); at > 0 && at+1 < len(p.Emails[0]) {
			return p.Emails[0][at+1:]
		}
	}
	return ""
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
