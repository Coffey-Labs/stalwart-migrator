package cutover

import (
	"strings"
	"testing"
)

const realisticUnit = `[Unit]
Description=Stalwart Mail Server
After=network.target

[Service]
Type=simple
User=stalwart
ExecStart=/usr/local/bin/stalwart --config /etc/stalwart/config.toml
Restart=on-failure
LimitNOFILE=65536
ProtectSystem=strict
ReadWritePaths=/var/lib/stalwart

[Install]
WantedBy=multi-user.target
`

func TestRewriteUnitRepointsExecStart(t *testing.T) {
	got, err := RewriteUnit(realisticUnit, "/usr/local/bin/stalwart", "/etc/stalwart/config.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ExecStart=/usr/local/bin/stalwart --config /etc/stalwart/config.json") {
		t.Errorf("ExecStart not repointed:\n%s", got)
	}
}

// The operator's unit is theirs: hardening options, limits and paths this
// tool has no opinion about must survive untouched.
func TestRewriteUnitPreservesEverythingElse(t *testing.T) {
	got, err := RewriteUnit(realisticUnit, "/opt/stalwart/bin/stalwart", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Description=Stalwart Mail Server", "User=stalwart", "Restart=on-failure",
		"LimitNOFILE=65536", "ProtectSystem=strict", "ReadWritePaths=/var/lib/stalwart",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewrite dropped %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "ExecStart=/opt/stalwart/bin/stalwart --config /etc/stalwart/config.toml") {
		t.Errorf("existing --config should be preserved when no new one is given:\n%s", got)
	}
}

func TestRewriteUnitAddsConfigWhenTheUnitHasNone(t *testing.T) {
	got, err := RewriteUnit("[Service]\nExecStart=/usr/local/bin/stalwart\n", "/usr/local/bin/stalwart", "/etc/stalwart/config.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ExecStart=/usr/local/bin/stalwart --config /etc/stalwart/config.json") {
		t.Errorf("--config not added:\n%s", got)
	}
}

func TestRewriteUnitKeepsSystemdExecPrefixes(t *testing.T) {
	got, err := RewriteUnit("[Service]\nExecStart=-@/old/stalwart --config /c\n", "/new/stalwart", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ExecStart=-@/new/stalwart --config /c") {
		t.Errorf("systemd exec prefix characters were dropped, changing what the unit means:\n%s", got)
	}
}

// Leaving STALWART_RECOVERY_MODE=1 in the unit is the documented footgun
// from §4.5: the service would recovery-boot on every restart, forever.
func TestRewriteUnitStripsRecoveryEnvironmentLines(t *testing.T) {
	unit := `[Service]
Environment=STALWART_RECOVERY_MODE=1
Environment="STALWART_RECOVERY_ADMIN=admin:hunter2"
Environment=RUST_LOG=info
ExecStart=/usr/local/bin/stalwart
`
	got, err := RewriteUnit(unit, "/usr/local/bin/stalwart", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"STALWART_RECOVERY_MODE", "STALWART_RECOVERY_ADMIN"} {
		if strings.Contains(got, gone) {
			t.Errorf("%s survived the rewrite - the service would recovery-boot on every restart:\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "Environment=RUST_LOG=info") {
		t.Errorf("unrelated Environment line was dropped:\n%s", got)
	}
}

// A line this tool only partly understands is one it must not edit.
func TestRewriteUnitRefusesAMixedEnvironmentLine(t *testing.T) {
	unit := "[Service]\nEnvironment=RUST_LOG=info STALWART_RECOVERY_MODE=1\nExecStart=/usr/local/bin/stalwart\n"
	_, err := RewriteUnit(unit, "/usr/local/bin/stalwart", "")
	if err == nil {
		t.Fatal("want refusal for an Environment line mixing recovery and other variables, got nil")
	}
	if !strings.Contains(err.Error(), "by hand") {
		t.Errorf("error %q should tell the operator what to do about it", err)
	}
}

func TestRewriteUnitRefusesAUnitWithNoExecStart(t *testing.T) {
	_, err := RewriteUnit("[Unit]\nDescription=Something else entirely\n", "/usr/local/bin/stalwart", "")
	if err == nil {
		t.Fatal("want refusal for a unit with no ExecStart, got nil")
	}
	if !strings.Contains(err.Error(), "right unit file") {
		t.Errorf("error %q should question whether this is the right file", err)
	}
}

func TestRewriteUnitHandlesMultipleExecStartLines(t *testing.T) {
	unit := "[Service]\nExecStart=\nExecStart=/old/stalwart --config /c\n"
	got, err := RewriteUnit(unit, "/new/stalwart", "")
	if err == nil {
		t.Fatalf("an empty ExecStart= names no executable and should be refused, got:\n%s", got)
	}
}
