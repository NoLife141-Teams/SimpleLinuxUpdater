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
	ClearSessions() (int64, error)
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
	authPasswordSucceeded       authPasswordOutcomeKind = "succeeded"
	authPasswordInvalid         authPasswordOutcomeKind = "invalid"
	authPasswordCurrentRejected authPasswordOutcomeKind = "current_password_rejected"
	authPasswordSetupRequired   authPasswordOutcomeKind = "setup_required"
	authPasswordRateLimited     authPasswordOutcomeKind = "rate_limited"
	authPasswordWriteFailed     authPasswordOutcomeKind = "write_failed"
)

type authPasswordCommand struct {
	Actor           string
	ClientIP        string
	CurrentPassword string
	NewPassword     string
	ConfirmPassword string
}

type authPasswordOutcome struct {
	Kind        authPasswordOutcomeKind
	PublicError string
	Err         error
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

func (m *authSessionCommands) ChangePassword(ctx context.Context, cmd authPasswordCommand) authPasswordOutcome {
	deps := m.deps.withDefaults()
	if ctx == nil {
		ctx = context.Background()
	}
	cmd.Actor = strings.TrimSpace(cmd.Actor)
	cmd.ClientIP = strings.TrimSpace(cmd.ClientIP)
	key := authPasswordChangeRateKey(cmd.ClientIP, cmd.Actor)
	if deps.PasswordPolicy.Limited(key) {
		return authPasswordOutcome{Kind: authPasswordRateLimited, PublicError: "too many password change attempts"}
	}

	err := deps.Account.ChangePassword(cmd.CurrentPassword, cmd.NewPassword, cmd.ConfirmPassword)
	if err == nil {
		m.recordAudit(authSessionCommandAuditRecord{
			Actor:      cmd.Actor,
			ClientIP:   cmd.ClientIP,
			Action:     "auth.password.change",
			TargetType: "auth_user",
			TargetName: cmd.Actor,
			Status:     "success",
			Message:    "Password changed",
		})
		return authPasswordOutcome{Kind: authPasswordSucceeded}
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

func authPasswordChangeRateKey(clientIP, actor string) string {
	return strings.TrimSpace(clientIP) + ":" + strings.TrimSpace(actor) + ":password-change"
}

func (m *authSessionCommands) recordAudit(record authSessionCommandAuditRecord) {
	if err := m.deps.RecordAudit(record); err != nil {
		m.deps.Logf("audit write failed: action=%s target=%s err=%v", record.Action, record.TargetName, err)
	}
}
