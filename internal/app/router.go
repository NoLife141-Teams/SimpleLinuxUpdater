package app

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

type RouterConfig struct {
	TrustedProxies        func() []string
	GlobalMiddleware      []gin.HandlerFunc
	InitializeMaintenance func() error
	InitializeJobs        func() error
	InitializeSessions    func() error
	TemplatesGlob         string
	StaticPath            string
	StaticRoot            string
	RegisterRoutes        func(*gin.Engine) error
}

func NewRouter(config RouterConfig) (*gin.Engine, error) {
	r := gin.Default()
	// Match encoded separators as part of a parameter, then decode path values
	// exactly once. PathUnescape preserves literal plus signs in server names.
	r.UseRawPath = true
	r.UnescapePathValues = false
	r.Use(func(c *gin.Context) {
		if c.Request.URL.RawPath != "" {
			for i := range c.Params {
				if value, err := url.PathUnescape(c.Params[i].Value); err == nil {
					c.Params[i].Value = value
				}
			}
		}
		c.Next()
	})
	trustedProxies := []string(nil)
	if config.TrustedProxies != nil {
		trustedProxies = config.TrustedProxies()
	}
	if err := r.SetTrustedProxies(trustedProxies); err != nil {
		return nil, fmt.Errorf("failed to configure trusted proxies: %w", err)
	}
	if len(config.GlobalMiddleware) > 0 {
		r.Use(config.GlobalMiddleware...)
	}
	if config.InitializeMaintenance != nil {
		if err := config.InitializeMaintenance(); err != nil {
			return nil, fmt.Errorf("failed to initialize maintenance state: %w", err)
		}
	}
	if config.InitializeJobs != nil {
		if err := config.InitializeJobs(); err != nil {
			return nil, fmt.Errorf("failed to initialize job manager: %w", err)
		}
	}
	if config.InitializeSessions != nil {
		if err := config.InitializeSessions(); err != nil {
			return nil, fmt.Errorf("failed to initialize session manager: %w", err)
		}
	}

	templatesGlob := strings.TrimSpace(config.TemplatesGlob)
	if templatesGlob == "" {
		templatesGlob = "templates/*"
	}
	r.LoadHTMLGlob(templatesGlob)

	staticPath := strings.TrimSpace(config.StaticPath)
	if staticPath == "" {
		staticPath = "/static"
	}
	staticRoot := strings.TrimSpace(config.StaticRoot)
	if staticRoot == "" {
		staticRoot = "./static"
	}
	r.Static(staticPath, staticRoot)

	if config.RegisterRoutes != nil {
		if err := config.RegisterRoutes(r); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func ParseTrustedProxies(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "none") {
		return nil
	}
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	proxies := make([]string, 0, len(parts))
	for _, part := range parts {
		proxy := strings.TrimSpace(part)
		if proxy == "" {
			continue
		}
		if _, ok := seen[proxy]; ok {
			continue
		}
		seen[proxy] = struct{}{}
		proxies = append(proxies, proxy)
	}
	return proxies
}
