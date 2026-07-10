package servers

import (
	"errors"
	"fmt"
	"strings"
)

type CommandOutcome string

const (
	CommandOutcomeSuccess     CommandOutcome = "success"
	CommandOutcomeInvalid     CommandOutcome = "invalid"
	CommandOutcomeConflict    CommandOutcome = "conflict"
	CommandOutcomeNotFound    CommandOutcome = "not_found"
	CommandOutcomeFailed      CommandOutcome = "failed"
	CommandOutcomeRemoteError CommandOutcome = "remote_error"
)

type CommandAudit struct {
	Action     string
	TargetType string
	TargetName string
	Status     string
	Message    string
	Meta       map[string]any
}

type CommandResult struct {
	Outcome       CommandOutcome
	Message       string
	Error         string
	Audit         CommandAudit
	ActiveServers []string
	Server        *Server
	HostKeyScan   *HostKeyScanResult
	HostKeyTrust  *HostKeyTrustResult
	HostKeyClear  *HostKeyClearResult
}

func (r CommandResult) Succeeded() bool {
	return r.Outcome == CommandOutcomeSuccess
}

type CommandService struct {
	inventory *Service
}

func NewCommandService(inventory *Service) *CommandService {
	return &CommandService{inventory: inventory}
}

func (c *CommandService) CreateServer(server Server) CommandResult {
	created, err := c.inventory.Create(server)
	switch {
	case err == nil:
		return successServerCommand("server.create", created.Name, "Server created", &created, map[string]any{
			"host":       created.Host,
			"port":       created.Port,
			"tags_count": len(created.Tags),
		})
	case errors.Is(err, ErrRequiredFields):
		target := strings.TrimSpace(server.Name)
		return failedCommand(CommandOutcomeInvalid, "server.create", "server", target, "Missing required fields", "name, host, and user are required", nil)
	case errors.Is(err, ErrInvalidSSHUsername):
		target := strings.TrimSpace(server.Name)
		user := strings.TrimSpace(server.User)
		return failedCommand(CommandOutcomeInvalid, "server.create", "server", target, "Invalid SSH username", "invalid user; allowed characters are letters, digits, '.', '-', '_'", map[string]any{"user": user})
	case errors.Is(err, ErrNameExists):
		target := strings.TrimSpace(server.Name)
		return failedCommand(CommandOutcomeConflict, "server.create", "server", target, "Server name already exists", "Server name already exists", nil)
	case errors.Is(err, ErrHostExists):
		target := strings.TrimSpace(server.Name)
		host := strings.TrimSpace(server.Host)
		return failedCommand(CommandOutcomeConflict, "server.create", "server", target, "Server host already exists", "Server host already exists", map[string]any{"host": host})
	default:
		target := strings.TrimSpace(server.Name)
		return failedCommand(CommandOutcomeFailed, "server.create", "server", target, "Failed to persist server", fmt.Sprintf("Failed to save servers: %v", err), map[string]any{"error": err.Error()})
	}
}

func (c *CommandService) UpdateServer(name string, server Server) CommandResult {
	updated, err := c.inventory.Update(name, server)
	switch {
	case err == nil:
		return successServerCommand("server.update", updated.Name, "Server updated", &updated, map[string]any{
			"from":       name,
			"host":       updated.Host,
			"port":       updated.Port,
			"tags_count": len(updated.Tags),
		})
	case errors.Is(err, ErrRequiredFields):
		return failedCommand(CommandOutcomeInvalid, "server.update", "server", name, "Missing required fields", "name, host, and user are required", nil)
	case errors.Is(err, ErrInvalidSSHUsername):
		user := strings.TrimSpace(server.User)
		return failedCommand(CommandOutcomeInvalid, "server.update", "server", name, "Invalid SSH username", "invalid user; allowed characters are letters, digits, '.', '-', '_'", map[string]any{"user": user})
	case errors.Is(err, ErrActionInProgress):
		return failedCommand(CommandOutcomeConflict, "server.update", "server", name, "Server action already in progress", "wait for the active server action to finish before editing this server", actionStatusMeta(err))
	case errors.Is(err, ErrNameExists):
		return failedCommand(CommandOutcomeConflict, "server.update", "server", name, "Server name already exists", "Server name already exists", nil)
	case errors.Is(err, ErrHostExists):
		host := strings.TrimSpace(server.Host)
		return failedCommand(CommandOutcomeConflict, "server.update", "server", name, "Server host already exists", "Server host already exists", map[string]any{"host": host})
	case errors.Is(err, ErrNotFound):
		return failedCommand(CommandOutcomeNotFound, "server.update", "server", name, "Server not found", "Server not found", nil)
	default:
		return failedCommand(CommandOutcomeFailed, "server.update", "server", name, "Failed to persist server", fmt.Sprintf("Failed to save servers: %v", err), map[string]any{"error": err.Error()})
	}
}

func (c *CommandService) DeleteServer(name string) CommandResult {
	err := c.inventory.Delete(name)
	switch {
	case err == nil:
		return successMessageCommand("server.delete", "server", name, "Server deleted", nil)
	case errors.Is(err, ErrActionInProgress):
		return failedCommand(CommandOutcomeConflict, "server.delete", "server", name, "Server action already in progress", "wait for the active server action to finish before deleting this server", actionStatusMeta(err))
	case errors.Is(err, ErrNotFound):
		return failedCommand(CommandOutcomeNotFound, "server.delete", "server", name, "Server not found", "Server not found", nil)
	default:
		return failedCommand(CommandOutcomeFailed, "server.delete", "server", name, "Failed to persist deletion", fmt.Sprintf("Failed to save servers: %v", err), map[string]any{"error": err.Error()})
	}
}

func (c *CommandService) ClearPassword(name string) CommandResult {
	err := c.inventory.ClearPassword(name)
	switch {
	case err == nil:
		return successMessageCommand("server.password.clear", "server", name, "Password cleared", nil)
	case errors.Is(err, ErrActionInProgress):
		return failedCommand(CommandOutcomeConflict, "server.password.clear", "server", name, "Server action already in progress", "wait for the active server action to finish before clearing this server password", actionStatusMeta(err))
	case errors.Is(err, ErrNotFound):
		return failedCommand(CommandOutcomeNotFound, "server.password.clear", "server", name, "Server not found", "Server not found", nil)
	default:
		return failedCommand(CommandOutcomeFailed, "server.password.clear", "server", name, "Failed to persist password clear", fmt.Sprintf("Failed to save servers: %v", err), map[string]any{"error": err.Error()})
	}
}

func (c *CommandService) CheckServerKeyUpload(name string) CommandResult {
	err := c.inventory.CheckMutationAllowed(name)
	switch {
	case err == nil:
		return CommandResult{Outcome: CommandOutcomeSuccess}
	case errors.Is(err, ErrNotFound):
		return failedCommand(CommandOutcomeNotFound, "server.key.upload", "server", name, "Server not found", "Server not found", nil)
	case errors.Is(err, ErrActionInProgress):
		return failedCommand(CommandOutcomeConflict, "server.key.upload", "server", name, "Server action already in progress", "wait for the active server action to finish before updating this server key", actionStatusMeta(err))
	default:
		return failedCommand(CommandOutcomeFailed, "server.key.upload", "server", name, "Failed to save key", err.Error(), map[string]any{"error": err.Error()})
	}
}

func (c *CommandService) SetServerKey(name, key string) CommandResult {
	err := c.inventory.SetKey(name, key)
	switch {
	case err == nil:
		return successMessageCommand("server.key.upload", "server", name, "SSH key uploaded", nil).withMessage("Key uploaded")
	case errors.Is(err, ErrActionInProgress):
		return failedCommand(CommandOutcomeConflict, "server.key.upload", "server", name, "Server action already in progress", "wait for the active server action to finish before updating this server key", actionStatusMeta(err))
	case errors.Is(err, ErrNotFound):
		return failedCommand(CommandOutcomeNotFound, "server.key.upload", "server", name, "Server not found", "Server not found", nil)
	default:
		return failedCommand(CommandOutcomeFailed, "server.key.upload", "server", name, "Failed to save key", err.Error(), map[string]any{"error": err.Error()})
	}
}

func (c *CommandService) ClearServerKey(name string) CommandResult {
	err := c.inventory.ClearKey(name)
	switch {
	case err == nil:
		return successMessageCommand("server.key.clear", "server", name, "SSH key cleared", nil).withMessage("Key cleared")
	case errors.Is(err, ErrActionInProgress):
		return failedCommand(CommandOutcomeConflict, "server.key.clear", "server", name, "Server action already in progress", "wait for the active server action to finish before clearing this server key", actionStatusMeta(err))
	case errors.Is(err, ErrNotFound):
		return failedCommand(CommandOutcomeNotFound, "server.key.clear", "server", name, "Server not found", "Server not found", nil)
	default:
		return failedCommand(CommandOutcomeFailed, "server.key.clear", "server", name, "Failed to clear key", err.Error(), map[string]any{"error": err.Error()})
	}
}

func (c *CommandService) ScanHostKey(host string, port int) CommandResult {
	host = strings.TrimSpace(host)
	if host == "" {
		return failedCommand(CommandOutcomeInvalid, "hostkey.scan", "hostkey", "-", "Host is required", "host is required", nil)
	}
	port = NormalizePort(port)
	result, err := c.inventory.ScanHostKey(host, port)
	if err != nil {
		return failedCommand(CommandOutcomeRemoteError, "hostkey.scan", "hostkey", host, "Host key scan failed", fmt.Sprintf("failed to scan host key: %v", err), map[string]any{"port": port, "error": err.Error()})
	}
	return CommandResult{
		Outcome:     CommandOutcomeSuccess,
		Message:     "Host key scanned",
		HostKeyScan: &result,
		Audit: CommandAudit{
			Action:     "hostkey.scan",
			TargetType: "hostkey",
			TargetName: host,
			Status:     "success",
			Message:    "Host key scanned",
			Meta:       map[string]any{"port": port, "algorithm": result.Algorithm, "already_trusted": result.AlreadyTrusted},
		},
	}
}

func (c *CommandService) TrustHostKey(host string, port int, expectedFingerprint string) CommandResult {
	host = strings.TrimSpace(host)
	if host == "" {
		return failedCommand(CommandOutcomeInvalid, "hostkey.trust", "hostkey", "-", "Host is required", "host is required", nil)
	}
	expectedFingerprint = strings.TrimSpace(expectedFingerprint)
	if expectedFingerprint == "" {
		return failedCommand(CommandOutcomeInvalid, "hostkey.trust", "hostkey", host, "Fingerprint is required", "fingerprint_sha256 is required", nil)
	}
	port = NormalizePort(port)
	result, err := c.inventory.TrustHostKey(host, port, expectedFingerprint)
	if err != nil {
		if errors.Is(err, ErrFingerprintMismatch) {
			return failedCommand(CommandOutcomeConflict, "hostkey.trust", "hostkey", host, "Host key fingerprint mismatch", err.Error(), map[string]any{"port": port})
		}
		return failedCommand(CommandOutcomeRemoteError, "hostkey.trust", "hostkey", host, "Failed to trust host key", fmt.Sprintf("failed to trust host key: %v", err), map[string]any{"port": port, "error": err.Error()})
	}
	return CommandResult{
		Outcome:      CommandOutcomeSuccess,
		Message:      result.Message,
		HostKeyTrust: &result,
		Audit: CommandAudit{
			Action:     "hostkey.trust",
			TargetType: "hostkey",
			TargetName: host,
			Status:     "success",
			Message:    result.Message,
			Meta:       map[string]any{"port": port, "fingerprint_sha256": result.FingerprintSHA256, "already_trusted": result.AlreadyTrusted},
		},
	}
}

func (c *CommandService) ClearKnownHost(host string, port int) CommandResult {
	host = strings.TrimSpace(host)
	if host == "" {
		return failedCommand(CommandOutcomeInvalid, "hostkey.clear", "hostkey", "-", "Host is required", "host is required", nil)
	}
	port = NormalizePort(port)
	result, err := c.inventory.ClearKnownHost(host, port)
	if err != nil {
		return failedCommand(CommandOutcomeFailed, "hostkey.clear", "hostkey", host, "Failed to clear host key entry", fmt.Sprintf("failed to clear host key: %v", err), map[string]any{"port": port, "error": err.Error()})
	}
	return CommandResult{
		Outcome:      CommandOutcomeSuccess,
		Message:      result.Message,
		HostKeyClear: &result,
		Audit: CommandAudit{
			Action:     "hostkey.clear",
			TargetType: "hostkey",
			TargetName: host,
			Status:     "success",
			Message:    result.Message,
			Meta:       map[string]any{"port": port, "removed_entries": result.RemovedEntries},
		},
	}
}

func successServerCommand(action, targetName, auditMessage string, server *Server, meta map[string]any) CommandResult {
	return CommandResult{
		Outcome: CommandOutcomeSuccess,
		Message: auditMessage,
		Server:  server,
		Audit: CommandAudit{
			Action:     action,
			TargetType: "server",
			TargetName: targetName,
			Status:     "success",
			Message:    auditMessage,
			Meta:       meta,
		},
	}
}

func successMessageCommand(action, targetType, targetName, message string, meta map[string]any) CommandResult {
	return CommandResult{
		Outcome: CommandOutcomeSuccess,
		Message: message,
		Audit: CommandAudit{
			Action:     action,
			TargetType: targetType,
			TargetName: targetName,
			Status:     "success",
			Message:    message,
			Meta:       meta,
		},
	}
}

func failedCommand(outcome CommandOutcome, action, targetType, targetName, auditMessage, errMessage string, meta map[string]any) CommandResult {
	return CommandResult{
		Outcome: outcome,
		Error:   errMessage,
		Audit: CommandAudit{
			Action:     action,
			TargetType: targetType,
			TargetName: targetName,
			Status:     "failure",
			Message:    auditMessage,
			Meta:       meta,
		},
	}
}

func (r CommandResult) withMessage(message string) CommandResult {
	r.Message = message
	return r
}

func actionStatusMeta(err error) map[string]any {
	var actionErr ActionError
	if errors.As(err, &actionErr) {
		return map[string]any{"status": actionErr.Status}
	}
	return map[string]any{"status": ""}
}
