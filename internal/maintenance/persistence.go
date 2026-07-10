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
