package maintenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

const stateSetting = "maintenance_state"

type SQLiteStore struct {
	DB func() *sql.DB
}

func (s SQLiteStore) database() (*sql.DB, error) {
	if s.DB == nil {
		return nil, errors.New("database is not initialized")
	}
	db := s.DB()
	if db == nil {
		return nil, errors.New("database is not initialized")
	}
	return db, nil
}

func (s SQLiteStore) Load(ctx context.Context) (State, error) {
	db, err := s.database()
	if err != nil {
		return State{}, err
	}
	var raw string
	err = db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", stateSetting).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || strings.TrimSpace(raw) == "" {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return State{}, err
	}
	return state, nil
}

func (s SQLiteStore) Save(ctx context.Context, state State) error {
	db, err := s.database()
	if err != nil {
		return err
	}
	blob, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		"INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		stateSetting,
		string(blob),
	)
	return err
}

// PrepareReplacement stamps the owning restore into an offline replacement
// database without changing the live lease. Successful restores clear it through
// normal Close after handoff/reload. Startup treats either this recovery marker or the
// active restore written by Handoff as an interrupted operation.
func (l *ExclusiveLease) PrepareReplacement(ctx context.Context, db *sql.DB) error {
	if l == nil || l.coordinator == nil {
		return errors.New("maintenance lease is not configured")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.releasePending || !l.activated || l.operation != OperationBackupRestore {
		return errors.New("active backup restore lease is required for replacement preparation")
	}
	state := l.activeState
	state.RecoveryRequired = true
	state.Message = recoveryRequiredMessage
	return (SQLiteStore{DB: func() *sql.DB { return db }}).Save(ctx, state)
}
