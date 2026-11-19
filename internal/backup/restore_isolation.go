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
}

func NewRestoreIsolation(databasePath func() string) *RestoreIsolation {
	return &RestoreIsolation{databasePath: databasePath}
}

func RestoreIsolationPath(database string) string { return database + ".restore-isolation" }

func restoreIsolationPendingPath(database string) string {
	return RestoreIsolationPath(database) + ".resume-pending"
}

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
	database := s.databasePath()
	if info, err := os.Lstat(restoreIsolationPendingPath(database)); err == nil {
		if !info.IsDir() {
			return restoreIsolationState{}, errors.New("invalid restore isolation resumption marker")
		}
		return restoreIsolationState{Active: true}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return restoreIsolationState{}, err
	}
	return loadRestoreIsolationState(RestoreIsolationPath(database))
}

func loadRestoreIsolationState(path string) (restoreIsolationState, error) {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	database, err := s.resolveDatabaseForWrite()
	if err != nil {
		return err
	}
	return s.replaceState(RestoreIsolationPath(database), restoreIsolationState{Active: true})
}

func (s *RestoreIsolation) Reactivate(now func() time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	database, err := s.resolveDatabaseForWrite()
	if err != nil {
		return err
	}
	// Keep a durable gate across restarts until the new schedule boundary is
	// committed. A temporary file already renamed into place can be visible
	// after a sync error.
	pending := restoreIsolationPendingPath(database)
	if err := os.Mkdir(pending, 0700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if info, statErr := os.Lstat(pending); statErr != nil || !info.IsDir() {
			return errors.New("invalid restore isolation resumption marker")
		}
	}
	dir := filepath.Dir(database)
	if err := errors.Join(s.files.syncPath(pending), s.files.syncPath(dir)); err != nil {
		return err
	}
	resumeAt := now().UTC().Truncate(time.Minute).Add(time.Minute)
	if err := s.replaceState(RestoreIsolationPath(database), restoreIsolationState{ResumeSchedules: resumeAt}); err != nil {
		return err
	}
	if !now().Before(resumeAt) {
		return errors.New("restore isolation schedule boundary elapsed during resumption")
	}
	if err := os.Remove(pending); err != nil {
		return err
	}
	// Removal releases the gate after all required writes are durable. If
	// cleanup synchronization fails, a crash may safely restore the gate.
	_ = s.files.syncPath(dir)
	return nil
}

func (s *RestoreIsolation) resolveDatabaseForWrite() (string, error) {
	database, err := installationlock.ResolveDatabasePath(s.databasePath())
	if err != nil {
		return "", err
	}
	if _, err := loadRestoreIsolationState(RestoreIsolationPath(database)); err != nil {
		return "", err
	}
	return database, nil
}

func (s *RestoreIsolation) replaceState(path string, state restoreIsolationState) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".slu-restore-isolation-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := json.NewEncoder(file).Encode(state); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := errors.Join(s.files.syncFile(file), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return s.files.syncPath(dir)
}
