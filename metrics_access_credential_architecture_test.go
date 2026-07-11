package main

import (
	"os"
	"strings"
	"testing"
)

func TestMetricsAccessCredentialArchitecture(t *testing.T) {
	for _, file := range []string{"auth_session.go", "webserver.go", "runtime_composition.go", "backup_restore.go", "observability_service.go"} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			"SnapshotCache",
			"RestoreCache",
			"VerifyBearerToken",
			"getMetricsBearerTokenHash",
			"clearMetricsBearerTokenHash",
			"issueMetricsBearerToken",
			"syncMetricsTokenGlobals",
			"metricsBearerTokenHashMu",
			"metricsBearerTokenHashLoaded",
			"metricsBearerTokenHashDBPath",
		} {
			if strings.Contains(string(source), forbidden) {
				t.Errorf("%s restores Metrics Access Credential implementation %q", file, forbidden)
			}
		}
	}

	runtimeSource, err := os.ReadFile("runtime_composition.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runtimeSource), "MetricsAccessCredential.Invalidate()") {
		t.Error("Runtime Composition does not invalidate Metrics Access Credential during persistence replacement")
	}
}
