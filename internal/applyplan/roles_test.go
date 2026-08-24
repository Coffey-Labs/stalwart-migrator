// SPDX-FileCopyrightText: 2026 LINUXexpert-org
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
