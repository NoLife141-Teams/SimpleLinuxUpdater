package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	backupSecondaryMaxAttempts   = 5
	backupSecondaryQueueLimit    = 16
	backupSecondaryRetryBase     = time.Minute
	backupSecondaryValidateRoute = "/api/backup/automation/secondary/validate"
)

type backupSecondaryBinding struct {
	Path      string `json:"path_binding,omitempty"`
	Directory string `json:"directory_identity,omitempty"`
	Owner     string `json:"owner,omitempty"`
}

type backupSecondaryCopy struct {
	Archive      ownedBackupArchive `json:"archive"`
	Attempts     int                `json:"attempts"`
	Transferring bool               `json:"transferring,omitempty"`
	RetryAt      string             `json:"retry_at,omitempty"`
	Failure      string             `json:"failure,omitempty"`
	Temporary    string             `json:"temporary,omitempty"`
	FileIdentity string             `json:"file_identity,omitempty"`
	ModifiedAt   string             `json:"modified_at,omitempty"`
	ChangedAt    string             `json:"changed_at,omitempty"`
	VerifiedAt   string             `json:"verified_at,omitempty"`
	Unavailable  bool               `json:"unavailable,omitempty"`
}

func (copy backupSecondaryCopy) pending() bool {
	return copy.VerifiedAt == "" && (copy.Transferring || copy.Attempts < backupSecondaryMaxAttempts)
}

type backupSecondaryState struct {
	Binding     backupSecondaryBinding `json:"binding,omitzero"`
	Approved    bool                   `json:"approved"`
	Copies      []backupSecondaryCopy  `json:"copies,omitempty"`
	LastFailure string                 `json:"last_failure,omitempty"`
}

type backupSecondaryStatus struct {
	State      string `json:"state"`
	Pending    int    `json:"pending"`
	Failed     int    `json:"failed"`
	Failure    string `json:"failure,omitempty"`
	CapturedAt string `json:"captured_at,omitempty"`
	VerifiedAt string `json:"verified_at,omitempty"`
}

func (a *backupAutomation) secondaryConfigured(state backupSecondaryState) bool {
	return state.Approved && state.Binding.Path != "" &&
		state.Binding.Path == backupStorageBinding(a.deployment.SecondaryDirectory)
}

func (a *backupAutomation) secondaryStatus(state backupSecondaryState) *backupSecondaryStatus {
	if a.deployment.SecondaryDirectory == "" {
		return nil
	}
	status := &backupSecondaryStatus{State: "validation_required"}
	if state.Binding.Path != backupStorageBinding(a.deployment.SecondaryDirectory) {
		return status
	}
	var latestVerified, latestCapture time.Time
	hasUnavailableCopies := false
	for _, copy := range state.Copies {
		if copy.Unavailable {
			hasUnavailableCopies = true
			status.Failed++
		} else if at := parseBackupStamp(copy.VerifiedAt); !at.IsZero() {
			captured := parseBackupStamp(copy.Archive.CapturedAt)
			if latestVerified.IsZero() || captured.After(latestCapture) || (captured.Equal(latestCapture) && at.After(latestVerified)) {
				status.CapturedAt, status.VerifiedAt = copy.Archive.CapturedAt, copy.VerifiedAt
				latestCapture, latestVerified = captured, at
			}
		} else if copy.pending() {
			status.Pending++
		} else {
			status.Failed++
		}
	}
	if a.secondaryConfigured(state) {
		status.Failure = state.LastFailure
		if status.Failure == "" && hasUnavailableCopies {
			status.Failure = "copy_unavailable"
		}
		switch {
		case status.Failure != "" || status.Failed > 0:
			status.State = "degraded"
		case status.Pending > 0:
			status.State = "pending"
		case status.VerifiedAt != "":
			status.State = "verified"
		default:
			status.State = "waiting"
		}
	}
	return status
}

func (a *backupAutomation) queueSecondaryCopy(state *backupAutomationState, archive ownedBackupArchive) {
	if !a.secondaryConfigured(state.Secondary) || archive.Owner != state.Secondary.Binding.Owner {
		return
	}
	pending := 0
	for _, copy := range state.Secondary.Copies {
		if copy.Archive.ID == archive.ID {
			return
		}
		if copy.pending() {
			pending++
		}
	}
	if pending >= backupSecondaryQueueLimit {
		state.Secondary.LastFailure = "queue_full"
		return
	}
	// Bound historical failures alongside the pending source pins. Temp
	// registrations remain until the worker can safely clean the approved mount.
	failed := 0
	for i := len(state.Secondary.Copies) - 1; i >= 0; i-- {
		copy := state.Secondary.Copies[i]
		if copy.VerifiedAt == "" && !copy.pending() && copy.Temporary == "" {
			failed++
			if failed > backupSecondaryQueueLimit {
				state.Secondary.Copies = slices.Delete(state.Secondary.Copies, i, i+1)
			}
		}
	}
	state.Secondary.Copies = append(state.Secondary.Copies, backupSecondaryCopy{Archive: archive})
}

func (a *backupAutomation) secondaryProtectsSource(state backupSecondaryState, id string) bool {
	if !a.secondaryConfigured(state) {
		return false
	}
	for _, copy := range state.Copies {
		if copy.Archive.ID == id && copy.pending() {
			return true
		}
	}
	return false
}

func (a *backupAutomation) openSecondaryDestination() (*os.Root, backupSecondaryBinding, error) {
	var binding backupSecondaryBinding
	if a.deployment.SecondaryDirectory == "" {
		return nil, binding, errors.New("a secondary backup directory has not been configured")
	}
	directory, err := backupRealPath(a.deployment.SecondaryDirectory)
	if err != nil {
		return nil, binding, errors.New("secondary backup storage is unavailable or contains a symbolic link")
	}
	primary := filepath.Clean(a.deployment.Directory)
	paths := a.deps.BackupService.Status()
	dataPaths := []string{primary, string(filepath.Separator)}
	protected := []string{primary, filepath.Dir(a.deployment.SecretFile)}
	for _, path := range []string{paths.DBPath, paths.ConfigPath, paths.KnownHostsPath} {
		if path != "" {
			dataPaths = append(dataPaths, filepath.Dir(path))
			protected = append(protected, filepath.Dir(path))
		}
	}
	for _, path := range protected {
		if pathWithin(path, directory) || pathWithin(directory, path) {
			return nil, binding, errors.New("secondary backup storage must be separate from the primary archive, secret and application data directories")
		}
	}
	owner, err := a.localIdentity(false)
	if err != nil {
		return nil, binding, errors.New("validate the primary backup destination before the secondary destination")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, binding, errors.New("secondary backup storage cannot be opened")
	}
	identity := backupArchiveDirectoryIdentity(root)
	if identity == "" || !backupArchiveMountAvailable(directory, identity) {
		_ = root.Close()
		return nil, binding, errors.New("secondary backup storage could not be identified safely")
	}
	for _, path := range protected {
		if info, err := os.Stat(path); err == nil && backupCatalogFileIdentity(info) == identity {
			_ = root.Close()
			return nil, binding, errors.New("secondary backup storage aliases another application directory")
		}
	}
	for _, path := range dataPaths {
		if info, err := os.Stat(path); err == nil && backupFilesystemDevice(backupCatalogFileIdentity(info)) == backupFilesystemDevice(identity) {
			_ = root.Close()
			return nil, binding, errors.New("secondary backup storage must be mounted separately from the primary archives and application data")
		}
	}
	return root, backupSecondaryBinding{Path: backupStorageBinding(directory), Directory: identity, Owner: owner}, nil
}

func backupFilesystemDevice(identity string) string {
	device, _, _ := strings.Cut(identity, ":")
	return device
}

func (a *backupAutomation) ValidateSecondaryDestination(ctx context.Context) (*backupSecondaryStatus, error) {
	if !a.secondaryMu.TryLock() {
		return nil, errors.New("wait for the current secondary backup before validating the destination")
	}
	defer a.secondaryMu.Unlock()
	root, binding, err := a.openSecondaryDestination()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	a.mu.Lock()
	defer a.mu.Unlock()
	state, err := a.load(ctx)
	if err != nil {
		return nil, errors.New("secondary backup settings are unavailable")
	}
	if !backupArchiveMountAvailable(root.Name(), binding.Directory) {
		return nil, errors.New("secondary backup storage changed during validation")
	}
	if state.Secondary.Binding != binding {
		// A different mount grants no authority over files from the old one.
		state.Secondary = backupSecondaryState{Binding: binding}
	}
	state.Secondary.Approved = true
	if err := a.save(ctx, state); err != nil {
		return nil, errors.New("secondary backup validation could not be saved")
	}
	return a.secondaryStatus(state.Secondary), nil
}

func handleBackupSecondaryValidate(c *gin.Context, automation *backupAutomation) {
	setNoStoreHeaders(c)
	status, err := automation.ValidateSecondaryDestination(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, status)
}
