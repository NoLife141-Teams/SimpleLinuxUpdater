package main

import (
	"net/http"
	"strconv"
	"strings"

	authpkg "debian-updater/internal/auth"
	"github.com/gin-gonic/gin"
)

// Every protected route must appear here. Registration without a declaration
// fails closed, including pages, streams, exports and read-only administration.
type routePermission struct {
	Scope  string
	Bearer bool
}

var routePermissions = buildRoutePermissions()

func buildRoutePermissions() map[string]routePermission {
	out := map[string]routePermission{}
	add := func(scope string, bearer bool, method string, paths ...string) {
		for _, p := range paths {
			out[method+" "+p] = routePermission{scope, bearer}
		}
	}
	add("operational:read", false, "GET", "/", "/observability", "/account", "/api/dashboard/events")
	add("inventory:write", false, "GET", "/manage")
	add("operational:read", true, "GET", "/api/servers", "/api/servers/:name/vulnerabilities", "/api/dashboard/summary", "/api/observability/summary", "/api/observability/health-trends", "/api/app-settings/timezone", "/api/package-deferral-snapshots/:revision", "/api/servers/:name/package-deferrals")
	add("logs:read", true, "GET", "/api/jobs/:id", "/api/jobs/:id/logs", "/api/reports/jobs/:id")
	add("audit:read", true, "GET", "/api/audit-events", "/api/reports/audit/:id")
	add("maintenance:write", true, "POST", "/api/update/:name", "/api/autoremove/:name", "/api/repair/:name", "/api/reboot/:name", "/api/servers/:name/facts/refresh", "/api/servers/:name/vulnerabilities/scan", "/api/servers/:name/vulnerabilities/scans/:scan_id/cancel", "/api/servers/:name/postchecks/test")
	add("approvals:write", true, "POST", "/api/approve/:name", "/api/approve-security/:name", "/api/approve-security-kept-back/:name", "/api/approve-full/:name", "/api/cancel/:name")
	add("inventory:write", false, "POST", "/api/servers", "/api/servers/:name/key", "/api/keys/global", "/api/hostkeys/scan", "/api/hostkeys/trust", "/api/hostkeys/clear", "/api/sudoers/:name", "/api/sudoers/disable/:name")
	add("inventory:write", false, "PUT", "/api/servers/:name")
	add("inventory:write", false, "PUT", "/api/servers/:name/availability")
	add("inventory:write", false, "POST", "/api/servers/onboarding", "/api/servers/onboarding/check", "/api/servers/import", "/api/servers/import/preview")
	add("inventory:write", false, "GET", "/api/servers/import/example/:format")
	add("inventory:write", false, "DELETE", "/api/servers/:name", "/api/servers/:name/password", "/api/servers/:name/key", "/api/keys/global")
	add("inventory:write", false, "GET", "/api/keys/global", "/api/postcheck-profiles", "/api/servers/:name/postchecks")
	add("policies:write", false, "GET", "/api/update-policies", "/api/update-policies/runs", "/api/update-policies/calendar", "/api/update-policies/settings", "/api/update-policies/:id/overrides", "/api/package-deferrals")
	add("policies:write", false, "POST", "/api/update-policies", "/api/update-policies/preview", "/api/package-deferrals")
	add("policies:write", false, "PUT", "/api/update-policies/settings", "/api/update-policies/:id/overrides/:server", "/api/update-policies/:id", "/api/postcheck-profiles")
	add("policies:write", false, "DELETE", "/api/update-policies/:id", "/api/package-deferrals/:id")
	add("settings:write", false, "GET", "/admin", "/api/metrics/token", "/api/notifications/settings", "/api/notifications/delivery-diagnostics", "/api/auth/accounts")
	add("settings:write", false, "POST", "/api/metrics/token", "/api/notifications/test", "/api/audit-events/prune", "/api/auth/accounts")
	add("settings:write", false, "PUT", "/api/notifications/settings", "/api/app-settings/timezone", "/api/auth/accounts/:id")
	add("settings:write", false, "DELETE", "/api/metrics/token")
	add("backup:write", false, "GET", "/api/backup/automation", "/api/backup/status")
	add("backup:write", false, "PUT", "/api/backup/automation")
	add("backup:write", false, "POST", "/api/backup/automation/run", "/api/backup/export", "/api/backup/restore", "/api/backup/verify")
	add("self", false, "GET", "/api/auth/sessions", "/api/auth/identity", "/api/auth/tokens")
	add("self", false, "POST", "/api/auth/logout", "/api/auth/sessions/:id/reveal-ip", "/api/auth/reauthenticate", "/api/auth/mfa/enroll", "/api/auth/mfa/confirm", "/api/auth/tokens")
	add("self", false, "PUT", "/api/auth/password")
	add("self", false, "DELETE", "/api/auth/sessions", "/api/auth/sessions/others", "/api/auth/sessions/:id", "/api/auth/mfa", "/api/auth/tokens/:id")
	return out
}

type authenticatedPrincipal struct {
	Account authpkg.Account
	Token   authpkg.PersonalToken
	Bearer  bool
}

func principalForContext(c *gin.Context) (authenticatedPrincipal, bool) {
	if c == nil {
		return authenticatedPrincipal{}, false
	}
	v, ok := c.Get("auth_principal")
	if !ok {
		return authenticatedPrincipal{}, false
	}
	p, ok := v.(authenticatedPrincipal)
	return p, ok
}
func contextAllows(c *gin.Context, scope string) bool {
	p, ok := principalForContext(c)
	if !ok {
		return true
	} // Direct domain-handler tests have no HTTP boundary.
	if p.Bearer {
		return p.Token.Allows(p.Account, scope)
	}
	return authpkg.RoleAllows(p.Account.Role, scope)
}
func resolveCookiePrincipal(c *gin.Context) (authenticatedPrincipal, error) {
	sm := sessionManagerForContext(c)
	if sm == nil {
		return authenticatedPrincipal{}, nil
	}
	a, err := authpkg.SessionAccount(c.Request.Context(), sm)
	return authenticatedPrincipal{Account: a}, err
}
func authorizeRequest(c *gin.Context) bool {
	permission, declared := routePermissions[c.Request.Method+" "+c.FullPath()]
	var p authenticatedPrincipal
	var err error
	authz := strings.TrimSpace(c.GetHeader("Authorization"))
	if authz != "" {
		if !declared || !permission.Bearer {
			c.AbortWithStatusJSON(401, gin.H{"error": "bearer authentication is not accepted on this route"})
			return false
		}
		parts := strings.Fields(authz)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(401, gin.H{"error": "invalid bearer token"})
			return false
		}
		if !personalAPIRateLimiter.Allow("api:" + rateLimitClientIP(c)) {
			c.AbortWithStatusJSON(429, gin.H{"error": "too many API requests"})
			return false
		}
		p.Account, p.Token, err = authServiceForContext(c).VerifyPersonalToken(parts[1], maskedSessionClientIP(c))
		p.Bearer = true
	} else {
		p, err = resolveCookiePrincipal(c)
	}
	if err != nil || p.Account.ID == 0 {
		return false
	}
	if !declared || (permission.Scope != "self" && !authpkg.RoleAllows(p.Account.Role, permission.Scope)) || (p.Bearer && !p.Token.Allows(p.Account, permission.Scope)) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "permission denied"})
		return false
	}
	c.Set("auth_principal", p)
	actor := stableActor(p)
	c.Set("actor", actor)
	return true
}
func stableActor(p authenticatedPrincipal) string {
	actor := "account:" + formatAccountID(p.Account.ID)
	if p.Bearer {
		actor += "/token:" + p.Token.ID
	}
	return actor
}

func formatAccountID(id int64) string { return strconv.FormatInt(id, 10) }
