package servers

import "testing"

func TestEvaluateMaintenanceReadiness(t *testing.T) {
	tests := []struct {
		name                                    string
		hasPassword, hasServerKey, hasGlobalKey bool
		globalKeyKnown                          bool
		hostKeyStatus                           HostKeyStatus
		wantReady                               bool
		wantCode                                string
	}{
		{name: "password", hasPassword: true, globalKeyKnown: true, hostKeyStatus: HostKeyStatusTrusted, wantReady: true, wantCode: MaintenanceReadinessReady},
		{name: "server key", hasServerKey: true, globalKeyKnown: true, hostKeyStatus: HostKeyStatusTrusted, wantReady: true, wantCode: MaintenanceReadinessReady},
		{name: "global key", hasGlobalKey: true, globalKeyKnown: true, hostKeyStatus: HostKeyStatusTrusted, wantReady: true, wantCode: MaintenanceReadinessReady},
		{name: "missing authentication", globalKeyKnown: true, hostKeyStatus: HostKeyStatusTrusted, wantCode: MaintenanceReadinessMissingAuthentication},
		{name: "global status unavailable", hostKeyStatus: HostKeyStatusTrusted, wantCode: MaintenanceReadinessAuthenticationUnknown},
		{name: "host key missing", hasPassword: true, globalKeyKnown: true, hostKeyStatus: HostKeyStatusMissing, wantCode: MaintenanceReadinessHostKeyNotTrusted},
		{name: "host key unknown", hasPassword: true, globalKeyKnown: true, hostKeyStatus: HostKeyStatusUnknown, wantCode: MaintenanceReadinessHostKeyStatusUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateMaintenanceReadiness(tt.hasPassword, tt.hasServerKey, tt.hasGlobalKey, tt.globalKeyKnown, tt.hostKeyStatus)
			if got.Ready != tt.wantReady || got.Code != tt.wantCode || got.Message == "" {
				t.Fatalf("EvaluateMaintenanceReadiness() = %+v, want ready=%t code=%q", got, tt.wantReady, tt.wantCode)
			}
		})
	}
}
