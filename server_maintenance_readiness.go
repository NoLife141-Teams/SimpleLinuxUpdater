package main

import (
	"context"
	"strings"

	serverpkg "debian-updater/internal/servers"
)

func maintenanceReadinessForServers(deps AppDeps, serverList []Server) map[string]serverpkg.MaintenanceReadiness {
	globalKeyConfigured, globalKeyKnown := globalSSHCredentialReadiness(deps.GlobalSSHCredential)
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

func globalSSHCredentialReadiness(credential *serverpkg.GlobalSSHCredential) (configured, known bool) {
	if credential == nil {
		return false, false
	}
	resolved, err := credential.Resolve(context.Background(), "")
	if err != nil {
		return false, false
	}
	return resolved.Source == serverpkg.GlobalSSHCredentialSourceGlobal, true
}

func serverByName(state *serverpkg.State, name string) (Server, bool) {
	if state == nil {
		return Server{}, false
	}
	return state.FindByName(name)
}
