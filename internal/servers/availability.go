package servers

import "errors"

func DisabledReadiness() MaintenanceReadiness {
	return MaintenanceReadiness{Code: MaintenanceReadinessDisabled, Message: ErrDisabled.Error()}
}

// SetDisabled persists availability under the same lock as action admission.
// An admitted action, including a pending approval, must finish first.
func (s *Service) SetDisabled(name string, disabled bool) (Server, error) {
	state := s.state()
	state.Lock()
	defer state.Unlock()
	for i, server := range state.Servers() {
		if server.Name != name {
			continue
		}
		if server.Disabled == disabled {
			return server, nil
		}
		if blocked, status := state.ActionStatusInProgressLocked(name); blocked {
			return Server{}, ActionError{Status: status}
		}
		previousServers := state.CloneServers()
		previousStatuses := state.CloneStatusMap()
		server.Disabled = disabled
		state.Servers()[i] = server
		UpdateStatusFromServer(state.StatusMap(), name, server)
		if err := s.SaveOrRollbackLocked(previousServers, previousStatuses, nil); err != nil {
			return Server{}, err
		}
		return server, nil
	}
	return Server{}, ErrNotFound
}

func (c *CommandService) SetServerDisabled(name string, disabled bool) CommandResult {
	action, message := "server.enable", "Server enabled"
	if disabled {
		action, message = "server.disable", "Server disabled"
	}
	server, err := c.inventory.SetDisabled(name, disabled)
	switch {
	case err == nil:
		return successServerCommand(action, name, message, &server, map[string]any{"disabled": disabled})
	case errors.Is(err, ErrNotFound):
		return failedCommand(CommandOutcomeNotFound, action, "server", name, "Server not found", "Server not found", nil)
	case errors.Is(err, ErrActionInProgress):
		return failedCommand(CommandOutcomeConflict, action, "server", name, "Server action already in progress", "wait for the active server action to finish before enabling or disabling this server", actionStatusMeta(err))
	default:
		return failedCommand(CommandOutcomeFailed, action, "server", name, "Failed to save server availability", "Failed to save server availability", map[string]any{"error": err.Error()})
	}
}
