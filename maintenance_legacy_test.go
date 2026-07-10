package main

import (
	"sync"

	maintenancepkg "debian-updater/internal/maintenance"
)

var (
	maintenanceStateMu sync.RWMutex
	maintenanceState   MaintenanceState
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

func maintenancePageHTML() string {
	state := currentMaintenanceState()
	return maintenancePageHTMLFromSnapshot(maintenancepkg.Snapshot{Active: state.Active, Kind: state.Kind, StartedAt: state.StartedAt, Message: state.Message})
}
