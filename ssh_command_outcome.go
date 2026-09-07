package main

import (
	"errors"
	"fmt"

	updatespkg "debian-updater/internal/updates"

	"golang.org/x/crypto/ssh"
)

func commandOutputBuffer(effect updatespkg.HostCommandEffect) updatespkg.OutputBuffer {
	limit := updatespkg.ParsedOutputLimit
	if effect.UsesPackageManagerLocks() {
		limit = updatespkg.DiagnosticOutputLimit
	}
	return updatespkg.OutputBuffer{Limit: limit}
}

func commandOutputResult(effect updatespkg.HostCommandEffect, command string, stdout, stderr *sshCommandOutputWriter, err error) (string, string, error) {
	// Only a remote exit status or explicit exec rejection establishes a known
	// command outcome. Losing the exec acknowledgement is also uncertain.
	var exit *ssh.ExitError
	if err != nil && !errors.As(err, &exit) && err.Error() != fmt.Sprintf("ssh: command %v failed", command) {
		err = classifyCommandTimeout(effect, err)
	}
	stdout.mu.Lock()
	stdoutTruncated := stdout.buffer.Truncated
	stdout.mu.Unlock()
	stderr.mu.Lock()
	stderrTruncated := stderr.buffer.Truncated
	stderr.mu.Unlock()
	if !effect.UsesPackageManagerLocks() && (stdoutTruncated || stderrTruncated) {
		err = updatespkg.NonRetryableTaggedError{Err: fmt.Errorf("SSH command output exceeds the %d-byte parsing limit", updatespkg.ParsedOutputLimit)}
	}
	return stdout.String(), stderr.String(), err
}
