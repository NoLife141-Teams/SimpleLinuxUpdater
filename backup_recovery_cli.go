package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	internalbackup "debian-updater/internal/backup"
	"debian-updater/internal/installationlock"
	policypkg "debian-updater/internal/policies"
)

const restoreRecoveryUsage = `Usage
  webserver backup recover --inspect
  webserver backup recover --apply <recovery-set-id>
`

func runRestoreRecoveryCommand(args []string, output io.Writer) (bool, error) {
	if len(args) == 0 || args[0] != "backup" {
		return false, nil
	}
	if (len(args) == 2 && args[1] == "--help") ||
		(len(args) == 3 && args[1] == "recover" && args[2] == "--help") {
		_, err := io.WriteString(output, restoreRecoveryUsage)
		return true, err
	}
	if len(args) < 2 || args[1] != "recover" {
		return true, errors.New(strings.TrimSpace(restoreRecoveryUsage))
	}
	flags := flag.NewFlagSet("backup recover", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	inspect := flags.Bool("inspect", false, "inspect without writing active files")
	selection := flags.String("apply", "", "apply the explicitly selected recovery set")
	if err := flags.Parse(args[2:]); err != nil || flags.NArg() != 0 || *inspect == (*selection != "") {
		return true, errors.New("choose --inspect or --apply <recovery-set-id>")
	}
	path := dbPath()
	lease, err := installationlock.Acquire(path, false)
	if err != nil {
		return true, err
	}
	defer lease.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Supply only offline dependencies. Runtime composition starts workers and
	// getEncryptionKey can create files, so neither is used by this command.
	service := NewBackupServiceWithDeps(internalbackup.ServiceDeps{DBPath: func() string { return path }})
	report, recoveryErr := service.OfflineRestoreRecovery(ctx, *selection, func(ctx context.Context, current, staged *sql.DB) error {
		if err := prepareAPTRestore(ctx, current, staged, jobTimestampNow()); err != nil {
			return err
		}
		deps := AppDeps{DB: func() *sql.DB { return staged }, BackupService: service, Now: time.Now}
		if err := newBackupAutomation(deps).SuspendAfterRestore(ctx); err != nil {
			return err
		}
		policies := policypkg.NewSQLiteRepository(policypkg.SQLiteRepositoryDeps{DB: deps.DB})
		return policies.SaveSchedulerWatermark(time.Now().UTC().Truncate(time.Minute))
	})
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return true, errors.Join(recoveryErr, encoder.Encode(report))
}
