package main

import (
	"os"
	"strings"

	appshell "debian-updater/internal/app"

	"github.com/gin-gonic/gin"
)

const trustedProxiesEnv = "DEBIAN_UPDATER_TRUSTED_PROXIES"
const devAllowBrowserAnnotationsEnv = "DEBIAN_UPDATER_DEV_ALLOW_BROWSER_ANNOTATIONS"
const defaultContentSecurityPolicy = "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; object-src 'none'; script-src 'self'; style-src 'self' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'"
const browserAnnotationsContentSecurityPolicy = defaultContentSecurityPolicy + "; style-src-elem 'self' https://fonts.googleapis.com 'unsafe-inline'"

func securityHeadersMiddleware() gin.HandlerFunc {
	contentSecurityPolicy := contentSecurityPolicyFromEnv()
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Content-Security-Policy", contentSecurityPolicy)
		if c.Request != nil && c.Request.TLS != nil {
			c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		} else {
			forwardedProto := strings.TrimSpace(c.GetHeader("X-Forwarded-Proto"))
			if forwardedProto != "" {
				if idx := strings.Index(forwardedProto, ","); idx >= 0 {
					forwardedProto = strings.TrimSpace(forwardedProto[:idx])
				}
				if strings.EqualFold(forwardedProto, "https") {
					c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
				}
			}
		}
		c.Next()
	}
}

func contentSecurityPolicyFromEnv() string {
	if parseBoolEnvWithDefault(devAllowBrowserAnnotationsEnv, false) {
		return browserAnnotationsContentSecurityPolicy
	}
	return defaultContentSecurityPolicy
}

func trustedProxiesFromEnv() []string {
	return appshell.ParseTrustedProxies(os.Getenv(trustedProxiesEnv))
}
