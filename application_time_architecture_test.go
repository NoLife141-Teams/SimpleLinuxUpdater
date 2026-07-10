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
}
