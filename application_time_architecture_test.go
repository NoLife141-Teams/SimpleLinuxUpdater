package main

import (
	"os"
	"strings"
	"testing"
)

func TestApplicationTimeInterpretationArchitectureBoundary(t *testing.T) {
	for _, path := range []string{"app_deps.go", "runtime_composition.go"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, forbidden := range []string{"CurrentAppTimezone", "CurrentAppLocation", "AppTimezoneDisplayName", "AppTimezoneResolvedName"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s restores multiplied application-time fact %q", path, forbidden)
			}
		}
	}
	data, err := os.ReadFile("internal/policies/schedule_projection.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "time.Date(day.Year(), day.Month(), day.Day(), hour, minute") {
		t.Fatal("schedule projection constructs application-local occurrences outside Application Time Interpretation")
	}

	interaction, err := os.ReadFile("static/js/scheduled-policy-administration-interaction.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"Intl.", "new Date(", ".getDay(", ".getTimezoneOffset("} {
		if strings.Contains(string(interaction), forbidden) {
			t.Errorf("scheduled policy interaction reimplements server-authoritative schedule projection with %q", forbidden)
		}
	}
}
