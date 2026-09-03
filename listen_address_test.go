package main

import (
	"os"
	"strings"
	"testing"
)

func TestResolveListenAddr(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "unset defaults to loopback", want: "127.0.0.1:8080"},
		{name: "whitespace only defaults to loopback", raw: " \t\n ", want: "127.0.0.1:8080"},
		{name: "IPv4 override", raw: "0.0.0.0:9090", want: "0.0.0.0:9090"},
		{name: "wildcard override", raw: ":8080", want: ":8080"},
		{name: "hostname override with surrounding spaces", raw: "  localhost:8081  ", want: "localhost:8081"},
		{name: "IPv6 loopback override", raw: "[::1]:8080", want: "[::1]:8080"},
		{name: "IPv6 wildcard override", raw: "[::]:8080", want: "[::]:8080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveListenAddr(func(key string) string {
				if key == listenAddrEnv {
					return tt.raw
				}
				return ""
			})
			if err != nil {
				t.Fatalf("resolveListenAddr() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveListenAddr() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBootstrapNetworkBindingsAreExplicit(t *testing.T) {
	checks := map[string][]string{
		"http_server_lifecycle.go": {
			"resolveListenAddr(os.Getenv)",
			"net.Listen(\"tcp\", listenAddr)",
			"Addr:         listenAddr",
			"server.Serve(listener)",
		},
		"Dockerfile": {
			"DEBIAN_UPDATER_LISTEN_ADDR=:8080",
			"EXPOSE 8080",
		},
		"playwright.config.js": {
			"DEBIAN_UPDATER_LISTEN_ADDR=127.0.0.1:8080",
			"http://127.0.0.1:8080/login",
		},
	}

	for path, required := range checks {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", path, err)
		}
		for _, fragment := range required {
			if !strings.Contains(string(raw), fragment) {
				t.Errorf("%s does not contain %q", path, fragment)
			}
		}
	}

	lifecycleRaw, err := os.ReadFile("http_server_lifecycle.go")
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", "http_server_lifecycle.go", err)
	}
	bindIndex := strings.Index(string(lifecycleRaw), "net.Listen(\"tcp\", listenAddr)")
	depsIndex := strings.Index(string(lifecycleRaw), ").withDefaults()")
	if bindIndex < 0 || depsIndex < 0 || bindIndex > depsIndex {
		t.Fatal("webserver must bind its HTTP listener before initializing runtime dependencies")
	}
}

func TestResolveListenAddrRejectsInvalidValues(t *testing.T) {
	for _, raw := range []string{
		"localhost",
		"localhost:not-a-port",
		"localhost:0",
		"localhost:65536",
		"::1:8080",
		"bad host:8080",
		"999.999.999.999:8080",
		"[localhost]:8080",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := resolveListenAddr(func(string) string { return raw })
			if err == nil {
				t.Fatalf("resolveListenAddr(%q) error = nil, want invalid address error", raw)
			}
			if !strings.Contains(err.Error(), listenAddrEnv) {
				t.Fatalf("resolveListenAddr(%q) error = %q, want environment variable name", raw, err)
			}
		})
	}
}
