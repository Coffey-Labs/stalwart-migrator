// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package applyplan

import (
	"fmt"
	"sort"
	"strings"
)

// ListenerGenerator maps v0.15's server.listener.* settings onto v0.16
// x:NetworkListener objects.
//
// This is the first generator built, and deliberately so: server.listener
// is not among the settings migrate_v016.py carries over, so a migrated
// instance binds nothing and answers on no port until these exist. Every
// other unmigrated setting degrades the server; this one stops it being a
// server at all.
//
// The v0.16 shape below was not taken from the published schema reference,
// which gives `bind` as a JSON array. That form is rejected by the actual
// 0.16.14 binary ("Invalid value for object property. Properties: bind").
// The encoding here - a value-keyed set, {"[::]:25": true} - was confirmed
// by applying a plan to a real recovery-mode 0.16.14 instance and reading
// the result back with `stalwart-cli snapshot`:
//
//	{"@type":"upsert","object":"NetworkListener","matchOn":["name"],
//	 "value":{"...":{"name":"t2","bind":{"[::]:2526":true},
//	                 "protocol":"smtp","tlsImplicit":false, ...}}}
type ListenerGenerator struct{}

func (ListenerGenerator) Prefix() string { return "server.listener." }

// protocolMap translates v0.15 protocol values to v0.16's enum. Only
// manageSieve actually differs - it is camelCase in v0.16 and lowercase in
// v0.15 - but going through an explicit table means an unrecognized value
// is reported rather than passed through to be rejected at apply time.
var protocolMap = map[string]string{
	"smtp":        "smtp",
	"lmtp":        "lmtp",
	"http":        "http",
	"imap":        "imap",
	"pop3":        "pop3",
	"managesieve": "manageSieve",
}

func (g ListenerGenerator) Generate(settings map[string]string) ([]Operation, []string, []string, error) {
	type listener struct {
		binds       []string
		protocol    string
		tlsImplicit *bool
		keys        []string
	}
	byName := map[string]*listener{}

	get := func(name string) *listener {
		if byName[name] == nil {
			byName[name] = &listener{}
		}
		return byName[name]
	}

	var warnings []string
	for key, value := range settings {
		rest := strings.TrimPrefix(key, g.Prefix())
		name, field, ok := strings.Cut(rest, ".")
		if !ok || name == "" {
			continue
		}
		l := get(name)
		switch {
		case field == "bind" || strings.HasPrefix(field, "bind."):
			// v0.15 allows either a single bind or numbered bind.0000
			// entries; v0.16 holds them all in one set.
			if value != "" {
				l.binds = append(l.binds, value)
				l.keys = append(l.keys, key)
			}
		case field == "protocol":
			l.protocol = strings.ToLower(strings.TrimSpace(value))
			l.keys = append(l.keys, key)
		case field == "tls.implicit":
			implicit := strings.EqualFold(strings.TrimSpace(value), "true")
			l.tlsImplicit = &implicit
			l.keys = append(l.keys, key)
		default:
			// Deliberately not covered: anything else under a listener
			// (socket tuning, per-listener TLS overrides) is left to the
			// operator rather than guessed at, and stays counted as
			// unhandled so the coverage report tells the truth.
		}
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	var ops []Operation
	var covered []string
	for _, name := range names {
		l := byName[name]
		if len(l.binds) == 0 {
			warnings = append(warnings, fmt.Sprintf("listener %q has no bind address in the source settings - skipped", name))
			continue
		}
		protocol, known := protocolMap[l.protocol]
		if l.protocol == "" {
			// v0.16's own default; recorded as a warning because inferring
			// a listener's protocol is not something to do silently.
			protocol = "smtp"
			warnings = append(warnings, fmt.Sprintf("listener %q declared no protocol - defaulting to smtp, which may be wrong", name))
		} else if !known {
			warnings = append(warnings, fmt.Sprintf("listener %q uses protocol %q, which has no known v0.16 equivalent - skipped", name, l.protocol))
			continue
		}

		bind := map[string]any{}
		sort.Strings(l.binds)
		for _, addr := range l.binds {
			bind[addr] = true
		}
		value := map[string]any{
			"name":     name,
			"bind":     bind,
			"protocol": protocol,
		}
		if l.tlsImplicit != nil {
			value["tlsImplicit"] = *l.tlsImplicit
		}

		ops = append(ops, Operation{
			Type:    "upsert",
			Object:  "NetworkListener",
			MatchOn: []string{"name"},
			Value:   map[string]map[string]any{"listener-" + name: value},
		})
		covered = append(covered, l.keys...)
	}
	sort.Strings(covered)
	return ops, covered, warnings, nil
}

// DefaultGenerators is the set Build runs. It is short on purpose: each
// entry is a mapping confirmed against a real v0.16 binary, and an
// unverified guess in here would produce a plan that fails at apply time or,
// worse, silently configures the wrong thing.
func DefaultGenerators() []Generator {
	return []Generator{ListenerGenerator{}}
}
