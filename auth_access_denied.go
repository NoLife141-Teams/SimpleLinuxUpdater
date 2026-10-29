package main

import (
	"mime"
	"net/http"
	"strconv"
	"strings"

	authpkg "debian-updater/internal/auth"
	"github.com/gin-gonic/gin"
)

// Page presentation is an explicit contract, never a global conversion of 403s.
// The caller has already authenticated the cookie and rejected authorization.
func renderPageAccessDenied(c *gin.Context, p authenticatedPrincipal) bool {
	if c.Request.Method != http.MethodGet || p.Bearer || p.Account.ID == 0 || !authpkg.RoleAllows(p.Account.Role, "operational:read") {
		return false
	}
	switch c.FullPath() {
	case "/admin", "/manage":
	default:
		return false
	}
	// Both representations depend on Accept; preserve any existing Vary values.
	c.Writer.Header().Add("Vary", "Accept")
	setNoStoreHeaders(c)
	if !acceptsAccessDeniedHTML(strings.Join(c.Request.Header.Values("Accept"), ",")) {
		return false
	}
	view := newOperatorApplicationShellView("", "Access denied")
	// Authorization refusal precedes auth_principal installation. Never use the
	// context helper's absent-role fallback to build this restricted shell.
	view.Role = p.Account.Role
	c.HTML(http.StatusForbidden, "access-denied.html", view)
	c.Abort()
	return true
}

// Explicit text/html is required. Empty or wildcard-only requests retain JSON.
// Specific ranges override wildcards (including q=0), and JSON wins equal weight.
func acceptsAccessDeniedHTML(accept string) bool {
	qualities := [2]float64{}
	specificities := [2]int{-1, -1}
	explicitHTML := false
	for _, item := range strings.Split(accept, ",") {
		media, params, err := mime.ParseMediaType(strings.TrimSpace(item))
		if err != nil {
			continue
		}
		q := 1.0
		if raw, ok := params["q"]; ok {
			q, err = strconv.ParseFloat(raw, 64)
			if err != nil || !(q >= 0 && q <= 1) {
				continue
			}
		}
		if media == "text/html" && q > 0 {
			explicitHTML = true
		}
		for i, candidate := range []string{"text/html", "application/json"} {
			specificity := -1
			switch media {
			case candidate:
				specificity = 2
			case strings.SplitN(candidate, "/", 2)[0] + "/*":
				specificity = 1
			case "*/*":
				specificity = 0
			}
			if specificity >= 0 && (specificity > specificities[i] || (specificity == specificities[i] && q > qualities[i])) {
				specificities[i], qualities[i] = specificity, q
			}
		}
	}
	return explicitHTML && qualities[0] > 0 && qualities[0] > qualities[1]
}
