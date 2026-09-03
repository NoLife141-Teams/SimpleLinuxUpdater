package main

import (
	"os"
	"strings"
	"testing"
)

func TestWebserverTransportResponsibilitiesStaySeparated(t *testing.T) {
	tests := []struct {
		path     string
		required []string
	}{
		{
			path: "ssh_transport.go",
			required: []string{
				"type realSSHSession struct",
				"type realSSHConnection struct",
				"var dialSSHConnection",
			},
		},
		{
			path: "http_security.go",
			required: []string{
				"func securityHeadersMiddleware() gin.HandlerFunc",
				"func contentSecurityPolicyFromEnv() string",
				"func trustedProxiesFromEnv() []string",
			},
		},
		{
			path: "http_server_lifecycle.go",
			required: []string{
				"type actionRunnerTracker struct",
				"func shutdownApplication(",
				"func main()",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			source, err := os.ReadFile(tt.path)
			if err != nil {
				t.Fatalf("read %s: %v", tt.path, err)
			}
			for _, required := range tt.required {
				if !strings.Contains(string(source), required) {
					t.Errorf("%s is missing responsibility marker %q", tt.path, required)
				}
			}
		})
	}

	webserverSource, err := os.ReadFile("webserver.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, extracted := range []string{
		"type realSSHSession struct",
		"func securityHeadersMiddleware() gin.HandlerFunc",
		"type actionRunnerTracker struct",
		"func main()",
	} {
		if strings.Contains(string(webserverSource), extracted) {
			t.Errorf("webserver.go still owns extracted transport responsibility %q", extracted)
		}
	}
}
