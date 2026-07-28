package main

import (
	"context"
	"errors"
	"log"
	"strings"

	authpkg "debian-updater/internal/auth"

	"github.com/alexedwards/scs/v2"
)

type authSessionAccount interface {
	SetupRequired() (bool, error)
	ValidateUsername(string) error
	ValidatePasswordPolicy(string) error
	CreateInitialUser(username, password string) error
	ChangePassword(currentPassword, newPassword, confirmPassword string) error
	Authenticate(username, password string) (bool, error)
	CountSessions() (int, error)
	ClearSessions() (int64, error)
	RevokeSession(id string) (bool, error)
	ClearOtherSessions(currentToken string) (int64, error)
	RevealSessionIP(id string) (string, bool, error)
}

type authPasswordAttemptPolicy interface {
	Limited(string) bool
	RecordFailure(string)
}

type allowAllAuthPasswordAttempts struct{}

func (allowAllAuthPasswordAttempts) Limited(string) bool  { return false }
func (allowAllAuthPasswordAttempts) RecordFailure(string) {}

type authSessionLifecycle interface {
	Stage(context.Context, string) error
	Destroy(context.Context) error
}

type scsAuthSessionLifecycle struct {
	current func() *scs.SessionManager
}

func (s scsAuthSessionLifecycle) Stage(ctx context.Context, username string) error {
	sm := s.manager()
	if sm == nil {
		return errors.New("session manager not initialized")
	}
	if err := sm.RenewToken(ctx); err != nil {
		return err
	}
	sm.Put(ctx, authSessionUserKey, username)
	return nil
}

func (s scsAuthSessionLifecycle) Destroy(ctx context.Context) error {
	sm := s.manager()
	if sm == nil {
		return errors.New("session manager not initialized")
	}
	return sm.Destroy(ctx)
}

func (s scsAuthSessionLifecycle) manager() *scs.SessionManager {
	if s.current == nil {
		return nil
	}
	return s.current()
}

type authSessionCommandAuditRecord struct {
	Actor      string
	ClientIP   string
	Action     string
	TargetType string
	TargetName string
	Status     string
	Message    string
	Meta       map[string]any
}

type authSessionCommandDeps struct {
	Account        authSessionAccount
	Session        authSessionLifecycle
	PasswordPolicy authPasswordAttemptPolicy
	RecordAudit    func(authSessionCommandAuditRecord) error
	Logf           func(string, ...any)
}

type authSessionCommands struct {
	deps authSessionCommandDeps
}

type authSetupOutcomeKind string

const (
	authSetupSucceeded                   authSetupOutcomeKind = "succeeded"
	authSetupInvalid                     authSetupOutcomeKind = "invalid"
	authSetupAlreadyCompleted            authSetupOutcomeKind = "already_completed"
	authSetupStateFailed                 authSetupOutcomeKind = "setup_state_failed"
	authSetupAccountWriteFailed          authSetupOutcomeKind = "account_write_failed"
	authSetupAccountCreatedSessionFailed authSetupOutcomeKind = "account_created_session_failed"
)

type authSetupCommand struct {
	Username string
	Password string
	ClientIP string
}

type authSetupOutcome struct {
	Kind        authSetupOutcomeKind
	Username    string
	PublicError string
	Err         error
}

type authPasswordOutcomeKind string

const (
	authPasswordSucceeded          authPasswordOutcomeKind = "succeeded"
	authPasswordInvalid            authPasswordOutcomeKind = "invalid"
	authPasswordCurrentRejected    authPasswordOutcomeKind = "current_password_rejected"
	authPasswordSetupRequired      authPasswordOutcomeKind = "setup_required"
	authPasswordRateLimited        authPasswordOutcomeKind = "rate_limited"
	authPasswordWriteFailed        authPasswordOutcomeKind = "write_failed"
	authPasswordInvalidationFailed authPasswordOutcomeKind = "session_invalidation_failed"
)

type authPasswordCommand struct {
	Actor                   string
	ClientIP                string
	CurrentToken            string
	CurrentPassword         string
	NewPassword             string
	ConfirmPassword         string
	InvalidateOtherSessions bool
}

type authPasswordOutcome struct {
	Kind                    authPasswordOutcomeKind
	PublicError             string
	Err                     error
	InvalidationRequested   bool
	InvalidatedSessions     int64
	PreservedSessions       int
	CurrentSessionPreserved bool
}

type authLoginOutcomeKind string

const (
	authLoginSucceeded            authLoginOutcomeKind = "succeeded"
	authLoginSetupRequired        authLoginOutcomeKind = "setup_required"
	authLoginSetupStateFailed     authLoginOutcomeKind = "setup_state_failed"
	authLoginInvalidCredentials   authLoginOutcomeKind = "invalid_credentials"
	authLoginAuthenticationFailed authLoginOutcomeKind = "authentication_failed"
	authLoginSessionFailed        authLoginOutcomeKind = "session_failed"
)

type authLoginCommand struct {
	Username string
	Password string
	ClientIP string
}

type authLoginOutcome struct {
	Kind     authLoginOutcomeKind
	Username string
	Err      error
}

type authLogoutOutcomeKind string

const (
	authLogoutSucceeded     authLogoutOutcomeKind = "succeeded"
	authLogoutSessionFailed authLogoutOutcomeKind = "session_failed"
)

type authLogoutCommand struct {
	Actor    string
	ClientIP string
}

type authLogoutOutcome struct {
	Kind authLogoutOutcomeKind
	Err  error
}

type authClearSessionsOutcomeKind string

const (
	authClearSessionsSucceeded                   authClearSessionsOutcomeKind = "succeeded"
	authClearSessionsCurrentSessionFailed        authClearSessionsOutcomeKind = "current_session_failed"
	authClearSessionsCurrentDestroyedClearFailed authClearSessionsOutcomeKind = "current_destroyed_clear_failed"
)

type authClearSessionsCommand struct {
	Actor    string
	ClientIP string
}

type authClearSessionsOutcome struct {
	Kind            authClearSessionsOutcomeKind
	DeletedSessions int64
	Err             error
}

type authRevokeSessionOutcomeKind string

const (
	authRevokeSessionSucceeded       authRevokeSessionOutcomeKind = "succeeded"
	authRevokeSessionNotFound        authRevokeSessionOutcomeKind = "not_found"
	authRevokeSessionWriteFailed     authRevokeSessionOutcomeKind = "write_failed"
	authRevokeSessionDestroyedFailed authRevokeSessionOutcomeKind = "current_destroy_failed"
)

type authRevokeSessionCommand struct {
	Actor     string
	ClientIP  string
	SessionID string
	Current   bool
}

type authRevokeSessionOutcome struct {
	Kind    authRevokeSessionOutcomeKind
	Current bool
	Err     error
}

type authClearOtherSessionsOutcomeKind string

const (
	authClearOtherSessionsSucceeded   authClearOtherSessionsOutcomeKind = "succeeded"
	authClearOtherSessionsWriteFailed authClearOtherSessionsOutcomeKind = "write_failed"
)

type authClearOtherSessionsCommand struct {
	Actor        string
	ClientIP     string
	CurrentToken string
}

type authClearOtherSessionsOutcome struct {
	Kind            authClearOtherSessionsOutcomeKind
	DeletedSessions int64
	Err             error
}

type authRevealSessionIPOutcomeKind string

const (
	authRevealSessionIPSucceeded   authRevealSessionIPOutcomeKind = "succeeded"
	authRevealSessionIPInvalid     authRevealSessionIPOutcomeKind = "invalid_credentials"
	authRevealSessionIPRateLimited authRevealSessionIPOutcomeKind = "rate_limited"
	authRevealSessionIPNotFound    authRevealSessionIPOutcomeKind = "not_found"
	authRevealSessionIPFailed      authRevealSessionIPOutcomeKind = "failed"
)

type authRevealSessionIPCommand struct {
	Actor           string
	ClientIP        string
	SessionID       string
	CurrentPassword string
	AttemptKey      string
}

type authRevealSessionIPOutcome struct {
	Kind authRevealSessionIPOutcomeKind
	IP   string
	Err  error
}

func authSessionCommandDepsFromAppDeps(deps AppDeps) authSessionCommandDeps {
	return authSessionCommandDeps{
		Account:        deps.AuthService,
		Session:        scsAuthSessionLifecycle{current: deps.CurrentSessionManager},
		PasswordPolicy: deps.PasswordChangeRateLimiter,
		RecordAudit: func(record authSessionCommandAuditRecord) error {
			if deps.AuditService != nil {
				return deps.AuditService.Record(record.Actor, record.ClientIP, record.Action, record.TargetType, record.TargetName, record.Status, record.Message, record.Meta)
			}
			auditWithActor(record.Actor, record.ClientIP, record.Action, record.TargetType, record.TargetName, record.Status, record.Message, record.Meta)
			return nil
		},
		Logf: log.Printf,
	}
}

func newAuthSessionCommandsWithDeps(deps authSessionCommandDeps) *authSessionCommands {
	return &authSessionCommands{deps: deps.withDefaults()}
}

func (deps authSessionCommandDeps) withDefaults() authSessionCommandDeps {
	if deps.Account == nil {
		deps.Account = defaultAuthService()
	}
	if deps.Session == nil {
		deps.Session = scsAuthSessionLifecycle{current: currentSessionManager}
	}
	if deps.PasswordPolicy == nil {
		deps.PasswordPolicy = allowAllAuthPasswordAttempts{}
	}
	if deps.RecordAudit == nil {
		deps.RecordAudit = func(record authSessionCommandAuditRecord) error {
			auditWithActor(record.Actor, record.ClientIP, record.Action, record.TargetType, record.TargetName, record.Status, record.Message, record.Meta)
			return nil
		}
	}
	if deps.Logf == nil {
		deps.Logf = log.Printf
	}
	return deps
}

func (m *authSessionCommands) Setup(ctx context.Context, cmd authSetupCommand) authSetupOutcome {
	deps := m.deps.withDefaults()
	if ctx == nil {
		ctx = context.Background()
	}
	cmd.Username = strings.TrimSpace(cmd.Username)
	cmd.ClientIP = strings.TrimSpace(cmd.ClientIP)

	required, err := deps.Account.SetupRequired()
	if err != nil {
		return authSetupOutcome{Kind: authSetupStateFailed, Err: err}
	}
	if !required {
		return authSetupOutcome{Kind: authSetupAlreadyCompleted}
	}
	if err := deps.Account.ValidateUsername(cmd.Username); err != nil {
		return authSetupOutcome{Kind: authSetupInvalid, PublicError: err.Error(), Err: err}
	}
	if err := deps.Account.ValidatePasswordPolicy(cmd.Password); err != nil {
		return authSetupOutcome{Kind: authSetupInvalid, PublicError: err.Error(), Err: err}
	}
	if err := deps.Account.CreateInitialUser(cmd.Username, cmd.Password); err != nil {
		if errors.Is(err, authpkg.ErrSetupAlreadyCompleted) {
			return authSetupOutcome{Kind: authSetupAlreadyCompleted, Err: err}
		}
		return authSetupOutcome{Kind: authSetupAccountWriteFailed, Err: err}
	}
	if err := deps.Session.Stage(ctx, cmd.Username); err != nil {
		m.recordAudit(authSessionCommandAuditRecord{
			Actor:      cmd.Username,
			ClientIP:   cmd.ClientIP,
			Action:     "auth.setup",
			TargetType: "auth_user",
			TargetName: cmd.Username,
			Status:     "failure",
			Message:    "Initial admin user created but session initialization failed",
			Meta:       map[string]any{"account_created": true, "failure_kind": "session_stage_failed"},
		})
		return authSetupOutcome{Kind: authSetupAccountCreatedSessionFailed, Username: cmd.Username, Err: err}
	}

	m.recordAudit(authSessionCommandAuditRecord{
		Actor:      cmd.Username,
		ClientIP:   cmd.ClientIP,
		Action:     "auth.setup",
		TargetType: "auth_user",
		TargetName: cmd.Username,
		Status:     "success",
		Message:    "Initial admin user created",
	})
	return authSetupOutcome{Kind: authSetupSucceeded, Username: cmd.Username}
}

func (m *authSessionCommands) ChangePassword(_ context.Context, cmd authPasswordCommand) authPasswordOutcome {
	deps := m.deps.withDefaults()
	cmd.Actor = strings.TrimSpace(cmd.Actor)
	cmd.ClientIP = strings.TrimSpace(cmd.ClientIP)
	key := authPasswordChangeRateKey(cmd.ClientIP, cmd.Actor)
	if deps.PasswordPolicy.Limited(key) {
		return authPasswordOutcome{Kind: authPasswordRateLimited, PublicError: "too many password change attempts"}
	}

	activeSessions, err := deps.Account.CountSessions()
	if err != nil {
		deps.PasswordPolicy.RecordFailure(key)
		m.recordAudit(authSessionCommandAuditRecord{
			Actor:      cmd.Actor,
			ClientIP:   cmd.ClientIP,
			Action:     "auth.password.change",
			TargetType: "auth_user",
			TargetName: cmd.Actor,
			Status:     "failure",
			Message:    "Password change failed",
			Meta:       map[string]any{"failure_kind": "session_count_failed"},
		})
		return authPasswordOutcome{Kind: authPasswordWriteFailed, PublicError: "failed to change password", Err: err}
	}

	err = deps.Account.ChangePassword(cmd.CurrentPassword, cmd.NewPassword, cmd.ConfirmPassword)
	if err == nil {
		outcome := authPasswordOutcome{
			Kind:                    authPasswordSucceeded,
			InvalidationRequested:   cmd.InvalidateOtherSessions,
			PreservedSessions:       max(1, activeSessions),
			CurrentSessionPreserved: true,
		}
		if cmd.InvalidateOtherSessions {
			deleted, clearErr := deps.Account.ClearOtherSessions(cmd.CurrentToken)
			if clearErr != nil {
				m.recordAudit(authSessionCommandAuditRecord{
					Actor:      cmd.Actor,
					ClientIP:   cmd.ClientIP,
					Action:     "auth.password.change",
					TargetType: "auth_user",
					TargetName: cmd.Actor,
					Status:     "failure",
					Message:    "Password changed but other session invalidation failed",
					Meta: map[string]any{
						"failure_kind":              "session_invalidation_failed",
						"password_changed":          true,
						"invalidation_requested":    true,
						"invalidated_sessions":      0,
						"preserved_sessions":        max(1, activeSessions),
						"current_session_preserved": true,
					},
				})
				outcome.Kind = authPasswordInvalidationFailed
				outcome.PublicError = "password changed, but other sessions could not be invalidated"
				outcome.Err = clearErr
				return outcome
			}
			outcome.InvalidatedSessions = deleted
			outcome.PreservedSessions = max(1, activeSessions-int(deleted))
		}
		m.recordAudit(authSessionCommandAuditRecord{
			Actor:      cmd.Actor,
			ClientIP:   cmd.ClientIP,
			Action:     "auth.password.change",
			TargetType: "auth_user",
			TargetName: cmd.Actor,
			Status:     "success",
			Message:    "Password changed",
			Meta: map[string]any{
				"invalidation_requested":    outcome.InvalidationRequested,
				"invalidated_sessions":      outcome.InvalidatedSessions,
				"preserved_sessions":        outcome.PreservedSessions,
				"current_session_preserved": outcome.CurrentSessionPreserved,
			},
		})
		return outcome
	}

	deps.PasswordPolicy.RecordFailure(key)
	outcome := authPasswordOutcome{Kind: authPasswordWriteFailed, PublicError: "failed to change password", Err: err}
	failureKind := "write_failed"
	switch {
	case errors.Is(err, authpkg.ErrPasswordMismatch):
		outcome.Kind = authPasswordInvalid
		outcome.PublicError = err.Error()
		failureKind = "password_mismatch"
	case errors.Is(err, authpkg.ErrCurrentPasswordInvalid):
		outcome.Kind = authPasswordCurrentRejected
		outcome.PublicError = err.Error()
		failureKind = "current_password_rejected"
	case errors.Is(err, authpkg.ErrSetupRequired):
		outcome.Kind = authPasswordSetupRequired
		outcome.PublicError = "setup required"
		failureKind = "setup_required"
	case authpkg.IsValidationError(err):
		outcome.Kind = authPasswordInvalid
		outcome.PublicError = err.Error()
		failureKind = "validation_failed"
	}
	m.recordAudit(authSessionCommandAuditRecord{
		Actor:      cmd.Actor,
		ClientIP:   cmd.ClientIP,
		Action:     "auth.password.change",
		TargetType: "auth_user",
		TargetName: cmd.Actor,
		Status:     "failure",
		Message:    "Password change failed",
		Meta:       map[string]any{"failure_kind": failureKind},
	})
	return outcome
}

func (m *authSessionCommands) Login(ctx context.Context, cmd authLoginCommand) authLoginOutcome {
	deps := m.deps.withDefaults()
	if ctx == nil {
		ctx = context.Background()
	}
	cmd.Username = strings.TrimSpace(cmd.Username)
	cmd.ClientIP = strings.TrimSpace(cmd.ClientIP)

	required, err := deps.Account.SetupRequired()
	if err != nil {
		return authLoginOutcome{Kind: authLoginSetupStateFailed, Err: err}
	}
	if required {
		return authLoginOutcome{Kind: authLoginSetupRequired}
	}
	ok, err := deps.Account.Authenticate(cmd.Username, cmd.Password)
	if errors.Is(err, authpkg.ErrSetupRequired) {
		return authLoginOutcome{Kind: authLoginSetupRequired, Err: err}
	}
	if err != nil {
		return authLoginOutcome{Kind: authLoginAuthenticationFailed, Err: err}
	}
	if !ok {
		m.recordAudit(authSessionCommandAuditRecord{
			Actor:      "unknown",
			ClientIP:   cmd.ClientIP,
			Action:     "auth.login",
			TargetType: "auth_user",
			TargetName: cmd.Username,
			Status:     "failure",
			Message:    "Invalid credentials",
		})
		return authLoginOutcome{Kind: authLoginInvalidCredentials, Username: cmd.Username}
	}
	if err := deps.Session.Stage(ctx, cmd.Username); err != nil {
		m.recordAudit(authSessionCommandAuditRecord{
			Actor:      cmd.Username,
			ClientIP:   cmd.ClientIP,
			Action:     "auth.login",
			TargetType: "auth_user",
			TargetName: cmd.Username,
			Status:     "failure",
			Message:    "Login session initialization failed",
			Meta:       map[string]any{"failure_kind": "session_stage_failed"},
		})
		return authLoginOutcome{Kind: authLoginSessionFailed, Username: cmd.Username, Err: err}
	}
	m.recordAudit(authSessionCommandAuditRecord{
		Actor:      cmd.Username,
		ClientIP:   cmd.ClientIP,
		Action:     "auth.login",
		TargetType: "auth_user",
		TargetName: cmd.Username,
		Status:     "success",
		Message:    "User logged in",
	})
	return authLoginOutcome{Kind: authLoginSucceeded, Username: cmd.Username}
}

func (m *authSessionCommands) Logout(ctx context.Context, cmd authLogoutCommand) authLogoutOutcome {
	deps := m.deps.withDefaults()
	if ctx == nil {
		ctx = context.Background()
	}
	cmd.Actor = strings.TrimSpace(cmd.Actor)
	cmd.ClientIP = strings.TrimSpace(cmd.ClientIP)
	if err := deps.Session.Destroy(ctx); err != nil {
		m.recordAudit(authSessionCommandAuditRecord{
			Actor:      cmd.Actor,
			ClientIP:   cmd.ClientIP,
			Action:     "auth.logout",
			TargetType: "auth_user",
			TargetName: cmd.Actor,
			Status:     "failure",
			Message:    "Failed to logout",
			Meta:       map[string]any{"failure_kind": "session_destroy_failed"},
		})
		return authLogoutOutcome{Kind: authLogoutSessionFailed, Err: err}
	}
	m.recordAudit(authSessionCommandAuditRecord{
		Actor:      cmd.Actor,
		ClientIP:   cmd.ClientIP,
		Action:     "auth.logout",
		TargetType: "auth_user",
		TargetName: cmd.Actor,
		Status:     "success",
		Message:    "User logged out",
	})
	return authLogoutOutcome{Kind: authLogoutSucceeded}
}

func (m *authSessionCommands) ClearSessions(ctx context.Context, cmd authClearSessionsCommand) authClearSessionsOutcome {
	deps := m.deps.withDefaults()
	if ctx == nil {
		ctx = context.Background()
	}
	cmd.Actor = strings.TrimSpace(cmd.Actor)
	cmd.ClientIP = strings.TrimSpace(cmd.ClientIP)
	if err := deps.Session.Destroy(ctx); err != nil {
		m.recordAudit(authSessionCommandAuditRecord{
			Actor:      cmd.Actor,
			ClientIP:   cmd.ClientIP,
			Action:     "auth.sessions.clear",
			TargetType: "auth_user",
			TargetName: cmd.Actor,
			Status:     "failure",
			Message:    "Failed to clear sessions",
			Meta:       map[string]any{"failure_kind": "current_session_destroy_failed"},
		})
		return authClearSessionsOutcome{Kind: authClearSessionsCurrentSessionFailed, Err: err}
	}
	deleted, err := deps.Account.ClearSessions()
	if err != nil {
		m.recordAudit(authSessionCommandAuditRecord{
			Actor:      cmd.Actor,
			ClientIP:   cmd.ClientIP,
			Action:     "auth.sessions.clear",
			TargetType: "auth_user",
			TargetName: cmd.Actor,
			Status:     "failure",
			Message:    "Current session destroyed but remaining sessions could not be cleared",
			Meta:       map[string]any{"failure_kind": "current_destroyed_clear_failed", "current_session_destroyed": true},
		})
		return authClearSessionsOutcome{Kind: authClearSessionsCurrentDestroyedClearFailed, DeletedSessions: 1, Err: err}
	}
	totalDeleted := deleted + 1
	m.recordAudit(authSessionCommandAuditRecord{
		Actor:      cmd.Actor,
		ClientIP:   cmd.ClientIP,
		Action:     "auth.sessions.clear",
		TargetType: "auth_user",
		TargetName: cmd.Actor,
		Status:     "success",
		Message:    "All sessions cleared",
		Meta:       map[string]any{"deleted_sessions": totalDeleted, "current_session_destroyed": true},
	})
	return authClearSessionsOutcome{Kind: authClearSessionsSucceeded, DeletedSessions: totalDeleted}
}

func (m *authSessionCommands) RevokeSession(ctx context.Context, cmd authRevokeSessionCommand) authRevokeSessionOutcome {
	deps := m.deps.withDefaults()
	if ctx == nil {
		ctx = context.Background()
	}
	cmd.Actor = strings.TrimSpace(cmd.Actor)
	cmd.ClientIP = strings.TrimSpace(cmd.ClientIP)
	cmd.SessionID = strings.TrimSpace(cmd.SessionID)
	found, err := deps.Account.RevokeSession(cmd.SessionID)
	if err != nil {
		m.recordAudit(authSessionCommandAuditRecord{
			Actor: cmd.Actor, ClientIP: cmd.ClientIP, Action: "auth.session.revoke",
			TargetType: "auth_session", TargetName: cmd.SessionID, Status: "failure",
			Message: "Failed to revoke session",
		})
		return authRevokeSessionOutcome{Kind: authRevokeSessionWriteFailed, Current: cmd.Current, Err: err}
	}
	if !found {
		return authRevokeSessionOutcome{Kind: authRevokeSessionNotFound, Current: cmd.Current}
	}
	if cmd.Current {
		if err := deps.Session.Destroy(ctx); err != nil {
			m.recordAudit(authSessionCommandAuditRecord{
				Actor: cmd.Actor, ClientIP: cmd.ClientIP, Action: "auth.session.revoke",
				TargetType: "auth_session", TargetName: cmd.SessionID, Status: "failure",
				Message: "Session revoked but current browser cookie could not be destroyed",
				Meta:    map[string]any{"current_session": true, "persisted_session_revoked": true},
			})
			return authRevokeSessionOutcome{Kind: authRevokeSessionDestroyedFailed, Current: true, Err: err}
		}
	}
	m.recordAudit(authSessionCommandAuditRecord{
		Actor: cmd.Actor, ClientIP: cmd.ClientIP, Action: "auth.session.revoke",
		TargetType: "auth_session", TargetName: cmd.SessionID, Status: "success",
		Message: "Session revoked", Meta: map[string]any{"current_session": cmd.Current},
	})
	return authRevokeSessionOutcome{Kind: authRevokeSessionSucceeded, Current: cmd.Current}
}

func (m *authSessionCommands) ClearOtherSessions(_ context.Context, cmd authClearOtherSessionsCommand) authClearOtherSessionsOutcome {
	deps := m.deps.withDefaults()
	cmd.Actor = strings.TrimSpace(cmd.Actor)
	cmd.ClientIP = strings.TrimSpace(cmd.ClientIP)
	cmd.CurrentToken = strings.TrimSpace(cmd.CurrentToken)
	deleted, err := deps.Account.ClearOtherSessions(cmd.CurrentToken)
	if err != nil {
		m.recordAudit(authSessionCommandAuditRecord{
			Actor: cmd.Actor, ClientIP: cmd.ClientIP, Action: "auth.sessions.clear_others",
			TargetType: "auth_user", TargetName: cmd.Actor, Status: "failure",
			Message: "Failed to clear other sessions",
		})
		return authClearOtherSessionsOutcome{Kind: authClearOtherSessionsWriteFailed, Err: err}
	}
	m.recordAudit(authSessionCommandAuditRecord{
		Actor: cmd.Actor, ClientIP: cmd.ClientIP, Action: "auth.sessions.clear_others",
		TargetType: "auth_user", TargetName: cmd.Actor, Status: "success",
		Message: "Other sessions cleared", Meta: map[string]any{"deleted_sessions": deleted},
	})
	return authClearOtherSessionsOutcome{Kind: authClearOtherSessionsSucceeded, DeletedSessions: deleted}
}

func (m *authSessionCommands) RevealSessionIP(_ context.Context, cmd authRevealSessionIPCommand) authRevealSessionIPOutcome {
	deps := m.deps.withDefaults()
	cmd.Actor = strings.TrimSpace(cmd.Actor)
	cmd.ClientIP = strings.TrimSpace(cmd.ClientIP)
	cmd.SessionID = strings.TrimSpace(cmd.SessionID)
	if deps.PasswordPolicy.Limited(cmd.AttemptKey) {
		return authRevealSessionIPOutcome{Kind: authRevealSessionIPRateLimited}
	}
	accepted, err := deps.Account.Authenticate(cmd.Actor, cmd.CurrentPassword)
	if err != nil {
		return authRevealSessionIPOutcome{Kind: authRevealSessionIPFailed, Err: err}
	}
	if !accepted {
		deps.PasswordPolicy.RecordFailure(cmd.AttemptKey)
		m.recordAudit(authSessionCommandAuditRecord{
			Actor: cmd.Actor, ClientIP: cmd.ClientIP, Action: "auth.session.ip_reveal",
			TargetType: "auth_session", TargetName: cmd.SessionID, Status: "failure",
			Message: "Session IP reveal rejected",
		})
		return authRevealSessionIPOutcome{Kind: authRevealSessionIPInvalid}
	}
	ip, found, err := deps.Account.RevealSessionIP(cmd.SessionID)
	if err != nil {
		return authRevealSessionIPOutcome{Kind: authRevealSessionIPFailed, Err: err}
	}
	if !found || strings.TrimSpace(ip) == "" {
		return authRevealSessionIPOutcome{Kind: authRevealSessionIPNotFound}
	}
	m.recordAudit(authSessionCommandAuditRecord{
		Actor: cmd.Actor, ClientIP: cmd.ClientIP, Action: "auth.session.ip_reveal",
		TargetType: "auth_session", TargetName: cmd.SessionID, Status: "success",
		Message: "Session IP revealed",
	})
	return authRevealSessionIPOutcome{Kind: authRevealSessionIPSucceeded, IP: ip}
}

func authPasswordChangeRateKey(clientIP, actor string) string {
	return strings.TrimSpace(clientIP) + ":" + strings.TrimSpace(actor) + ":password-change"
}

func (m *authSessionCommands) recordAudit(record authSessionCommandAuditRecord) {
	if err := m.deps.RecordAudit(record); err != nil {
		m.deps.Logf("audit write failed: action=%s target=%s err=%v", record.Action, record.TargetName, err)
	}
}
