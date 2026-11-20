package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	internalbackup "debian-updater/internal/backup"

	"golang.org/x/term"
)

const offlineVerifyUsage = "Usage: webserver backup verify --archive <file> [--work-dir <directory>] [--passphrase-file <file>] [--json]\n"

const (
	offlineVerifyError               = 1
	offlineVerifyUsageError          = 2
	offlineVerifyInvalidArchive      = 3
	offlineVerifyIncompatible        = 4
	offlineVerifyInsufficientStorage = 5
	offlineVerifyInterrupted         = 130
)

type offlineArchiveReport struct {
	Status          string                                `json:"status"`
	Message         string                                `json:"message"`
	ArchiveVerified bool                                  `json:"archive_verified"`
	TargetStatus    string                                `json:"target_status"`
	Storage         []internalbackup.RestoreStorageVolume `json:"storage,omitempty"`
	Findings        []string                              `json:"findings,omitempty"`
}

func runOfflineArchiveVerifyCommand(args []string, input *os.File, output, diagnostics io.Writer) (bool, int) {
	if len(args) < 2 || args[0] != "backup" || args[1] != "verify" {
		return false, 0
	}
	flags := flag.NewFlagSet("backup verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	archive := flags.String("archive", "", "encrypted backup archive to verify")
	workDir := flags.String("work-dir", os.TempDir(), "existing directory for temporary verification files")
	passphraseFile := flags.String("passphrase-file", "", "private file containing the backup passphrase")
	jsonOutput := flags.Bool("json", false, "write the report as JSON")
	err := flags.Parse(args[2:])
	if errors.Is(err, flag.ErrHelp) {
		_, err := io.WriteString(output, offlineVerifyUsage)
		if err != nil {
			return true, offlineVerifyError
		}
		return true, 0
	}
	if err != nil || flags.NArg() != 0 || *archive == "" || *workDir == "" {
		_, _ = io.WriteString(diagnostics, offlineVerifyUsage)
		return true, offlineVerifyUsageError
	}
	report := offlineArchiveReport{TargetStatus: "not_inspected"}
	code := offlineVerifyError
	archiveInfo, archiveErr := os.Stat(*archive)
	workInfo, workErr := os.Stat(*workDir)
	switch {
	case archiveErr != nil || !archiveInfo.Mode().IsRegular():
		report.Status, report.Message = "archive_unavailable", "The archive must be an accessible regular file."
	case workErr != nil || !workInfo.IsDir():
		report.Status, report.Message = "workspace_unavailable", "The workspace must be an existing accessible directory."
	default:
		passphrase, err := readOfflineArchivePassphrase(input, diagnostics, *passphraseFile)
		if err != nil {
			report.Status, report.Message = "passphrase_unavailable", err.Error()
			break
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		service := NewBackupServiceWithDeps(internalbackup.ServiceDeps{TempDir: func() string { return *workDir }})
		result, err := service.VerifyArchiveFileOffline(ctx, internalbackup.TemporaryFile{
			Path: *archive, Size: archiveInfo.Size(),
		}, passphrase)
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		cancel()
		report, code = classifyOfflineArchiveVerification(result, err)
	}
	if err := writeOfflineArchiveReport(output, report, *jsonOutput); err != nil {
		_, _ = io.WriteString(diagnostics, "Could not write the verification report.\n")
		return true, offlineVerifyError
	}
	return true, code
}

func readOfflineArchivePassphrase(input *os.File, diagnostics io.Writer, path string) (string, error) {
	if path == "" {
		if input == nil || !term.IsTerminal(int(input.Fd())) {
			return "", errors.New("use a terminal for hidden passphrase entry or supply a private --passphrase-file")
		}
		restore, err := guardRecoveryTerminal(input, diagnostics)
		if err != nil {
			return "", err
		}
		defer restore()
		if _, err := io.WriteString(diagnostics, "Backup passphrase: "); err != nil {
			return "", errors.New("could not display the passphrase prompt")
		}
		secret, readErr := term.ReadPassword(int(input.Fd()))
		defer clear(secret)
		_, _ = io.WriteString(diagnostics, "\n")
		if readErr != nil {
			return "", errors.New("could not read the backup passphrase from the terminal")
		}
		return strings.TrimSpace(string(secret)), nil
	}

	const limit = 4096
	private := func(info os.FileInfo) bool {
		return info.Mode().IsRegular() && info.Size() <= limit && info.Mode().Perm()&0077 == 0
	}
	invalid := errors.New("the passphrase file must be a readable private regular file of at most 4096 bytes")
	info, err := os.Stat(path)
	if err != nil || !private(info) {
		return "", invalid
	}
	file, err := os.Open(path)
	if err != nil {
		return "", invalid
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !private(info) {
		return "", invalid
	}
	secret, err := io.ReadAll(io.LimitReader(file, limit+1))
	defer clear(secret)
	if err != nil || len(secret) > limit {
		return "", invalid
	}
	return strings.TrimSpace(string(secret)), nil
}

func classifyOfflineArchiveVerification(result internalbackup.VerifyResult, err error) (offlineArchiveReport, int) {
	report := offlineArchiveReport{TargetStatus: "not_inspected", Storage: result.Storage}
	switch {
	case errors.Is(err, context.Canceled):
		report.Status, report.Message = "interrupted", "Archive verification was interrupted."
		return report, offlineVerifyInterrupted
	case internalbackup.IsStorageFull(err):
		report.Status, report.Message = "insufficient_storage", "The verification workspace ran out of space."
		return report, offlineVerifyInsufficientStorage
	case errors.Is(err, internalbackup.ErrInvalidPassphrase):
		report.Status, report.Message = "invalid_passphrase", "The passphrase does not meet the minimum length."
		return report, offlineVerifyInvalidArchive
	case errors.Is(err, internalbackup.ErrArchiveAuthentication):
		report.Status, report.Message = "archive_authentication_failed", "The passphrase is incorrect or the encrypted archive is corrupted."
		return report, offlineVerifyInvalidArchive
	case err != nil:
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			report.Status, report.Message = "verification_io_error", "The archive or workspace could not be accessed."
			return report, offlineVerifyError
		}
		report.Status, report.Message = "invalid_archive", "The archive could not be read or did not pass integrity validation."
		return report, offlineVerifyInvalidArchive
	}
	for _, warning := range result.Warnings {
		if warning.Code == "storage_capacity_unknown" {
			report.Findings = append(report.Findings, warning.Code)
		}
	}
	code := offlineVerifyInvalidArchive
	report.Status, report.Message = "invalid_archive", "The archive did not pass verification."
	for _, blocker := range result.Blockers {
		report.Findings = append(report.Findings, blocker.Code)
		switch blocker.Code {
		case "insufficient_storage":
			report.Status, report.Message = "insufficient_storage", "The verification workspace does not have enough space."
			code = offlineVerifyInsufficientStorage
		case "unsupported_format", "unsupported_version", "unsupported_encryption", "unsupported_manifest", "invalid_database", "invalid_notification_state":
			report.Status, report.Message = "incompatible_archive", "The archive is incompatible with this application."
			code = offlineVerifyIncompatible
		}
	}
	if result.RestoreReady {
		report.Status, report.Message = "verified", "Archive verified. The restoration target was not inspected."
		report.ArchiveVerified = true
		code = 0
	}
	return report, code
}

func writeOfflineArchiveReport(output io.Writer, report offlineArchiveReport, asJSON bool) error {
	if asJSON {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	if _, err := fmt.Fprintln(output, report.Message); err != nil {
		return err
	}
	for _, volume := range report.Storage {
		available := "unknown"
		if volume.AvailableBytes != nil {
			available = fmt.Sprintf("%d bytes", *volume.AvailableBytes)
		}
		if _, err := fmt.Fprintf(output, "%s: %s (%d bytes required, %s available)\n",
			strings.Join(volume.Roles, ", "), volume.State, volume.RequiredBytes, available); err != nil {
			return err
		}
	}
	return nil
}
