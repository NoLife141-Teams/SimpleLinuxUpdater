package main

import (
	"context"
	"strings"

	serverpkg "debian-updater/internal/servers"
)

func maintenanceReadinessForServers(deps AppDeps, serverList []Server) map[string]serverpkg.MaintenanceReadiness {
	globalKeyConfigured := false
	globalKeyKnown := false
	if deps.GlobalSSHCredential != nil {
		status, err := deps.GlobalSSHCredential.Status(context.Background())
		if err == nil {
			globalKeyConfigured = status.Configured
			globalKeyKnown = true
		}
	}
	hostKeyStatuses := map[string]serverpkg.HostKeyStatus{}
	if deps.ServerInventoryService != nil {
		for _, status := range deps.ServerInventoryService.ListStatuses() {
			hostKeyStatuses[status.Name] = status.HostKeyStatus
		}
	}
	result := make(map[string]serverpkg.MaintenanceReadiness, len(serverList))
	for _, server := range serverList {
		result[server.Name] = serverpkg.EvaluateMaintenanceReadiness(
			strings.TrimSpace(server.Pass) != "",
			strings.TrimSpace(server.Key) != "",
			globalKeyConfigured,
			globalKeyKnown,
			hostKeyStatuses[server.Name],
		)
	}
	return result
}

func serverByName(state *serverpkg.State, name string) (Server, bool) {
	if state == nil {
		return Server{}, false
	}
	for _, server := range state.CloneServers() {
		if server.Name == name {
			return server, true
		}
	}
	return Server{}, false
}
