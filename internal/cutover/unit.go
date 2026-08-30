// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package cutover

import (
	"fmt"
	"strings"
)

// recoveryEnvVars are the two variables that must never survive into the
// live service definition. ARCHITECTURE.md §4.5 calls leaving
// STALWART_RECOVERY_MODE=1 set a documented footgun, and it is: the service
// would recovery-boot on every restart from then on, quietly, forever.
//
// This tool never puts them in a unit itself - internal/recovery runs
// recovery mode as a supervised child process, not through systemd - so
// finding them here means an operator followed the manual upgrade guide by
// hand at some point. That's exactly the case worth catching.
var recoveryEnvVars = []string{"STALWART_RECOVERY_MODE", "STALWART_RECOVERY_ADMIN"}

// RewriteUnit points a systemd unit's ExecStart at a new binary (and, if
// configPath is non-empty, a new --config path) and strips any recovery-mode
// environment lines, returning the rewritten file.
//
// It rewrites in place rather than generating a unit from a template: the
// operator's unit is theirs, and it may carry hardening options, resource
// limits, dependencies and overrides this tool has no business having an
// opinion about. Replacing it with something generated would silently drop
// all of that.
//
// It refuses rather than guesses in two cases: a unit with no ExecStart at
// all, and an Environment line that mixes a recovery variable with other
// variables. Both mean the file isn't shaped the way this rewrite assumes,
// and editing it anyway risks producing a unit that starts something other
// than what the operator intended.
func RewriteUnit(unit, binaryPath, configPath string) (string, error) {
	lines := strings.Split(unit, "\n")
	out := make([]string, 0, len(lines))
	execStarts := 0

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "ExecStart=") {
			rewritten, err := rewriteExecStart(line, binaryPath, configPath)
			if err != nil {
				return "", err
			}
			execStarts++
			out = append(out, rewritten)
			continue
		}

		if strings.HasPrefix(trimmed, "Environment=") || strings.HasPrefix(trimmed, "Environment ") {
			mentions, only := classifyEnvironmentLine(trimmed)
			if mentions && !only {
				return "", fmt.Errorf(
					"cutover: the unit's %q line sets a recovery-mode variable alongside others, and this tool won't edit a line it "+
						"only partly understands - remove the STALWART_RECOVERY_* assignment by hand and re-run", trimmed)
			}
			if mentions {
				continue // the whole line is recovery-only: drop it
			}
		}

		out = append(out, line)
	}

	if execStarts == 0 {
		return "", fmt.Errorf("cutover: the service definition has no ExecStart= line, so there's nothing to point at the new binary - is this the right unit file?")
	}
	return strings.Join(out, "\n"), nil
}

// rewriteExecStart replaces the executable in an ExecStart line, preserving
// every argument after it (and any leading whitespace or systemd prefix
// characters like "-" or "@"), then updates --config if asked to.
func rewriteExecStart(line, binaryPath, configPath string) (string, error) {
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	value := strings.TrimSpace(line)[len("ExecStart="):]

	// systemd allows prefix characters on the executable ("-", "@", ":",
	// "+", "!"). Preserve whatever is there rather than dropping semantics
	// the operator chose deliberately.
	prefix := ""
	for len(value) > 0 && strings.ContainsRune("-@:+!", rune(value[0])) {
		prefix += string(value[0])
		value = value[1:]
	}

	fields := strings.Fields(value)
	if len(fields) == 0 {
		return "", fmt.Errorf("cutover: the unit's ExecStart= line names no executable")
	}
	fields[0] = binaryPath

	if configPath != "" {
		replaced := false
		for i := 0; i < len(fields); i++ {
			switch {
			// Both spellings are real, and getting this wrong is not
			// cosmetic: a production unit uses the equals form, and only
			// matching the separated one appended a second --config, so
			// the service started with two and used the wrong one - the
			// v0.15 config, which v0.16 cannot read as a store descriptor.
			case strings.HasPrefix(fields[i], "--config=") || strings.HasPrefix(fields[i], "-c="):
				fields[i] = "--config=" + configPath
				replaced = true
			case (fields[i] == "--config" || fields[i] == "-c") && i+1 < len(fields):
				fields[i+1] = configPath
				replaced = true
				i++
			}
		}
		if !replaced {
			fields = append(fields, "--config", configPath)
		}
	}
	return indent + "ExecStart=" + prefix + strings.Join(fields, " "), nil
}

// classifyEnvironmentLine reports whether an Environment= line mentions a
// recovery variable at all, and whether that's all it sets.
func classifyEnvironmentLine(trimmed string) (mentions, only bool) {
	value := trimmed[strings.Index(trimmed, "=")+1:]
	assignments := strings.Fields(value)
	if len(assignments) == 0 {
		return false, false
	}
	recoveryCount := 0
	for _, a := range assignments {
		a = strings.Trim(a, `"'`)
		for _, name := range recoveryEnvVars {
			if strings.HasPrefix(a, name+"=") {
				recoveryCount++
			}
		}
	}
	return recoveryCount > 0, recoveryCount == len(assignments)
}
