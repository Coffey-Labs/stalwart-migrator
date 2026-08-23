package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/LINUXexpert-org/stalwart-migrator/internal/backup"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/checkpoint"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/rollback"
	"github.com/LINUXexpert-org/stalwart-migrator/internal/service"
)

// splitRunID pulls the run-id out of args wherever it appears, so
// `rollback <run-id> --data-dir X` works as naturally as
// `rollback --data-dir X <run-id>`. Go's flag package stops parsing at the
// first positional argument, which would otherwise make the obvious
// invocation order silently drop every flag after the run-id - on a command
// whose flags decide what gets overwritten, that is not a failure mode
// worth living with. Tokens consumed as a flag's value are skipped by
// asking the FlagSet itself which flags take one, rather than by
// maintaining a second list of them here.
func splitRunID(fs *flag.FlagSet, args []string) (runID string, rest []string) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if strings.HasPrefix(arg, "-") {
			rest = append(rest, arg)
			name := strings.TrimLeft(arg, "-")
			if !strings.Contains(arg, "=") && takesValue(fs, name) && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if runID == "" {
			runID = arg
			continue
		}
		rest = append(rest, arg) // a second positional: let Parse report it
	}
	return runID, rest
}

func takesValue(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !boolFlag.IsBoolFlag()
}

// runRollback implements `stalwart-migrate rollback <run-id>`: put the
// instance back the way it was before the named run touched it
// (ARCHITECTURE.md §4.8).
//
// This is the only command that stops a running mail server and overwrites
// a live data directory, so it always prints the resolved plan first and
// refuses to act without --yes. Everything the plan needs comes from the
// run's own checkpoint - which backup, which manifest, which preserved
// binary - so an operator rolling back days after the fact doesn't have to
// remember any of it. The flags below exist to supply what the checkpoint
// deliberately doesn't store (database credentials) or to override a
// detection that was wrong.
func runRollback(args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	stateDir := fs.String("state-dir", checkpoint.DefaultBaseDir, "directory runs are checkpointed in")
	dataDir := fs.String("data-dir", "/var/lib/stalwart", "stalwart data directory to restore into (embedded backends)")
	binaryPath := fs.String("binary", "/usr/local/bin/stalwart", "path the preserved old binary is reinstalled to")
	serviceUnitPath := fs.String("service-unit", "", "path a preserved systemd unit or Compose file is restored to (only used if the run recorded one)")

	deployment := fs.String("deployment", "", `override how the service is controlled: "systemd" or "docker" (default: whatever preflight detected for this run)`)
	unitName := fs.String("unit", "stalwart", "systemd unit name")
	containerName := fs.String("container", "stalwart", "docker container name")
	stopTimeout := fs.Duration("stop-timeout", 60*time.Second, "how long to wait for the service to actually stop")
	startTimeout := fs.Duration("start-timeout", 60*time.Second, "how long to wait for the restored service to come back")

	adminURL := fs.String("admin-url", "", "base URL for the restored instance's admin/JMAP API, for post-rollback verification")
	adminUser := fs.String("admin-user", "", "admin username")
	adminPassword := fs.String("admin-password", os.Getenv("STALWART_MIGRATE_ADMIN_PASSWORD"),
		"admin password (or set STALWART_MIGRATE_ADMIN_PASSWORD)")
	verifyTimeout := fs.Duration("verify-timeout", 60*time.Second, "how long to wait for the restored instance to answer")

	dbHost := fs.String("db-host", "", "external database host (postgresql/mysql backends)")
	dbPort := fs.String("db-port", "", "external database port")
	dbName := fs.String("db-name", "", "external database name")
	dbUser := fs.String("db-user", "", "external database user")
	dbPassword := fs.String("db-password", os.Getenv("STALWART_MIGRATE_DB_PASSWORD"),
		"external database password (or set STALWART_MIGRATE_DB_PASSWORD)")
	sqlDump := fs.String("sql-dump", "", "override the dump file to replay (default: the one this run recorded)")

	yes := fs.Bool("yes", false, "actually perform the rollback; without it, the plan is printed and nothing is touched")

	runID, rest := splitRunID(fs, args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if runID == "" || fs.NArg() != 0 {
		return fmt.Errorf("usage: stalwart-migrate rollback <run-id> [flags]")
	}

	store := checkpoint.NewStore(*stateDir)
	rs, err := store.Load(runID)
	if err != nil {
		return fmt.Errorf("load run %s: %w", runID, err)
	}

	opts := rollback.Options{
		Deployment: service.Options{
			Kind: service.Kind(*deployment), UnitName: *unitName, ContainerName: *containerName,
		},
		StopTimeout: *stopTimeout, StartTimeout: *startTimeout,
		DataDir:         *dataDir,
		BinaryPath:      *binaryPath,
		ServiceUnitPath: *serviceUnitPath,
		SQL: backup.SQLOptions{
			Host: *dbHost, Port: *dbPort, Database: *dbName, User: *dbUser, Password: *dbPassword, OutPath: *sqlDump,
		},
		AdminURL: *adminURL, AdminUser: *adminUser, AdminPassword: *adminPassword,
		HTTPClient: &http.Client{}, VerifyTimeout: *verifyTimeout,
	}

	plan, err := rollback.BuildPlan(rs, opts)
	if err != nil {
		return err
	}
	fmt.Print(plan.String())

	if !*yes {
		fmt.Println("\nnothing has been touched. Re-run with --yes to perform this rollback.")
		return nil
	}
	if *adminURL == "" {
		fmt.Println("\nnote: without --admin-url, the reachability and directory-count checks after the restart are skipped - " +
			"the rollback will report success on the restore mechanics alone.")
	}

	fmt.Println("\n--- rollback ---")
	report, err := rollback.Run(context.Background(), store, rs, opts)
	fmt.Print(report.String())
	if err != nil {
		return fmt.Errorf("rollback did not complete: %w", err)
	}

	fmt.Printf("\nROLLBACK COMPLETE for run %s: the instance is back on %s. "+
		"The failed attempt's data and binary are preserved under .failed-%s names, and this run's backups are untouched, "+
		"so a retry after the underlying issue is fixed doesn't have to re-capture anything.\n",
		rs.RunID, rs.SourceVersion, rs.RunID)
	return nil
}
