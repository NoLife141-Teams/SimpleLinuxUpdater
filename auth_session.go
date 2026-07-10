package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	apptimepkg "debian-updater/internal/apptime"
	authpkg "debian-updater/internal/auth"
	maintenancepkg "debian-updater/internal/maintenance"
	serverpkg "debian-updater/internal/servers"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
)

const (
	sessionCookieSecureEnv        = authpkg.SessionCookieSecureEnv
	sessionIdleTimeoutHoursEnv    = authpkg.SessionIdleTimeoutHoursEnv
	authSessionUserKey            = authpkg.SessionUserKey
	authUserID                    = authpkg.UserID
	authMinPasswordLen            = authpkg.MinPasswordLen
	authMaxPasswordLen            = authpkg.MaxPasswordLen
	authMaxRequestBytes           = authpkg.MaxRequestBytes
	authLoginRateLimitMaxAttempts = authpkg.LoginRateLimitMaxAttempts
	authPasswordChangeMaxAttempts = authpkg.PasswordChangeMaxAttempts
	authSetupRateLimitMaxAttempts = authpkg.SetupRateLimitMaxAttempts
	metricsRateLimitMaxAttempts   = authpkg.MetricsRateLimitMaxAttempts
	authRateLimitWindow           = authpkg.RateLimitWindow
	metricsRateLimitWindow        = authpkg.MetricsRateLimitWindow
	defaultSessionLifetime        = authpkg.DefaultSessionLifetime
)

var sessionManager *scs.SessionManager
var sessionManagerMu sync.RWMutex

var (
	errAuthPasswordMismatch = authpkg.ErrPasswordMismatch
)

type AuthService = authpkg.Service
type AuthRateLimiter = authpkg.RateLimiter
type AuthRateBucket = authpkg.RateBucket
type AuthCredentialsRequest = authpkg.CredentialsRequest
type AuthPasswordChangeRequest = authpkg.PasswordChangeRequest

func NewAuthService(db authpkg.DBProvider) *AuthService {
	return authpkg.NewService(authpkg.ServiceOptions{DB: db})
}

func defaultAuthService() *AuthService {
	return NewAuthService(getDB)
}

func NewAuthRateLimiter(window time.Duration, max int) *AuthRateLimiter {
	return authpkg.NewRateLimiter(window, max)
}

func limitAuthRequestBody(c *gin.Context) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, authMaxRequestBytes)
}

func authRequestBodyTooLarge(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}

var (
	loginRateLimiter          = NewAuthRateLimiter(authRateLimitWindow, authLoginRateLimitMaxAttempts)
	passwordChangeRateLimiter = NewAuthRateLimiter(authRateLimitWindow, authPasswordChangeMaxAttempts)
	setupRateLimiter          = NewAuthRateLimiter(authRateLimitWindow, authSetupRateLimitMaxAttempts)
	metricsRateLimiter        = NewAuthRateLimiter(metricsRateLimitWindow, metricsRateLimitMaxAttempts)
)

const authRuntimeDepsContextKey = "auth_runtime_deps"

type authRuntimeDeps struct {
	service                   *AuthService
	auditService              *AuditService
	commands                  *authSessionCommands
	serverState               *serverpkg.State
	sessionManager            func() *scs.SessionManager
	loginRateLimiter          *AuthRateLimiter
	passwordChangeRateLimiter *AuthRateLimiter
	setupRateLimiter          *AuthRateLimiter
}

func authRuntimeMiddleware(deps AppDeps) gin.HandlerFunc {
	deps = deps.withDefaults()
	runtime := authRuntimeDeps{
		service:                   deps.AuthService,
		auditService:              deps.AuditService,
		commands:                  deps.AuthSessionCommands,
		serverState:               deps.ServerState,
		sessionManager:            deps.CurrentSessionManager,
		loginRateLimiter:          deps.LoginRateLimiter,
		passwordChangeRateLimiter: deps.PasswordChangeRateLimiter,
		setupRateLimiter:          deps.SetupRateLimiter,
	}
	return func(c *gin.Context) {
		c.Set(authRuntimeDepsContextKey, runtime)
		c.Next()
	}
}

func authCommandsForContext(c *gin.Context) *authSessionCommands {
	if runtime, ok := authRuntimeFromContext(c); ok && runtime.commands != nil {
		return runtime.commands
	}
	return newAuthSessionCommandsWithDeps(authSessionCommandDeps{
		Account:        authServiceForContext(c),
		PasswordPolicy: passwordChangeRateLimiterForContext(c),
		Session: scsAuthSessionLifecycle{current: func() *scs.SessionManager {
			return sessionManagerForContext(c)
		}},
		RecordAudit: func(record authSessionCommandAuditRecord) error {
			if service := auditServiceForContext(c); service != nil {
				return service.Record(record.Actor, record.ClientIP, record.Action, record.TargetType, record.TargetName, record.Status, record.Message, record.Meta)
			}
			auditWithActor(record.Actor, record.ClientIP, record.Action, record.TargetType, record.TargetName, record.Status, record.Message, record.Meta)
			return nil
		},
	})
}

func authRuntimeFromContext(c *gin.Context) (authRuntimeDeps, bool) {
	if c == nil {
		return authRuntimeDeps{}, false
	}
	value, ok := c.Get(authRuntimeDepsContextKey)
	if !ok {
		return authRuntimeDeps{}, false
	}
	runtime, ok := value.(authRuntimeDeps)
	return runtime, ok
}

func authServiceForContext(c *gin.Context) *AuthService {
	if runtime, ok := authRuntimeFromContext(c); ok && runtime.service != nil {
		return runtime.service
	}
	return defaultAuthService()
}

func auditServiceForContext(c *gin.Context) *AuditService {
	if runtime, ok := authRuntimeFromContext(c); ok && runtime.auditService != nil {
		return runtime.auditService
	}
	return nil
}

func serverStateForContext(c *gin.Context) *serverpkg.State {
	if runtime, ok := authRuntimeFromContext(c); ok && runtime.serverState != nil {
		return runtime.serverState
	}
	return nil
}

func sessionManagerForContext(c *gin.Context) *scs.SessionManager {
	if runtime, ok := authRuntimeFromContext(c); ok && runtime.sessionManager != nil {
		if sm := runtime.sessionManager(); sm != nil {
			return sm
		}
	}
	return currentSessionManager()
}

func loginRateLimiterForContext(c *gin.Context) *AuthRateLimiter {
	if runtime, ok := authRuntimeFromContext(c); ok && runtime.loginRateLimiter != nil {
		return runtime.loginRateLimiter
	}
	return loginRateLimiter
}

func passwordChangeRateLimiterForContext(c *gin.Context) *AuthRateLimiter {
	if runtime, ok := authRuntimeFromContext(c); ok && runtime.passwordChangeRateLimiter != nil {
		return runtime.passwordChangeRateLimiter
	}
	return passwordChangeRateLimiter
}

func setupRateLimiterForContext(c *gin.Context) *AuthRateLimiter {
	if runtime, ok := authRuntimeFromContext(c); ok && runtime.setupRateLimiter != nil {
		return runtime.setupRateLimiter
	}
	return setupRateLimiter
}

func StopAuthRateLimiters() {
	if loginRateLimiter != nil {
		loginRateLimiter.Stop()
	}
	if passwordChangeRateLimiter != nil {
		passwordChangeRateLimiter.Stop()
	}
	if setupRateLimiter != nil {
		setupRateLimiter.Stop()
	}
	if metricsRateLimiter != nil {
		metricsRateLimiter.Stop()
	}
}

func bindAuthCredentialsRequest(c *gin.Context, requirePasswordConfirm bool) (AuthCredentialsRequest, bool, error) {
	if c == nil || c.Request == nil {
		return AuthCredentialsRequest{}, false, errors.New("missing request")
	}
	contentType := strings.ToLower(strings.TrimSpace(c.ContentType()))
	switch contentType {
	case "application/x-www-form-urlencoded", "multipart/form-data":
		if err := c.Request.ParseForm(); err != nil {
			return AuthCredentialsRequest{}, true, err
		}
		req := AuthCredentialsRequest{
			Username: c.PostForm("username"),
			Password: c.PostForm("password"),
		}
		if requirePasswordConfirm && req.Password != c.PostForm("password-confirm") {
			return req, true, errAuthPasswordMismatch
		}
		return req, true, nil
	default:
		var req AuthCredentialsRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			return AuthCredentialsRequest{}, false, err
		}
		return req, false, nil
	}
}

func writeAuthFormError(c *gin.Context, status int, message string) {
	setNoStoreHeaders(c)
	c.String(status, message)
}

func newSessionManager(db *sql.DB) (*scs.SessionManager, error) {
	return authpkg.NewSessionManager(db, authpkg.SessionManagerOptions{
		SecureCookieEnv:     sessionCookieSecureEnv,
		IdleTimeoutHoursEnv: sessionIdleTimeoutHoursEnv,
		CookieName:          authpkg.DefaultSessionCookieName,
		Lifetime:            defaultSessionLifetime,
		Logf:                log.Printf,
	})
}

func sessionHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sm := currentSessionManager()
		if sm == nil {
			next.ServeHTTP(w, r)
			return
		}
		sm.LoadAndSave(next).ServeHTTP(w, r)
	})
}

func setupRequired() (bool, error) {
	return defaultAuthService().SetupRequired()
}

func setupRequiredForContext(c *gin.Context) (bool, error) {
	return authServiceForContext(c).SetupRequired()
}

func getSingleUser() (username, passwordHash string, exists bool, err error) {
	return defaultAuthService().GetSingleUser()
}

func validatePasswordPolicy(password string) error {
	return defaultAuthService().ValidatePasswordPolicy(password)
}

func createInitialUser(username, password string) error {
	return defaultAuthService().CreateInitialUser(username, password)
}

func authenticateUser(username, password string) (bool, error) {
	return defaultAuthService().Authenticate(username, password)
}

func countStoredSessions() (int, error) {
	return defaultAuthService().CountSessions()
}

func countStoredSessionsForContext(c *gin.Context) (int, error) {
	return authServiceForContext(c).CountSessions()
}

func sessionUsername(c *gin.Context) string {
	if runtime, ok := authRuntimeFromContext(c); ok && runtime.sessionManager != nil {
		if sm := runtime.sessionManager(); sm != nil {
			if username, ok := sessionUsernameWithManager(c, sm); ok {
				return username
			}
		}
	}
	username, _ := sessionUsernameWithManager(c, currentSessionManager())
	return username
}

func sessionUsernameWithManager(c *gin.Context, sm *scs.SessionManager) (username string, ok bool) {
	defer func() {
		if recover() != nil {
			username = ""
			ok = false
		}
	}()
	return authpkg.SessionUsername(c, sm), true
}

func currentSessionManager() *scs.SessionManager {
	sessionManagerMu.RLock()
	defer sessionManagerMu.RUnlock()
	return sessionManager
}

func setNoStoreHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "0")
}

// sameOriginAuthRequest requires setup/login/logout requests to provide matching
// Origin/Referer host headers. If Sec-Fetch-Site is present, it must indicate
// same-origin/site context.
//
//	Origin: http://localhost
//	Referer: http://localhost/
//	Sec-Fetch-Site: same-origin (optional)
func sameOriginAuthRequest(c *gin.Context) bool {
	return authpkg.SameOriginRequest(c)
}

func sameOriginWriteMiddleware() gin.HandlerFunc {
	return authpkg.SameOriginWriteMiddleware()
}

const maintenanceExclusiveLeaseContextKey = "maintenance_exclusive_lease"

func maintenanceCoordinationMiddleware(coordinator *maintenancepkg.Coordinator, applicationTimes ...*apptimepkg.Module) gin.HandlerFunc {
	var applicationTime *apptimepkg.Module
	if len(applicationTimes) > 0 {
		applicationTime = applicationTimes[0]
	}
	return func(c *gin.Context) {
		if c == nil || c.Request == nil || c.Request.URL == nil {
			c.Next()
			return
		}
		path := c.Request.URL.Path
		if maintenanceBypassPath(path) || maintenanceAdmissionBypassPath(path) {
			c.Next()
			return
		}
		if maintenanceExclusivePath(path) {
			lease, decision := coordinator.TryExclusive(maintenanceOperationForPath(path))
			if !decision.Allowed {
				writeMaintenanceBlockedSnapshotResponseWithTime(c, decision.State, applicationTime)
				return
			}
			c.Set(maintenanceExclusiveLeaseContextKey, lease)
			defer lease.Close()
			c.Next()
			return
		}
		lease, decision := coordinator.TryShared(maintenancepkg.WorkInteractive)
		if !decision.Allowed {
			writeMaintenanceBlockedSnapshotResponseWithTime(c, decision.State, applicationTime)
			return
		}
		defer lease.Close()
		c.Next()
	}
}

func maintenanceOperationForPath(path string) maintenancepkg.OperationClass {
	if path == "/api/backup/restore" {
		return maintenancepkg.OperationBackupRestore
	}
	return maintenancepkg.OperationBackupExport
}

func maintenanceExclusiveLeaseFromContext(c *gin.Context) *maintenancepkg.ExclusiveLease {
	if c == nil {
		return nil
	}
	value, ok := c.Get(maintenanceExclusiveLeaseContextKey)
	if !ok {
		return nil
	}
	lease, _ := value.(*maintenancepkg.ExclusiveLease)
	return lease
}

func maintenanceAdmissionBypassPath(path string) bool {
	return path == "/api/dashboard/events"
}

func rateLimitClientIP(c *gin.Context) string {
	if c == nil {
		return "unknown"
	}
	host := strings.TrimSpace(c.ClientIP())
	if host == "" {
		return "unknown"
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

func metricsBearerMiddleware() gin.HandlerFunc {
	return metricsBearerMiddlewareWithService(metricsTokenService)
}

func metricsBearerMiddlewareWithService(service *MetricsTokenService) gin.HandlerFunc {
	return metricsBearerMiddlewareWithServiceAndLimiter(service, nil)
}

func metricsBearerMiddlewareWithServiceAndLimiter(service *MetricsTokenService, limiter *AuthRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		svc := service
		if svc == nil {
			svc = metricsTokenService
		}
		if svc == metricsTokenService {
			if strings.TrimSpace(getMetricsBearerTokenHash()) == "" {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
		} else if !svc.Status() {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		activeLimiter := limiter
		if activeLimiter == nil {
			activeLimiter = metricsRateLimiter
		}
		if activeLimiter != nil && !activeLimiter.Allow(rateLimitClientIP(c)) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
			return
		}

		authz := strings.TrimSpace(c.GetHeader("Authorization"))
		if authz == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		parts := strings.Fields(authz)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid bearer token"})
			return
		}
		match, err := svc.VerifyBearerToken(parts[1])
		if err != nil || !match {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid bearer token"})
			return
		}
		c.Next()
	}
}

func authGateMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/static/") {
			c.Next()
			return
		}
		username := sessionUsername(c)
		if username != "" {
			c.Set("actor", username)
			c.Next()
			return
		}
		required, err := setupRequiredForContext(c)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "failed to evaluate auth setup state"})
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":          "authentication required",
				"setup_required": required,
			})
			return
		}
		target := "/login"
		if required {
			target = "/setup"
		}
		c.Redirect(http.StatusFound, target)
		c.Abort()
	}
}

func handleSetupPage(c *gin.Context) {
	setNoStoreHeaders(c)
	required, err := setupRequiredForContext(c)
	if err != nil {
		c.String(http.StatusInternalServerError, "failed to evaluate setup state")
		return
	}
	if !required {
		if sessionUsername(c) != "" {
			c.Redirect(http.StatusFound, "/")
		} else {
			c.Redirect(http.StatusFound, "/login")
		}
		return
	}
	c.HTML(http.StatusOK, "setup.html", nil)
}

func handleLoginPage(c *gin.Context) {
	setNoStoreHeaders(c)
	required, err := setupRequiredForContext(c)
	if err != nil {
		c.String(http.StatusInternalServerError, "failed to evaluate setup state")
		return
	}
	if required {
		c.Redirect(http.StatusFound, "/setup")
		return
	}
	if sessionUsername(c) != "" {
		c.Redirect(http.StatusFound, "/")
		return
	}
	c.HTML(http.StatusOK, "login.html", nil)
}

func handleAuthStatus(c *gin.Context) {
	required, err := setupRequiredForContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to evaluate setup state"})
		return
	}
	username := sessionUsername(c)
	c.JSON(http.StatusOK, gin.H{
		"authenticated":  username != "",
		"username":       username,
		"setup_required": required,
	})
}

func handleAuthSessionsStatus(c *gin.Context) {
	count, err := countStoredSessionsForContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count sessions"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"session_count": count})
}

func handleAuthPasswordChange(c *gin.Context) {
	actor := sessionUsername(c)
	key := authPasswordChangeRateKey(rateLimitClientIP(c), actor)
	recordPasswordChangeFailure := func() {
		if limiter := passwordChangeRateLimiterForContext(c); limiter != nil {
			limiter.RecordFailure(key)
		}
	}
	if limiter := passwordChangeRateLimiterForContext(c); limiter != nil && limiter.Limited(key) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many password change attempts"})
		return
	}
	var req AuthPasswordChangeRequest
	limitAuthRequestBody(c)
	if err := c.ShouldBindJSON(&req); err != nil {
		recordPasswordChangeFailure()
		if authRequestBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request payload too large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
		return
	}
	if strings.TrimSpace(req.CurrentPassword) == "" || strings.TrimSpace(req.NewPassword) == "" {
		recordPasswordChangeFailure()
		c.JSON(http.StatusBadRequest, gin.H{"error": "current_password and new_password are required"})
		return
	}
	outcome := authCommandsForContext(c).ChangePassword(c.Request.Context(), authPasswordCommand{
		Actor:           actor,
		ClientIP:        clientIPFromContext(c),
		CurrentPassword: req.CurrentPassword,
		NewPassword:     req.NewPassword,
		ConfirmPassword: req.ConfirmPassword,
	})
	switch outcome.Kind {
	case authPasswordSucceeded:
	case authPasswordRateLimited:
		c.JSON(http.StatusTooManyRequests, gin.H{"error": outcome.PublicError})
		return
	case authPasswordSetupRequired:
		c.JSON(http.StatusConflict, gin.H{"error": outcome.PublicError})
		return
	case authPasswordInvalid, authPasswordCurrentRejected:
		c.JSON(http.StatusBadRequest, gin.H{"error": outcome.PublicError})
		return
	case authPasswordWriteFailed:
		log.Printf("handleAuthPasswordChange: password write failed for actor=%q: %v", actor, outcome.Err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": outcome.PublicError})
		return
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to change password"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "password changed"})
}

func handleAuthSessionsClear(c *gin.Context) {
	actor := sessionUsername(c)
	outcome := authCommandsForContext(c).ClearSessions(c.Request.Context(), authClearSessionsCommand{
		Actor:    actor,
		ClientIP: clientIPFromContext(c),
	})
	if outcome.Kind != authClearSessionsSucceeded {
		log.Printf("handleAuthSessionsClear: failed for actor=%q kind=%s: %v", actor, outcome.Kind, outcome.Err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to clear sessions"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "sessions cleared", "deleted_sessions": outcome.DeletedSessions})
}

func handleAuthSetup(c *gin.Context) {
	if !sameOriginAuthRequest(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "cross-site setup request denied"})
		return
	}
	key := fmt.Sprintf("%s:setup", rateLimitClientIP(c))
	if limiter := setupRateLimiterForContext(c); limiter != nil && !limiter.Allow(key) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many setup attempts"})
		return
	}
	required, err := setupRequiredForContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to evaluate setup state"})
		return
	}
	if !required {
		c.JSON(http.StatusConflict, gin.H{"error": "setup already completed"})
		return
	}
	limitAuthRequestBody(c)
	req, formPost, err := bindAuthCredentialsRequest(c, true)
	if err != nil {
		if authRequestBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request payload too large"})
			return
		}
		if errors.Is(err, errAuthPasswordMismatch) {
			if formPost {
				writeAuthFormError(c, http.StatusBadRequest, err.Error())
				return
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
		return
	}
	outcome := authCommandsForContext(c).Setup(c.Request.Context(), authSetupCommand{
		Username: req.Username,
		Password: req.Password,
		ClientIP: clientIPFromContext(c),
	})
	switch outcome.Kind {
	case authSetupInvalid:
		c.JSON(http.StatusBadRequest, gin.H{"error": outcome.PublicError})
		return
	case authSetupAlreadyCompleted:
		c.JSON(http.StatusConflict, gin.H{"error": "setup already completed"})
		return
	case authSetupStateFailed:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to evaluate setup state"})
		return
	case authSetupAccountWriteFailed:
		log.Printf("handleAuthSetup: failed to create initial user for username=%q: %v", strings.TrimSpace(req.Username), outcome.Err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create user"})
		return
	case authSetupAccountCreatedSessionFailed:
		log.Printf("handleAuthSetup: initial user created but session initialization failed for username=%q: %v", outcome.Username, outcome.Err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to initialize session"})
		return
	case authSetupSucceeded:
		c.Set("actor", outcome.Username)
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create user"})
		return
	}
	if formPost {
		c.Redirect(http.StatusSeeOther, "/")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "setup complete"})
}

func handleAuthLogin(c *gin.Context) {
	if !sameOriginAuthRequest(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "cross-site login request denied"})
		return
	}
	key := fmt.Sprintf("%s:login", rateLimitClientIP(c))
	if limiter := loginRateLimiterForContext(c); limiter != nil && !limiter.Allow(key) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many login attempts"})
		return
	}
	required, err := setupRequiredForContext(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to evaluate setup state"})
		return
	}
	if required {
		c.JSON(http.StatusConflict, gin.H{"error": "setup required", "setup_required": true})
		return
	}

	limitAuthRequestBody(c)
	req, formPost, err := bindAuthCredentialsRequest(c, false)
	if err != nil {
		if authRequestBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request payload too large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
		return
	}
	outcome := authCommandsForContext(c).Login(c.Request.Context(), authLoginCommand{
		Username: req.Username,
		Password: req.Password,
		ClientIP: clientIPFromContext(c),
	})
	switch outcome.Kind {
	case authLoginSucceeded:
		c.Set("actor", outcome.Username)
	case authLoginSetupRequired:
		c.JSON(http.StatusConflict, gin.H{"error": "setup required", "setup_required": true})
		return
	case authLoginSetupStateFailed:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to evaluate setup state"})
		return
	case authLoginInvalidCredentials:
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	case authLoginAuthenticationFailed:
		log.Printf("handleAuthLogin: authentication failed for username=%q: %v", strings.TrimSpace(req.Username), outcome.Err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "authentication failed"})
		return
	case authLoginSessionFailed:
		log.Printf("handleAuthLogin: session initialization failed for username=%q: %v", outcome.Username, outcome.Err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to renew session"})
		return
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "authentication failed"})
		return
	}
	if formPost {
		c.Redirect(http.StatusSeeOther, "/")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "login successful"})
}

func handleAuthLogout(c *gin.Context) {
	if !sameOriginAuthRequest(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "cross-site logout request denied"})
		return
	}
	actor := sessionUsername(c)
	outcome := authCommandsForContext(c).Logout(c.Request.Context(), authLogoutCommand{
		Actor:    actor,
		ClientIP: clientIPFromContext(c),
	})
	if outcome.Kind != authLogoutSucceeded {
		log.Printf("handleAuthLogout: session destruction failed for actor=%q: %v", actor, outcome.Err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to logout"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "logout successful"})
}
