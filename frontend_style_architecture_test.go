package main

import (
	"os"
	"strings"
	"testing"
)

func TestPageStylesDoNotRedefineSharedThemeTokens(t *testing.T) {
	for _, path := range []string{
		"static/css/index.css",
		"static/css/manage.css",
		"static/css/admin.css",
		"static/css/observability.css",
	} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(contents), ":root") {
			t.Fatalf("%s redefines shared theme tokens; base.css is the canonical owner", path)
		}
	}
}
