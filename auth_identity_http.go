package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	auditpkg "debian-updater/internal/audit"
	authpkg "debian-updater/internal/auth"
	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
)

func registerIdentityRoutes(r *gin.Engine) {
	r.GET("/account", func(c *gin.Context) {
		setNoStoreHeaders(c)
		c.HTML(200, "account.html", operatorShellForContext(c, "admin", "Account"))
	})
	r.GET("/api/auth/identity", handleIdentity)
	r.POST("/api/auth/reauthenticate", handleReauthenticate)
	r.GET("/api/auth/accounts", handleAccounts)
	r.POST("/api/auth/accounts", requireRecentAuthentication(), handleCreateAccount)
	r.PUT("/api/auth/accounts/:id", requireRecentAuthentication(), handleUpdateAccount)
	r.POST("/api/auth/mfa/enroll", requireRecentAuthentication(), handleMFAEnroll)
	r.POST("/api/auth/mfa/confirm", requireRecentAuthentication(), handleMFAConfirm)
	r.DELETE("/api/auth/mfa", requireRecentAuthentication(), handleMFADisable)
	r.GET("/api/auth/tokens", handlePersonalTokens)
	r.POST("/api/auth/tokens", requireRecentAuthentication(), handlePersonalTokenCreate)
	r.DELETE("/api/auth/tokens/:id", requireRecentAuthentication(), handlePersonalTokenRevoke)
}
func identityAuditMeta(c *gin.Context, meta map[string]any) map[string]any {
	p, ok := principalForContext(c)
	if !ok {
		return meta
	}
	out := auditpkg.CloneMeta(meta)
	out["account_id"] = p.Account.ID
	if p.Bearer {
		out["personal_token_id"] = p.Token.ID
	}
	return out
}
func handleIdentity(c *gin.Context) {
	p, _ := principalForContext(c)
	setNoStoreHeaders(c)
	c.JSON(200, gin.H{"account": p.Account, "scopes": authpkg.PersonalScopesForRole(p.Account.Role), "reauthentication_seconds": 300})
}
func requireRecentAuthentication() gin.HandlerFunc {
	return func(c *gin.Context) {
		setNoStoreHeaders(c)
		sm := sessionManagerForContext(c)
		at, _ := sm.Get(c.Request.Context(), "auth_reauthenticated_at").(int64)
		if at <= 0 || time.Now().Unix()-at > 300 {
			c.AbortWithStatusJSON(401, gin.H{"error": "recent password and MFA reauthentication required", "reauthentication_required": true})
			return
		}
		c.Next()
	}
}
func handleReauthenticate(c *gin.Context) {
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	limitAuthRequestBody(c)
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(400, gin.H{"error": "invalid request payload"})
		return
	}
	p, _ := principalForContext(c)
	key := "reauth:" + rateLimitClientIP(c) + ":" + formatAccountID(p.Account.ID)
	limiter := passwordChangeRateLimiterForContext(c)
	if !limiter.Allow(key) {
		c.JSON(429, gin.H{"error": "too many reauthentication attempts"})
		return
	}
	service := authServiceForContext(c)
	ok, err := service.Authenticate(p.Account.Username, req.Password)
	if err != nil || !ok {
		c.JSON(401, gin.H{"error": "invalid credentials"})
		return
	}
	if p.Account.MFAEnabled {
		if err = service.ConsumeFactor(p.Account.ID, req.Code); err != nil {
			status := 401
			if errors.Is(err, authpkg.ErrFactorRateLimited) {
				status = 429
			}
			c.JSON(status, gin.H{"error": "invalid authentication factor or too many attempts"})
			return
		}
	}
	sessionManagerForContext(c).Put(c.Request.Context(), "auth_reauthenticated_at", time.Now().Unix())
	audit(c, "auth.reauthenticate", "auth_user", p.Account.Username, "success", "Account reauthenticated", nil)
	c.JSON(200, gin.H{"message": "reauthenticated"})
}
func handleAccounts(c *gin.Context) {
	accounts, err := authServiceForContext(c).Accounts()
	if err != nil {
		c.JSON(500, gin.H{"error": "accounts unavailable"})
		return
	}
	setNoStoreHeaders(c)
	c.JSON(200, gin.H{"accounts": accounts})
}
func handleCreateAccount(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	limitAuthRequestBody(c)
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(400, gin.H{"error": "invalid request payload"})
		return
	}
	a, err := authServiceForContext(c).CreateAccount(req.Username, req.Password, req.Role)
	if err != nil {
		if authpkg.IsValidationError(err) {
			c.JSON(400, gin.H{"error": err.Error()})
		} else {
			c.JSON(409, gin.H{"error": "account could not be created; username must be unique"})
		}
		return
	}
	audit(c, "auth.account.create", "auth_user", a.Username, "success", "Named account created", gin.H{"target_account_id": a.ID, "role": a.Role})
	c.JSON(201, a)
}
func handleUpdateAccount(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		c.JSON(400, gin.H{"error": "invalid account ID"})
		return
	}
	var req struct {
		Role     string `json:"role"`
		Disabled *bool  `json:"disabled"`
	}
	limitAuthRequestBody(c)
	if c.ShouldBindJSON(&req) != nil || req.Disabled == nil {
		c.JSON(400, gin.H{"error": "role and disabled are required"})
		return
	}
	service := authServiceForContext(c)
	if err = service.UpdateAccount(id, req.Role, *req.Disabled); err != nil {
		c.JSON(409, gin.H{"error": "account change rejected; the last administrator must remain enabled and role must be valid"})
		return
	}
	audit(c, "auth.account.update", "auth_user", formatAccountID(id), "success", "Account role or enabled state updated", gin.H{"target_account_id": id, "role": req.Role, "disabled": *req.Disabled})
	c.JSON(200, gin.H{"message": "account updated; existing sessions revoked"})
}
func handleMFAEnroll(c *gin.Context) {
	p, _ := principalForContext(c)
	out, err := authServiceForContext(c).BeginMFA(p.Account.ID)
	setNoStoreHeaders(c)
	if err != nil {
		c.JSON(409, gin.H{"error": "MFA enrollment unavailable"})
		return
	}
	c.JSON(200, out)
}
func handleMFAConfirm(c *gin.Context) {
	var req struct {
		Code string `json:"code"`
	}
	limitAuthRequestBody(c)
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(400, gin.H{"error": "code is required"})
		return
	}
	p, _ := principalForContext(c)
	if !passwordChangeRateLimiterForContext(c).Allow("mfa-enroll:" + formatAccountID(p.Account.ID)) {
		c.JSON(429, gin.H{"error": "too many factor attempts"})
		return
	}
	codes, err := authServiceForContext(c).ConfirmMFA(p.Account.ID, req.Code)
	if err != nil {
		c.JSON(401, gin.H{"error": "invalid or expired enrollment code"})
		return
	}
	audit(c, "auth.mfa.enable", "auth_user", p.Account.Username, "success", "TOTP enabled; sessions and personal tokens revoked", nil)
	_ = sessionManagerForContext(c).Destroy(c.Request.Context())
	setNoStoreHeaders(c)
	c.JSON(200, gin.H{"recovery_codes": codes, "message": "Save these single-use codes now. Sign in again; sessions and personal tokens were revoked."})
}
func handleMFADisable(c *gin.Context) {
	p, _ := principalForContext(c)
	if err := authServiceForContext(c).DisableMFA(p.Account.ID); err != nil {
		c.JSON(500, gin.H{"error": "factor change failed"})
		return
	}
	audit(c, "auth.mfa.disable", "auth_user", p.Account.Username, "success", "TOTP disabled; sessions and personal tokens revoked", nil)
	_ = sessionManagerForContext(c).Destroy(c.Request.Context())
	c.JSON(200, gin.H{"message": "MFA disabled; sign in again"})
}
func handleMFAChallenge(c *gin.Context) {
	if c.GetHeader("Authorization") != "" {
		c.JSON(401, gin.H{"error": "cookie login required"})
		return
	}
	var req struct {
		Challenge string `json:"challenge"`
		Code      string `json:"code"`
	}
	limitAuthRequestBody(c)
	if c.ShouldBindJSON(&req) != nil || len(req.Challenge) != 64 {
		c.JSON(400, gin.H{"error": "challenge and code are required"})
		return
	}
	limiter := loginRateLimiterForContext(c)
	if !limiter.Allow("mfa:" + rateLimitClientIP(c)) {
		c.JSON(429, gin.H{"error": "too many factor attempts"})
		return
	}
	sm := sessionManagerForContext(c)
	ctx, err := authpkg.PrepareAuthentication(c.Request.Context(), sm)
	if err != nil {
		c.JSON(500, gin.H{"error": "authentication unavailable"})
		return
	}
	a, err := authServiceForContext(c).CompleteLoginChallenge(req.Challenge, req.Code)
	if err != nil {
		status := 401
		if errors.Is(err, authpkg.ErrFactorRateLimited) {
			status = 429
		}
		c.JSON(status, gin.H{"error": "invalid, expired or replayed authentication factor; sign in again"})
		return
	}
	ctx = authpkg.WithPreparedAccount(ctx, a)
	if err = (scsAuthSessionLifecycle{current: func() *scs.SessionManager { return sm }}).Stage(ctx, a.Username); err != nil {
		c.JSON(401, gin.H{"error": "authentication changed; sign in again"})
		return
	}
	c.Set("auth_principal", authenticatedPrincipal{Account: a})
	c.Set("actor", stableActor(authenticatedPrincipal{Account: a}))
	audit(c, "auth.login", "auth_user", a.Username, "success", "Account authenticated with MFA", nil)
	c.JSON(http.StatusOK, gin.H{"message": "login successful"})
}
func handlePersonalTokens(c *gin.Context) {
	p, _ := principalForContext(c)
	tokens, err := authServiceForContext(c).PersonalTokens(p.Account.ID)
	if err != nil {
		c.JSON(500, gin.H{"error": "personal tokens unavailable"})
		return
	}
	setNoStoreHeaders(c)
	c.JSON(200, gin.H{"tokens": tokens})
}
func handlePersonalTokenCreate(c *gin.Context) {
	var req struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ExpiresAt string   `json:"expires_at"`
	}
	limitAuthRequestBody(c)
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(400, gin.H{"error": "invalid request payload"})
		return
	}
	expires, err := time.Parse(time.RFC3339, req.ExpiresAt)
	if err != nil {
		c.JSON(400, gin.H{"error": "expires_at must be RFC3339"})
		return
	}
	p, _ := principalForContext(c)
	token, secret, err := authServiceForContext(c).CreatePersonalToken(p.Account.ID, req.Name, req.Scopes, expires)
	if err != nil {
		if authpkg.IsValidationError(err) {
			c.JSON(400, gin.H{"error": err.Error()})
		} else {
			c.JSON(500, gin.H{"error": "personal token creation failed"})
		}
		return
	}
	audit(c, "auth.personal_token.create", "personal_token", token.ID, "success", "Personal API token created", gin.H{"personal_token_id": token.ID, "scopes": token.Scopes})
	setNoStoreHeaders(c)
	c.JSON(201, gin.H{"token": token, "secret": secret})
}
func handlePersonalTokenRevoke(c *gin.Context) {
	p, _ := principalForContext(c)
	found, err := authServiceForContext(c).RevokePersonalToken(p.Account.ID, strings.TrimSpace(c.Param("id")))
	if err != nil {
		c.JSON(500, gin.H{"error": "token revocation failed"})
		return
	}
	if !found {
		c.JSON(404, gin.H{"error": "personal token not found"})
		return
	}
	audit(c, "auth.personal_token.revoke", "personal_token", c.Param("id"), "success", "Personal API token revoked", nil)
	c.JSON(200, gin.H{"message": "personal token revoked"})
}
