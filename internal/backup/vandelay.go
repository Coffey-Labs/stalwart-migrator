package backup

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
)

// VandelayOptions configures a per-account content export via Stalwart's
// own Vandelay import/export tool - the documented, backend-independent
// backup mechanism (ARCHITECTURE.md §4.2's belt-and-suspenders layer): each
// account's mail, calendars, contacts, Sieve scripts, and identities land in
// one self-contained SQLite archive.
type VandelayOptions struct {
	BinaryPath string // defaults to "vandelay"
	URL        string // the source JMAP server
	AuthBasic  string // "user:app-password", per vandelay's own --auth-basic flag
	OutDir     string // one <account>.sqlite file per account goes here
}

// BuildVandelayImportArgs returns vandelay's argv for exporting one
// account's content into a self-contained SQLite archive, without the
// leading "vandelay" itself. "import" is vandelay's own verb for this - it
// names the direction relative to the archive file, not the live server;
// see ARCHITECTURE.md §4.2.
func BuildVandelayImportArgs(o VandelayOptions, accountName, outFile string) []string {
	return []string{
		"import", "jmap",
		"--url", o.URL,
		"--auth-basic", o.AuthBasic,
		"--account-name", accountName,
		outFile,
	}
}

// ExportAccount runs one account's Vandelay export and returns the archive
// path it wrote.
func ExportAccount(ctx context.Context, o VandelayOptions, accountName string) (outFile string, err error) {
	binary := o.BinaryPath
	if binary == "" {
		binary = "vandelay"
	}
	outFile = filepath.Join(o.OutDir, accountName+".sqlite")
	cmd := exec.CommandContext(ctx, binary, BuildVandelayImportArgs(o, accountName, outFile)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("backup: vandelay export of %s failed: %w (output: %s)", accountName, err, out)
	}
	return outFile, nil
}

// ExportAccounts exports every account in accounts, stopping at the first
// failure. A partial per-account backup set is reported precisely (how many
// succeeded, which one failed and why) rather than silently continuing past
// a failure that might indicate a systemic problem - bad credentials, an
// unreachable server - rather than a one-off.
func ExportAccounts(ctx context.Context, o VandelayOptions, accounts []string) (outFiles []string, err error) {
	for _, acct := range accounts {
		f, err := ExportAccount(ctx, o, acct)
		if err != nil {
			return outFiles, fmt.Errorf("backup: exported %d/%d accounts before failing: %w", len(outFiles), len(accounts), err)
		}
		outFiles = append(outFiles, f)
	}
	return outFiles, nil
}
