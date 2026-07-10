package updates

import (
	"reflect"
	"testing"
)

func TestServiceDepsExposeHostMaintenanceSessionInsteadOfRawSSHOrchestration(t *testing.T) {
	typeInfo := reflect.TypeOf(ServiceDeps{})
	forbidden := []string{
		"BuildAuthMethods",
		"HostKeyCallback",
		"DialSSH",
		"DialSSHWithRetry",
		"RunSSHOperationWithRetry",
		"RunSSHCommandWithTimeout",
		"RunUpdatePrechecks",
		"RunPostUpdateHealthChecks",
		"ListFailedSystemdUnits",
		"CollectServerFacts",
		"DiscoverPackages",
		"QueryPackageCVEs",
		"SSHConnectTimeout",
	}
	for _, fieldName := range forbidden {
		if _, exists := typeInfo.FieldByName(fieldName); exists {
			t.Errorf("ServiceDeps must not expose legacy SSH dependency %q", fieldName)
		}
	}
	field, exists := typeInfo.FieldByName("HostMaintenanceSessions")
	if !exists {
		t.Fatal("ServiceDeps must expose HostMaintenanceSessions")
	}
	want := reflect.TypeOf((*HostMaintenanceSessionFactory)(nil)).Elem()
	if field.Type != want {
		t.Fatalf("HostMaintenanceSessions type = %v, want %v", field.Type, want)
	}
}
