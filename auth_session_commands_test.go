package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	authpkg "debian-updater/internal/auth"
)

type fakeAuthCommandAccount struct {
	setupRequired     bool
	setupRequiredErr  error
	validateUserErr   error
	validatePassErr   error
	createErr         error
	createdUsername   string
	changePasswordErr error
	changedPassword   bool
	authenticated     bool
	authenticateErr   error
	sessionCount      int
	sessionCountErr   error
	clearSessionsErr  error
	clearedSessions   bool
	deletedSessions   int64
	revokeSessionErr  error
	revokedSessionID  string
	revokeFound       bool
	clearOthersErr    error
	clearOthersToken  string
	deletedOthers     int64
	revealedIP        string
	revealFound       bool
	revealErr         error
}

func (f *fakeAuthCommandAccount) SetupRequired() (bool, error) {
	return f.setupRequired, f.setupRequiredErr
}

func (f *fakeAuthCommandAccount) ValidateUsername(string) error {
	return f.validateUserErr
}

func (f *fakeAuthCommandAccount) ValidatePasswordPolicy(string) error {
	return f.validatePassErr
}

func (f *fakeAuthCommandAccount) CreateInitialUser(username, _ string) error {
	f.createdUsername = username
	return f.createErr
}

func (f *fakeAuthCommandAccount) ChangePassword(_, _, _ string) error {
	f.changedPassword = true
	return f.changePasswordErr
}

func (f *fakeAuthCommandAccount) Authenticate(_, _ string) (bool, error) {
	return f.authenticated, f.authenticateErr
}

func (f *fakeAuthCommandAccount) CountSessions() (int, error) {
	return f.sessionCount, f.sessionCountErr
}

func (f *fakeAuthCommandAccount) ClearSessions() (int64, error) {
	f.clearedSessions = true
	return f.deletedSessions, f.clearSessionsErr
}

func (f *fakeAuthCommandAccount) RevokeSession(id string) (bool, error) {
	f.revokedSessionID = id
	return f.revokeFound, f.revokeSessionErr
}

func (f *fakeAuthCommandAccount) ClearOtherSessions(currentToken string) (int64, error) {
	f.clearOthersToken = currentToken
	return f.deletedOthers, f.clearOthersErr
}

func (f *fakeAuthCommandAccount) RevealSessionIP(string) (string, bool, error) {
	return f.revealedIP, f.revealFound, f.revealErr
}

type fakeAuthCommandSession struct {
	stagedUsername string
	stageErr       error
	destroyErr     error
	destroyed      bool
}

type fakeAuthPasswordAttemptPolicy struct {
	limited  bool
	failures []string
}

func (f *fakeAuthPasswordAttemptPolicy) Limited(string) bool {
	return f.limited
}

func (f *fakeAuthPasswordAttemptPolicy) RecordFailure(key string) {
	f.failures = append(f.failures, key)
}

func (f *fakeAuthCommandSession) Stage(_ context.Context, username string) error {
	f.stagedUsername = username
	return f.stageErr
}

func (f *fakeAuthCommandSession) Destroy(context.Context) error {
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.destroyed = true
	return nil
}

func TestAuthSessionCommandsSetupOutcomes(t *testing.T) {
	validationErr := errors.New("username is invalid")
	writeErr := errors.New("database unavailable")
	sessionErr := errors.New("session unavailable")
	tests := []struct {
		name      string
		account   fakeAuthCommandAccount
		session   fakeAuthCommandSession
		wantKind  authSetupOutcomeKind
		wantError string
	}{
		{
			name:     "success stages authenticated session",
			account:  fakeAuthCommandAccount{setupRequired: true},
			wantKind: authSetupSucceeded,
		},
		{
			name:      "invalid username",
			account:   fakeAuthCommandAccount{setupRequired: true, validateUserErr: validationErr},
			wantKind:  authSetupInvalid,
			wantError: validationErr.Error(),
		},
		{
			name:     "setup already complete",
			account:  fakeAuthCommandAccount{},
			wantKind: authSetupAlreadyCompleted,
		},
		{
			name:     "concurrent setup conflict",
			account:  fakeAuthCommandAccount{setupRequired: true, createErr: authpkg.ErrSetupAlreadyCompleted},
			wantKind: authSetupAlreadyCompleted,
		},
		{
			name:     "setup state read fails",
			account:  fakeAuthCommandAccount{setupRequiredErr: writeErr},
			wantKind: authSetupStateFailed,
		},
		{
			name:     "account write fails",
			account:  fakeAuthCommandAccount{setupRequired: true, createErr: writeErr},
			wantKind: authSetupAccountWriteFailed,
		},
		{
			name:     "account committed before session failure",
			account:  fakeAuthCommandAccount{setupRequired: true},
			session:  fakeAuthCommandSession{stageErr: sessionErr},
			wantKind: authSetupAccountCreatedSessionFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			audits := make([]authSessionCommandAuditRecord, 0, 1)
			module := newAuthSessionCommandsWithDeps(authSessionCommandDeps{
				Account: &tt.account,
				Session: &tt.session,
				RecordAudit: func(record authSessionCommandAuditRecord) error {
					audits = append(audits, record)
					return nil
				},
			})

			outcome := module.Setup(context.Background(), authSetupCommand{
				Username: " admin ",
				Password: "StrongPass123",
				ClientIP: "192.0.2.10",
			})

			if outcome.Kind != tt.wantKind {
				t.Fatalf("Setup() kind = %q, want %q", outcome.Kind, tt.wantKind)
			}
			if outcome.PublicError != tt.wantError {
				t.Fatalf("Setup() public error = %q, want %q", outcome.PublicError, tt.wantError)
			}
			if tt.wantKind == authSetupSucceeded {
				if tt.account.createdUsername != "admin" || tt.session.stagedUsername != "admin" {
					t.Fatalf("setup username flow = created %q staged %q", tt.account.createdUsername, tt.session.stagedUsername)
				}
				if len(audits) != 1 || audits[0].Status != "success" {
					t.Fatalf("success audits = %+v", audits)
				}
			}
			if tt.wantKind == authSetupAccountCreatedSessionFailed {
				if tt.account.createdUsername != "admin" {
					t.Fatalf("account was not committed before session failure")
				}
				if len(audits) != 1 || audits[0].Status != "failure" || audits[0].Meta["account_created"] != true {
					t.Fatalf("partial failure audits = %+v", audits)
				}
			}
		})
	}
}

func TestAuthSessionCommandsSetupIgnoresAuditFailure(t *testing.T) {
	module := newAuthSessionCommandsWithDeps(authSessionCommandDeps{
		Account: &fakeAuthCommandAccount{setupRequired: true},
		Session: &fakeAuthCommandSession{},
		RecordAudit: func(authSessionCommandAuditRecord) error {
			return errors.New("audit unavailable")
		},
	})

	outcome := module.Setup(context.Background(), authSetupCommand{Username: "admin", Password: "StrongPass123"})
	if outcome.Kind != authSetupSucceeded {
		t.Fatalf("Setup() kind = %q, want %q", outcome.Kind, authSetupSucceeded)
	}
}

func TestAuthSessionCommandsChangePasswordOutcomes(t *testing.T) {
	writeErr := errors.New("database unavailable")
	tests := []struct {
		name         string
		accountErr   error
		limited      bool
		wantKind     authPasswordOutcomeKind
		wantError    string
		wantFailures int
	}{
		{name: "success", wantKind: authPasswordSucceeded},
		{name: "rate limited", limited: true, wantKind: authPasswordRateLimited, wantError: "too many password change attempts"},
		{name: "password mismatch", accountErr: authpkg.ErrPasswordMismatch, wantKind: authPasswordInvalid, wantError: authpkg.ErrPasswordMismatch.Error(), wantFailures: 1},
		{name: "current password rejected", accountErr: authpkg.ErrCurrentPasswordInvalid, wantKind: authPasswordCurrentRejected, wantError: authpkg.ErrCurrentPasswordInvalid.Error(), wantFailures: 1},
		{name: "password policy rejected", accountErr: authpkg.NewValidationError("password policy rejected"), wantKind: authPasswordInvalid, wantError: "password policy rejected", wantFailures: 1},
		{name: "setup required", accountErr: authpkg.ErrSetupRequired, wantKind: authPasswordSetupRequired, wantError: "setup required", wantFailures: 1},
		{name: "write failed", accountErr: writeErr, wantKind: authPasswordWriteFailed, wantError: "failed to change password", wantFailures: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &fakeAuthCommandAccount{changePasswordErr: tt.accountErr}
			policy := &fakeAuthPasswordAttemptPolicy{limited: tt.limited}
			audits := make([]authSessionCommandAuditRecord, 0, 1)
			module := newAuthSessionCommandsWithDeps(authSessionCommandDeps{
				Account:        account,
				Session:        &fakeAuthCommandSession{},
				PasswordPolicy: policy,
				RecordAudit: func(record authSessionCommandAuditRecord) error {
					audits = append(audits, record)
					return nil
				},
			})

			outcome := module.ChangePassword(context.Background(), authPasswordCommand{
				Actor:           "admin",
				ClientIP:        "192.0.2.10",
				CurrentPassword: "CurrentPass123",
				NewPassword:     "NewStrongPass123",
				ConfirmPassword: "NewStrongPass123",
			})

			if outcome.Kind != tt.wantKind || outcome.PublicError != tt.wantError {
				t.Fatalf("ChangePassword() = kind %q error %q, want kind %q error %q", outcome.Kind, outcome.PublicError, tt.wantKind, tt.wantError)
			}
			if len(policy.failures) != tt.wantFailures {
				t.Fatalf("recorded failures = %d, want %d", len(policy.failures), tt.wantFailures)
			}
			if tt.wantKind == authPasswordSucceeded {
				if !account.changedPassword || len(audits) != 1 || audits[0].Status != "success" {
					t.Fatalf("success state account=%v audits=%+v", account.changedPassword, audits)
				}
			}
			if tt.wantKind == authPasswordWriteFailed {
				if len(audits) != 1 || audits[0].Meta["failure_kind"] != "write_failed" {
					t.Fatalf("write failure audit = %+v", audits)
				}
				if audits[0].Meta["error"] == writeErr.Error() {
					t.Fatalf("write failure leaked infrastructure error in audit metadata")
				}
			}
		})
	}
}

func TestAuthSessionCommandsPasswordChangeSessionOutcomes(t *testing.T) {
	t.Run("preserves every session when invalidation is not requested", func(t *testing.T) {
		account := &fakeAuthCommandAccount{sessionCount: 3}
		audits := make([]authSessionCommandAuditRecord, 0, 1)
		module := newAuthSessionCommandsWithDeps(authSessionCommandDeps{
			Account: account,
			RecordAudit: func(record authSessionCommandAuditRecord) error {
				audits = append(audits, record)
				return nil
			},
		})

		outcome := module.ChangePassword(context.Background(), authPasswordCommand{
			Actor: "admin", CurrentToken: "current-token",
			CurrentPassword: "CurrentPass123", NewPassword: "NewStrongPass123", ConfirmPassword: "NewStrongPass123",
		})

		if outcome.Kind != authPasswordSucceeded || outcome.InvalidationRequested || outcome.InvalidatedSessions != 0 || outcome.PreservedSessions != 3 || !outcome.CurrentSessionPreserved {
			t.Fatalf("ChangePassword(preserve) = %+v", outcome)
		}
		if account.clearOthersToken != "" {
			t.Fatalf("ChangePassword(preserve) cleared sessions with token %q", account.clearOthersToken)
		}
		if len(audits) != 1 || audits[0].Meta["preserved_sessions"] != 3 || audits[0].Meta["invalidated_sessions"] != int64(0) {
			t.Fatalf("ChangePassword(preserve) audits = %+v", audits)
		}
	})

	t.Run("invalidates only other sessions and preserves current", func(t *testing.T) {
		account := &fakeAuthCommandAccount{sessionCount: 3, deletedOthers: 2}
		module := newAuthSessionCommandsWithDeps(authSessionCommandDeps{Account: account})

		outcome := module.ChangePassword(context.Background(), authPasswordCommand{
			Actor: "admin", CurrentToken: "current-token",
			CurrentPassword: "CurrentPass123", NewPassword: "NewStrongPass123", ConfirmPassword: "NewStrongPass123",
			InvalidateOtherSessions: true,
		})

		if outcome.Kind != authPasswordSucceeded || !outcome.InvalidationRequested || outcome.InvalidatedSessions != 2 || outcome.PreservedSessions != 1 || !outcome.CurrentSessionPreserved {
			t.Fatalf("ChangePassword(invalidate) = %+v", outcome)
		}
		if account.clearOthersToken != "current-token" {
			t.Fatalf("ChangePassword(invalidate) token = %q", account.clearOthersToken)
		}
	})

	t.Run("reports password success with session invalidation failure", func(t *testing.T) {
		clearErr := errors.New("session store unavailable")
		account := &fakeAuthCommandAccount{sessionCount: 3, clearOthersErr: clearErr}
		audits := make([]authSessionCommandAuditRecord, 0, 1)
		module := newAuthSessionCommandsWithDeps(authSessionCommandDeps{
			Account: account,
			RecordAudit: func(record authSessionCommandAuditRecord) error {
				audits = append(audits, record)
				return nil
			},
		})

		outcome := module.ChangePassword(context.Background(), authPasswordCommand{
			Actor: "admin", CurrentToken: "current-token",
			CurrentPassword: "CurrentPass123", NewPassword: "NewStrongPass123", ConfirmPassword: "NewStrongPass123",
			InvalidateOtherSessions: true,
		})

		if outcome.Kind != authPasswordInvalidationFailed || !account.changedPassword || outcome.InvalidatedSessions != 0 || outcome.PreservedSessions != 3 || !outcome.CurrentSessionPreserved {
			t.Fatalf("ChangePassword(partial) = %+v account=%+v", outcome, account)
		}
		if len(audits) != 1 || audits[0].Status != "failure" || audits[0].Meta["password_changed"] != true || audits[0].Meta["failure_kind"] != "session_invalidation_failed" {
			t.Fatalf("ChangePassword(partial) audits = %+v", audits)
		}
		if _, leaked := audits[0].Meta["error"]; leaked {
			t.Fatalf("ChangePassword(partial) leaked infrastructure error: %+v", audits[0].Meta)
		}
	})
}

func TestAuthSessionCommandsLoginOutcomes(t *testing.T) {
	infrastructureErr := errors.New("database unavailable")
	sessionErr := errors.New("session unavailable")
	tests := []struct {
		name       string
		account    fakeAuthCommandAccount
		session    fakeAuthCommandSession
		wantKind   authLoginOutcomeKind
		wantAudits int
	}{
		{name: "success stages session", account: fakeAuthCommandAccount{authenticated: true}, wantKind: authLoginSucceeded, wantAudits: 1},
		{name: "setup required", account: fakeAuthCommandAccount{setupRequired: true}, wantKind: authLoginSetupRequired},
		{name: "setup state failed", account: fakeAuthCommandAccount{setupRequiredErr: infrastructureErr}, wantKind: authLoginSetupStateFailed},
		{name: "invalid credentials", account: fakeAuthCommandAccount{}, wantKind: authLoginInvalidCredentials, wantAudits: 1},
		{name: "authentication failed", account: fakeAuthCommandAccount{authenticateErr: infrastructureErr}, wantKind: authLoginAuthenticationFailed},
		{name: "session stage failed", account: fakeAuthCommandAccount{authenticated: true}, session: fakeAuthCommandSession{stageErr: sessionErr}, wantKind: authLoginSessionFailed, wantAudits: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			audits := make([]authSessionCommandAuditRecord, 0, 1)
			module := newAuthSessionCommandsWithDeps(authSessionCommandDeps{
				Account: &tt.account,
				Session: &tt.session,
				RecordAudit: func(record authSessionCommandAuditRecord) error {
					audits = append(audits, record)
					return nil
				},
			})

			outcome := module.Login(context.Background(), authLoginCommand{
				Username: " admin ",
				Password: "StrongPass123",
				ClientIP: "192.0.2.10",
			})

			if outcome.Kind != tt.wantKind {
				t.Fatalf("Login() kind = %q, want %q", outcome.Kind, tt.wantKind)
			}
			if len(audits) != tt.wantAudits {
				t.Fatalf("Login() audits = %+v, want %d", audits, tt.wantAudits)
			}
			if tt.wantKind == authLoginSucceeded && tt.session.stagedUsername != "admin" {
				t.Fatalf("staged username = %q, want admin", tt.session.stagedUsername)
			}
			if tt.wantKind == authLoginInvalidCredentials && audits[0].Actor != "unknown" {
				t.Fatalf("invalid credential audit actor = %q, want unknown", audits[0].Actor)
			}
		})
	}
}

func TestAuthSessionCommandsLogoutOutcomes(t *testing.T) {
	tests := []struct {
		name     string
		session  fakeAuthCommandSession
		wantKind authLogoutOutcomeKind
	}{
		{name: "success", wantKind: authLogoutSucceeded},
		{name: "session failure", session: fakeAuthCommandSession{destroyErr: errors.New("store unavailable")}, wantKind: authLogoutSessionFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			audits := make([]authSessionCommandAuditRecord, 0, 1)
			module := newAuthSessionCommandsWithDeps(authSessionCommandDeps{
				Account: &fakeAuthCommandAccount{},
				Session: &tt.session,
				RecordAudit: func(record authSessionCommandAuditRecord) error {
					audits = append(audits, record)
					return nil
				},
			})

			outcome := module.Logout(context.Background(), authLogoutCommand{Actor: "admin", ClientIP: "192.0.2.10"})
			if outcome.Kind != tt.wantKind {
				t.Fatalf("Logout() kind = %q, want %q", outcome.Kind, tt.wantKind)
			}
			if len(audits) != 1 || audits[0].Status != map[bool]string{true: "success", false: "failure"}[tt.wantKind == authLogoutSucceeded] {
				t.Fatalf("Logout() audits = %+v", audits)
			}
			if tt.wantKind == authLogoutSucceeded && !tt.session.destroyed {
				t.Fatalf("Logout() reported success before session destruction")
			}
		})
	}
}

func TestAuthSessionCommandsClearSessionsOutcomes(t *testing.T) {
	clearErr := errors.New("database unavailable")
	destroyErr := errors.New("session store unavailable")
	tests := []struct {
		name        string
		account     fakeAuthCommandAccount
		session     fakeAuthCommandSession
		wantKind    authClearSessionsOutcomeKind
		wantDeleted int64
	}{
		{name: "success", account: fakeAuthCommandAccount{deletedSessions: 3}, wantKind: authClearSessionsSucceeded, wantDeleted: 4},
		{name: "current session destroy fails", session: fakeAuthCommandSession{destroyErr: destroyErr}, wantKind: authClearSessionsCurrentSessionFailed},
		{name: "current session destroyed before clear fails", account: fakeAuthCommandAccount{clearSessionsErr: clearErr}, wantKind: authClearSessionsCurrentDestroyedClearFailed, wantDeleted: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			audits := make([]authSessionCommandAuditRecord, 0, 1)
			module := newAuthSessionCommandsWithDeps(authSessionCommandDeps{
				Account: &tt.account,
				Session: &tt.session,
				RecordAudit: func(record authSessionCommandAuditRecord) error {
					audits = append(audits, record)
					return nil
				},
			})

			outcome := module.ClearSessions(context.Background(), authClearSessionsCommand{Actor: "admin", ClientIP: "192.0.2.10"})
			if outcome.Kind != tt.wantKind || outcome.DeletedSessions != tt.wantDeleted {
				t.Fatalf("ClearSessions() = kind %q deleted %d, want kind %q deleted %d", outcome.Kind, outcome.DeletedSessions, tt.wantKind, tt.wantDeleted)
			}
			if len(audits) != 1 {
				t.Fatalf("ClearSessions() audits = %+v", audits)
			}
			if tt.wantKind == authClearSessionsCurrentSessionFailed && tt.account.clearedSessions {
				t.Fatalf("ClearSessions() cleared persisted sessions after current session destruction failed")
			}
			if tt.wantKind == authClearSessionsCurrentDestroyedClearFailed && !tt.session.destroyed {
				t.Fatalf("ClearSessions() partial outcome without destroying current session")
			}
		})
	}
}

func TestAuthSessionCommandsRevokeAndClearOthers(t *testing.T) {
	t.Run("revokes one non-current session", func(t *testing.T) {
		account := &fakeAuthCommandAccount{revokeFound: true}
		session := &fakeAuthCommandSession{}
		module := newAuthSessionCommandsWithDeps(authSessionCommandDeps{Account: account, Session: session})
		outcome := module.RevokeSession(context.Background(), authRevokeSessionCommand{
			Actor: "admin", ClientIP: "192.0.2.10", SessionID: "safe-id",
		})
		if outcome.Kind != authRevokeSessionSucceeded || account.revokedSessionID != "safe-id" || session.destroyed {
			t.Fatalf("RevokeSession() outcome=%+v account=%+v session=%+v", outcome, account, session)
		}
	})

	t.Run("revoking current session destroys browser session", func(t *testing.T) {
		account := &fakeAuthCommandAccount{revokeFound: true}
		session := &fakeAuthCommandSession{}
		module := newAuthSessionCommandsWithDeps(authSessionCommandDeps{Account: account, Session: session})
		outcome := module.RevokeSession(context.Background(), authRevokeSessionCommand{
			Actor: "admin", SessionID: "safe-id", Current: true,
		})
		if outcome.Kind != authRevokeSessionSucceeded || !session.destroyed {
			t.Fatalf("RevokeSession(current) outcome=%+v session=%+v", outcome, session)
		}
	})

	t.Run("clears other sessions and preserves current", func(t *testing.T) {
		account := &fakeAuthCommandAccount{deletedOthers: 2}
		session := &fakeAuthCommandSession{}
		module := newAuthSessionCommandsWithDeps(authSessionCommandDeps{Account: account, Session: session})
		outcome := module.ClearOtherSessions(context.Background(), authClearOtherSessionsCommand{
			Actor: "admin", CurrentToken: "current-token",
		})
		if outcome.Kind != authClearOtherSessionsSucceeded || outcome.DeletedSessions != 2 || account.clearOthersToken != "current-token" || session.destroyed {
			t.Fatalf("ClearOtherSessions() outcome=%+v account=%+v session=%+v", outcome, account, session)
		}
	})
}

func TestAuthSessionCommandsRevealIPAuditsWithoutDisclosingTargetIP(t *testing.T) {
	const revealedIP = "203.0.113.84"
	account := &fakeAuthCommandAccount{
		authenticated: true,
		revealedIP:    revealedIP,
		revealFound:   true,
	}
	audits := make([]authSessionCommandAuditRecord, 0, 1)
	module := newAuthSessionCommandsWithDeps(authSessionCommandDeps{
		Account: account,
		RecordAudit: func(record authSessionCommandAuditRecord) error {
			audits = append(audits, record)
			return nil
		},
	})

	outcome := module.RevealSessionIP(context.Background(), authRevealSessionIPCommand{
		Actor:           "admin",
		ClientIP:        "192.0.2.10",
		SessionID:       "session-42",
		CurrentPassword: "StrongPass123",
		AttemptKey:      "attempt-key",
	})

	if outcome.Kind != authRevealSessionIPSucceeded || outcome.IP != revealedIP {
		t.Fatalf("RevealSessionIP() = %+v", outcome)
	}
	if len(audits) != 1 {
		t.Fatalf("RevealSessionIP() audits = %+v", audits)
	}
	audit := audits[0]
	if audit.Action != "auth.session.ip_reveal" || audit.Status != "success" || audit.TargetName != "session-42" {
		t.Fatalf("RevealSessionIP() audit = %+v", audit)
	}
	if strings.Contains(audit.Message, revealedIP) || strings.Contains(audit.TargetName, revealedIP) {
		t.Fatalf("RevealSessionIP() audit disclosed target IP: %+v", audit)
	}
	if len(audit.Meta) != 0 {
		t.Fatalf("RevealSessionIP() audit metadata should not include target details: %+v", audit.Meta)
	}
}
