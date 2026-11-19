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
	files        restoreFiles
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

// ScheduledWork reports the first eligible schedule slot after isolation
// and whether the scheduler must stay paused at the given tick.
func (s *RestoreIsolation) ScheduledWork(at time.Time) (resumeAt time.Time, blocked bool) {
	if s == nil {
		return time.Time{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, err := s.read()
	return state.ResumeSchedules, err != nil || state.Active || at.Before(state.ResumeSchedules)
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
	published, err := s.replaceState(path, state)
	if !published {
		return err
	}
	s.syncFailed = err != nil
	if err != nil && !state.Active {
		// A failed resumption must keep isolation visible after restart.
		// A rename can already be visible even when the directory sync fails.
		_, retainErr := s.replaceState(path, restoreIsolationState{Active: true})
		s.syncFailed = retainErr != nil
		return errors.Join(err, retainErr)
	}
	return err
}

func (s *RestoreIsolation) replaceState(path string, state restoreIsolationState) (published bool, err error) {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".slu-restore-isolation-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(file.Name())
	if err := json.NewEncoder(file).Encode(state); err != nil {
		return false, errors.Join(err, file.Close())
	}
	if err := errors.Join(s.files.syncFile(file), file.Close()); err != nil {
		return false, err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return false, err
	}
	return true, s.files.syncPath(dir)
}
