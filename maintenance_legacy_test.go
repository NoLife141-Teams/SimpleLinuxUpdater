package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"
)

const maintenanceStateSetting = "maintenance_state"

var (
	errMaintenanceModeActive = errors.New("maintenance mode active")
	maintenanceStateMu       sync.RWMutex
	maintenanceState         MaintenanceState
)

type MaintenanceState struct {
	Active    bool   `json:"active"`
	Kind      string `json:"kind"`
	JobID     string `json:"job_id"`
	StartedAt string `json:"started_at"`
	Actor     string `json:"actor"`
	Message   string `json:"message"`
}

func currentMaintenanceState() MaintenanceState {
	maintenanceStateMu.RLock()
	defer maintenanceStateMu.RUnlock()
	return maintenanceState
}

func setCurrentMaintenanceState(state MaintenanceState) {
	maintenanceStateMu.Lock()
	maintenanceState = state
	maintenanceStateMu.Unlock()
}

func loadPersistedMaintenanceState() (MaintenanceState, error) {
	var raw string
	err := getDB().QueryRow("SELECT value FROM settings WHERE key = ?", maintenanceStateSetting).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || strings.TrimSpace(raw) == "" {
		return MaintenanceState{}, nil
	}
	if err != nil {
		return MaintenanceState{}, err
	}
	var state MaintenanceState
	return state, json.Unmarshal([]byte(raw), &state)
}

func persistMaintenanceState(state MaintenanceState) error {
	blob, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = getDB().Exec("INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", maintenanceStateSetting, string(blob))
	return err
}

func initializeMaintenanceState() error {
	state, err := loadPersistedMaintenanceState()
	if err != nil {
		return err
	}
	if state.Active {
		state = MaintenanceState{}
		if err := persistMaintenanceState(state); err != nil {
			return err
		}
	}
	setCurrentMaintenanceState(state)
	return nil
}

func activateMaintenance(kind, jobID, actor, message string) error {
	state := MaintenanceState{Active: true, Kind: strings.TrimSpace(kind), JobID: strings.TrimSpace(jobID), StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Actor: strings.TrimSpace(actor), Message: strings.TrimSpace(message)}
	if err := persistMaintenanceState(state); err != nil {
		return err
	}
	setCurrentMaintenanceState(state)
	return nil
}

func deactivateMaintenance() error {
	if err := persistMaintenanceState(MaintenanceState{}); err != nil {
		return err
	}
	setCurrentMaintenanceState(MaintenanceState{})
	return nil
}

func maintenancePageHTML() string {
	state := currentMaintenanceState()
	return maintenancePageHTMLFromSnapshot(maintenancepkg.Snapshot{Active: state.Active, Kind: state.Kind, StartedAt: state.StartedAt, Message: state.Message})
}
