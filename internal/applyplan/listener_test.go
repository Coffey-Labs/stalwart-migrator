// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package applyplan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// productionListeners is the exact server.listener.* key set a real
// Stalwart 0.15.5 reports, taken verbatim from a live instance.
var productionListeners = map[string]string{
	"server.listener.http.bind":                "[::]:8090",
	"server.listener.http.protocol":            "http",
	"server.listener.https.bind":               "[::]:443",
	"server.listener.https.protocol":           "http",
	"server.listener.https.tls.implicit":       "true",
	"server.listener.imap.bind":                "[::]:143",
	"server.listener.imap.protocol":            "imap",
	"server.listener.imaptls.bind":             "[::]:993",
	"server.listener.imaptls.protocol":         "imap",
	"server.listener.imaptls.tls.implicit":     "true",
	"server.listener.sieve.bind":               "[::]:4190",
	"server.listener.sieve.protocol":           "managesieve",
	"server.listener.smtp.bind":                "[::]:25",
	"server.listener.smtp.protocol":            "smtp",
	"server.listener.submissions.bind":         "[::]:465",
	"server.listener.submissions.protocol":     "smtp",
	"server.listener.submissions.tls.implicit": "true",
}

func generate(t *testing.T, settings map[string]string) ([]Operation, []string, []string) {
	t.Helper()
	ops, covered, warnings, err := ListenerGenerator{}.Generate(settings)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return ops, covered, warnings
}

func findListener(ops []Operation, name string) map[string]any {
	for _, op := range ops {
		for _, v := range op.Value {
			if v["name"] == name {
				return v
			}
		}
	}
	return nil
}

func TestListenerGeneratorMapsAProductionListenerSet(t *testing.T) {
	ops, covered, warnings := generate(t, productionListeners)

	if len(ops) != 7 {
		t.Errorf("generated %d listener(s), want 7", len(ops))
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings for a well-formed listener set: %v", warnings)
	}
	if len(covered) != len(productionListeners) {
		t.Errorf("covered %d keys, want all %d - uncovered keys must not be silently dropped",
			len(covered), len(productionListeners))
	}

	smtp := findListener(ops, "smtp")
	if smtp == nil {
		t.Fatal("no smtp listener generated")
	}
	// The encoding the real 0.16.14 binary accepts: a value-keyed set, not
	// the array the published docs show.
	bind, ok := smtp["bind"].(map[string]any)
	if !ok {
		t.Fatalf("bind is %T, want a value-keyed set - an array is rejected by the server", smtp["bind"])
	}
	if v, present := bind["[::]:25"]; !present || v != true {
		t.Errorf("bind = %v, want {\"[::]:25\": true}", bind)
	}
}

// managesieve -> manageSieve is the one protocol whose spelling changes,
// and getting it wrong fails at apply time rather than at generation.
func TestListenerGeneratorRenamesManageSieve(t *testing.T) {
	ops, _, _ := generate(t, productionListeners)
	sieve := findListener(ops, "sieve")
	if sieve == nil {
		t.Fatal("no sieve listener generated")
	}
	if sieve["protocol"] != "manageSieve" {
		t.Errorf("protocol = %v, want manageSieve (v0.16 spelling)", sieve["protocol"])
	}
}

func TestListenerGeneratorCarriesImplicitTLS(t *testing.T) {
	ops, _, _ := generate(t, productionListeners)
	for name, want := range map[string]bool{"imaptls": true, "submissions": true, "https": true} {
		l := findListener(ops, name)
		if l == nil {
			t.Fatalf("no %s listener generated", name)
		}
		if l["tlsImplicit"] != want {
			t.Errorf("%s tlsImplicit = %v, want %v", name, l["tlsImplicit"], want)
		}
	}
	// A listener that never mentioned TLS shouldn't have an opinion forced
	// onto it; v0.16's own default applies.
	if smtp := findListener(ops, "smtp"); smtp != nil {
		if _, present := smtp["tlsImplicit"]; present {
			t.Error("smtp listener should not assert tlsImplicit when the source didn't set it")
		}
	}
}

// Upsert keyed on name, so re-running a plan updates rather than colliding.
// An operator will run this more than once.
func TestListenerGeneratorEmitsIdempotentUpserts(t *testing.T) {
	ops, _, _ := generate(t, productionListeners)
	for _, op := range ops {
		if op.Type != "upsert" {
			t.Errorf("@type = %q, want upsert so the plan can be re-run", op.Type)
		}
		if len(op.MatchOn) != 1 || op.MatchOn[0] != "name" {
			t.Errorf("matchOn = %v, want [name]", op.MatchOn)
		}
	}
}

func TestListenerGeneratorRefusesUnknownProtocol(t *testing.T) {
	ops, covered, warnings := generate(t, map[string]string{
		"server.listener.weird.bind":     "[::]:9999",
		"server.listener.weird.protocol": "gopher",
	})
	if len(ops) != 0 {
		t.Errorf("generated %d op(s) for an unmappable protocol, want 0 - guessing here fails at apply time", len(ops))
	}
	if len(covered) != 0 {
		t.Errorf("covered = %v, want none: an unmapped listener must stay counted as unhandled", covered)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "gopher") {
		t.Errorf("warnings = %v, want one naming the unknown protocol", warnings)
	}
}

func TestListenerGeneratorWarnsOnMissingBindAndProtocol(t *testing.T) {
	_, _, warnings := generate(t, map[string]string{"server.listener.orphan.protocol": "smtp"})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "no bind address") {
		t.Errorf("warnings = %v, want one about the missing bind address", warnings)
	}

	ops, _, warnings := generate(t, map[string]string{"server.listener.mystery.bind": "[::]:26"})
	if len(ops) != 1 {
		t.Fatalf("generated %d op(s), want 1 with a defaulted protocol", len(ops))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "may be wrong") {
		t.Errorf("warnings = %v, want one flagging the defaulted protocol", warnings)
	}
}

func TestBuildReportsHonestCoverage(t *testing.T) {
	settings := map[string]string{}
	unmigrated := map[string]bool{}
	for k, v := range productionListeners {
		settings[k] = v
		unmigrated[k] = true
	}
	// Things no generator handles yet, in the proportions a real instance
	// shows: the plan must not imply it covered them.
	for i := 0; i < 500; i++ {
		k := "spam-filter.rule.r" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		settings[k] = "x"
		unmigrated[k] = true
	}

	plan, coverage, err := Build(settings, unmigrated, DefaultGenerators())
	if err != nil {
		t.Fatal(err)
	}
	if coverage.CoveredKeys != len(productionListeners) {
		t.Errorf("CoveredKeys = %d, want %d", coverage.CoveredKeys, len(productionListeners))
	}
	if coverage.TotalKeys != len(unmigrated) {
		t.Errorf("TotalKeys = %d, want %d", coverage.TotalKeys, len(unmigrated))
	}
	if len(plan.Operations) == 0 {
		t.Fatal("no operations generated")
	}
	summary := coverage.Summary(5)
	if !strings.Contains(summary, "still unhandled") || !strings.Contains(summary, "spam-filter.rule") {
		t.Errorf("summary must name what it did NOT cover:\n%s", summary)
	}
	if strings.Contains(summary, "100.0%") {
		t.Errorf("summary claims full coverage but most settings are unhandled:\n%s", summary)
	}
}

// Build must never generate for a key migrate_v016.py already handles, or
// the plan would fight the official conversion.
func TestBuildIgnoresSettingsTheOfficialScriptAlreadyMigrates(t *testing.T) {
	settings := map[string]string{}
	for k, v := range productionListeners {
		settings[k] = v
	}
	plan, coverage, err := Build(settings, map[string]bool{}, DefaultGenerators())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Operations) != 0 {
		t.Errorf("generated %d op(s) for settings not listed as unmigrated, want 0", len(plan.Operations))
	}
	if coverage.CoveredKeys != 0 {
		t.Errorf("CoveredKeys = %d, want 0", coverage.CoveredKeys)
	}
}

func TestWriteNDJSONIsOnePlanEntryPerLine(t *testing.T) {
	ops, _, _ := generate(t, productionListeners)
	path := filepath.Join(t.TempDir(), "plan.json")
	plan := &Plan{Operations: ops}
	if err := plan.WriteNDJSON(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != len(ops) {
		t.Fatalf("wrote %d line(s) for %d operation(s)", len(lines), len(ops))
	}
	for i, line := range lines {
		var op Operation
		if err := json.Unmarshal([]byte(line), &op); err != nil {
			t.Errorf("line %d is not valid JSON: %v", i+1, err)
		}
		if op.Object != "NetworkListener" {
			t.Errorf("line %d object = %q", i+1, op.Object)
		}
	}
}
