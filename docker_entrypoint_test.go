package main

import (
	"os"
	"strings"
	"testing"
)

func TestDockerEntrypointDelegatesRootOwnershipRepairToDescriptorHelper(t *testing.T) {
	contents, err := os.ReadFile("docker-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(contents)
	for _, required := range []string{
		`/usr/local/bin/persistence-owner "$app_uid" "$app_gid"`,
		`exec su-exec "$app_user:$app_group" "$@"`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("docker-entrypoint.sh missing %q", required)
		}
	}
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "chown" || strings.HasPrefix(trimmed, "chown ") || strings.HasPrefix(trimmed, "chown\t") {
			t.Fatalf("docker-entrypoint.sh must not perform path-based root chown operations: %q", trimmed)
		}
	}
}

func TestDockerfileBuildsRootOwnedPersistenceOwnerHelper(t *testing.T) {
	contents, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(contents)
	for _, required := range []string{
		`go build -o persistence-owner ./cmd/persistence-owner`,
		`COPY --from=builder --chown=root:root /app/persistence-owner /usr/local/bin/persistence-owner`,
		`chmod 0755 /usr/local/bin/docker-entrypoint.sh /usr/local/bin/persistence-owner`,
	} {
		if !strings.Contains(dockerfile, required) {
			t.Fatalf("Dockerfile missing %q", required)
		}
	}
}
