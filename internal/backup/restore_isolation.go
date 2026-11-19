package backup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"debian-updater/internal/installationlock"
)

type restoreIsolationState struct {
	Active          bool      `json:"active"`
	ResumeSchedules time.Time `json:"resume_schedules,omitzero"`
}

// RestoreIsolation belongs to the local installation. Archive application can
// replace SQLite and host trust while this file continues to govern outbound work.
type RestoreIsolation struct {
	databasePath func() string
	mu           sync.RWMutex
	syncFailed   bool
}

func NewRestoreIsolation(databasePath func() string) *RestoreIsolation {
	return &RestoreIsolation{databasePath: databasePath}
}

func RestoreIsolationPath(database string) string { return database + ".restore-isolation" }

func (s *RestoreIsolation) State() (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, err := s.read()
	return state.Active || err != nil, err
}

func (s *RestoreIsolation) BlocksExternalWork() bool {
	if s == nil {
		return false
	}
	active, _ := s.State()
	return active
}

func (s *RestoreIsolation) BlocksScheduledWork(at time.Time) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, err := s.read()
	return err != nil || state.Active || at.Before(state.ResumeSchedules)
}

func (s *RestoreIsolation) read() (restoreIsolationState, error) {
	if s.syncFailed {
		return restoreIsolationState{}, errors.New("restore isolation state was not synchronized")
	}
	path := RestoreIsolationPath(s.databasePath())
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return restoreIsolationState{}, nil
	}
	raw, err := readRecoveryFile(path, 512)
	if err != nil {
		return restoreIsolationState{}, err
	}
	var state restoreIsolationState
	if err := json.Unmarshal(raw, &state); err != nil || state.Active != state.ResumeSchedules.IsZero() {
		return restoreIsolationState{}, errors.New("invalid restore isolation state")
	}
	return state, nil
}

func (s *RestoreIsolation) Activate() error {
	return s.write(restoreIsolationState{Active: true})
}

func (s *RestoreIsolation) Reactivate(resumeSchedules time.Time) error {
	return s.write(restoreIsolationState{ResumeSchedules: resumeSchedules.UTC()})
}

func (s *RestoreIsolation) write(state restoreIsolationState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	database, err := installationlock.ResolveDatabasePath(s.databasePath())
	if err != nil {
		return err
	}
	path := RestoreIsolationPath(database)
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("restore isolation path is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(database)
	file, err := os.CreateTemp(dir, ".slu-restore-isolation-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := json.NewEncoder(file).Encode(state); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	err = (restoreFiles{}).syncPath(dir)
	s.syncFailed = err != nil
	return err
}
