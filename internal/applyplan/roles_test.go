// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package applyplan

import (
	"strings"
	"testing"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/backup"
)

// The exact principal shape a real 0.15.5 reports.
var migratedPrincipals = []backup.Principal{
	{ID: 4, Type: "individual", Name: "alice", Emails: []string{"alice@smoke.test"}, Roles: []string{"user"}},
	{ID: 5, Type: "individual", Name: "bob", Emails: []string{"bob@smoke.test"}, Roles: []string{"user"}},
	{ID: 6, Type: "individual", Name: "sysadmin", Emails: []string{"sysadmin@smoke.test"}, Roles: []string{"admin"}},
	{ID: 1, Type: "domain", Name: "smoke.test"},
}

// The failure this exists to prevent: after a real migration the admin
// account authenticated fine and was refused every management call, because
// migrate_v016.py had given it the User role like everyone else.
func TestAccountRolesRestoresTheAdministrator(t *testing.T) {
	ops, covered, warnings, err := AccountRoleOperations(migratedPrincipals)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(ops) != 1 {
		t.Fatalf("generated %d op(s), want 1 - only the admin's role actually changes", len(ops))
	}
	v := ops[0].Value["role-sysadmin"]
	if v == nil {
		t.Fatalf("no operation for sysadmin: %+v", ops[0].Value)
	}
	// Account is multi-variant; without its own @type the upsert is
	// rejected outright by the server.
	if v["@type"] != "User" {
		t.Errorf("@type = %v, want User (the Account variant)", v["@type"])
	}
	roles, ok := v["roles"].(map[string]any)
	if !ok || roles["@type"] != "Admin" {
		t.Errorf("roles = %v, want {\"@type\": \"Admin\"}", v["roles"])
	}
	if len(covered) != 1 {
		t.Errorf("covered = %v, want one entry", covered)
	}
}

// Ordinary users already get User from the migration. Rewriting every
// account would be a far larger blast radius for no benefit.
func TestAccountRolesLeavesOrdinaryUsersAlone(t *testing.T) {
	ops, _, _, err := AccountRoleOperations([]backup.Principal{
		{Type: "individual", Name: "alice", Roles: []string{"user"}},
		{Type: "individual", Name: "bob", Roles: []string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 0 {
		t.Errorf("generated %d op(s) for plain users, want 0", len(ops))
	}
}

func TestAccountRolesSkipsNonIndividuals(t *testing.T) {
	ops, _, _, err := AccountRoleOperations([]backup.Principal{
		{Type: "domain", Name: "smoke.test", Roles: []string{"admin"}},
		{Type: "group", Name: "staff", Roles: []string{"admin"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 0 {
		t.Errorf("generated %d op(s) for non-individual principals, want 0", len(ops))
	}
}

// v0.15 carries a list, v0.16 one variant. Under-privileging an
// administrator locks them out, so admin wins - and the collapse is
// reported, not silent.
func TestAccountRolesCollapsesMultipleRolesToAdminAndSaysSo(t *testing.T) {
	ops, _, warnings, err := AccountRoleOperations([]backup.Principal{
		{Type: "individual", Name: "boss", Roles: []string{"user", "admin"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("generated %d op(s), want 1", len(ops))
	}
	roles := ops[0].Value["role-boss"]["roles"].(map[string]any)
	if roles["@type"] != "Admin" {
		t.Errorf("roles = %v, want Admin to win", roles)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "collapsed") {
		t.Errorf("warnings = %v, want the collapse reported", warnings)
	}
}

func TestAccountRolesReportsRolesItCannotMap(t *testing.T) {
	_, _, warnings, err := AccountRoleOperations([]backup.Principal{
		{Type: "individual", Name: "auditor", Roles: []string{"compliance-reviewer"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "compliance-reviewer") {
		t.Errorf("warnings = %v, want the unmappable role named", warnings)
	}
}

// Found by a dress rehearsal against a clone of production, where accounts
// are named by address. v0.16 stores an account as a local part plus a
// domain reference, and rejects a full address outright ("Invalid email
// local part"). The smoke instance used bare names and never exercised it.
func TestAccountRolesUsesTheLocalPartOfAnEmailStyleName(t *testing.T) {
	ops, _, warnings, err := AccountRoleOperations([]backup.Principal{
		{Type: "individual", Name: "john@linuxexperts.net", Emails: []string{"john@linuxexperts.net"}, Roles: []string{"admin"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("generated %d op(s), want 1", len(ops))
	}
	v := ops[0].Value["role-john"]
	if v == nil {
		t.Fatalf("operation not keyed by local part: %+v", ops[0].Value)
	}
	if v["name"] != "john" {
		t.Errorf("name = %v, want the local part \"john\" - the full address is rejected by the server", v["name"])
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
}

// Local parts are unique only within a domain. Granting Admin to the wrong
// account is worse than granting it to none.
func TestAccountRolesRefusesAnAmbiguousLocalPart(t *testing.T) {
	ops, covered, warnings, err := AccountRoleOperations([]backup.Principal{
		{Type: "individual", Name: "postmaster@one.example", Roles: []string{"admin"}},
		{Type: "individual", Name: "postmaster@two.example", Roles: []string{"user"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 0 {
		t.Errorf("generated %d op(s) for an ambiguous local part, want 0 - it could land on the wrong account", len(ops))
	}
	if len(covered) != 0 {
		t.Errorf("covered = %v, want none", covered)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "share the local part") {
		t.Errorf("warnings = %v, want one explaining the ambiguity", warnings)
	}
	if !strings.Contains(warnings[0], "by hand") {
		t.Errorf("warning %q should tell the operator what to do instead", warnings[0])
	}
}
