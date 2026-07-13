package observability

import (
	"os"
	"strings"
	"testing"
)

func TestDashboardProjectionArchitectureKeepsCollectionMechanicsOutsideProjection(t *testing.T) {
	source, err := os.ReadFile("dashboard_projection.go")
	if err != nil {
		t.Fatalf("read dashboard projection source: %v", err)
	}
	projectionSource := string(source)
	for _, forbidden := range []string{
		"ServiceDeps",
		"database/sql",
		"encoding/json",
		"jobs.Record",
		"time.Now",
		"CurrentTimezone",
		"FormatTimestamp",
	} {
		if strings.Contains(projectionSource, forbidden) {
			t.Fatalf("Dashboard Projection contains forbidden collection mechanic %q", forbidden)
		}
	}
}

func TestObservabilityServiceComposesDashboardCollectionThenProjection(t *testing.T) {
	source, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read observability service source: %v", err)
	}
	serviceSource := string(source)
	for _, required := range []string{
		"collector.Collect(rawWindow, now)",
		"projection.Project(projectionInput)",
	} {
		if !strings.Contains(serviceSource, required) {
			t.Fatalf("Observability Service is missing collect-then-project composition %q", required)
		}
	}
}
