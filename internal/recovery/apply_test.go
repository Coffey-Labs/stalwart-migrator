// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package recovery

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyInvokesCLIWithEnvCredentials(t *testing.T) {
	dir := t.TempDir()
	log := argsFile(t, dir)
	withFakeExecutable(t, "stalwart-cli", fmt.Sprintf("#!/bin/sh\necho \"$@ URL=$STALWART_URL USER=$STALWART_USER\" >> %q\nexit 0\n", log))

	err := Apply(context.Background(), ApplyOptions{URL: "http://127.0.0.1:8080", User: "admin", Password: "secret"}, "/tmp/export.json")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := readArgsFile(t, log)
	for _, want := range []string{"apply", "--file /tmp/export.json", "URL=http://127.0.0.1:8080", "USER=admin"} {
		if !strings.Contains(got, want) {
			t.Errorf("stalwart-cli invoked with %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "secret") {
		t.Error("password should not appear in argv")
	}
}

func TestApplyPropagatesFailure(t *testing.T) {
	withFakeExecutable(t, "stalwart-cli", "#!/bin/sh\necho 'invalid object' >&2\nexit 1\n")
	err := Apply(context.Background(), ApplyOptions{URL: "http://127.0.0.1:8080", User: "admin", Password: "x"}, "/tmp/export.json")
	if err == nil {
		t.Fatal("Apply should error when stalwart-cli exits non-zero")
	}
	if !strings.Contains(err.Error(), "invalid object") {
		t.Errorf("error = %v, want it to include stderr", err)
	}
}

func TestApplyAllStopsAtFirstFailure(t *testing.T) {
	dir := t.TempDir()
	log := argsFile(t, dir)
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\ncase \"$*\" in\n  *bad.json*) exit 1 ;;\n  *) exit 0 ;;\nesac\n", log)
	withFakeExecutable(t, "stalwart-cli", script)

	err := ApplyAll(context.Background(), ApplyOptions{URL: "http://127.0.0.1:8080", User: "admin", Password: "x"},
		[]string{filepath.Join(dir, "good1.json"), filepath.Join(dir, "bad.json"), filepath.Join(dir, "good2.json")})
	if err == nil {
		t.Fatal("ApplyAll should fail on bad.json")
	}
	got := readArgsFile(t, log)
	if strings.Contains(got, "good2.json") {
		t.Error("ApplyAll should stop before applying good2.json after bad.json failed")
	}
	if !strings.Contains(err.Error(), "1/3") {
		t.Errorf("error = %v, want it to report 1/3 files applied before failing", err)
	}
}
