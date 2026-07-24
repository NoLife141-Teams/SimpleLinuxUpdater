package updates

import (
	"context"
	"io"
	"reflect"
	"testing"
	"time"

	"debian-updater/internal/servers"

	"golang.org/x/crypto/ssh"
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

func TestProductionHostMaintenanceSessionDepsExposePrimitiveTransportOnly(t *testing.T) {
	typeInfo := reflect.TypeOf(ProductionHostMaintenanceSessionDeps{})
	want := map[string]reflect.Type{
		"BuildAuthMethods":    reflect.TypeOf((func(servers.Server) ([]ssh.AuthMethod, error))(nil)),
		"HostKeyCallback":     reflect.TypeOf((func() (ssh.HostKeyCallback, error))(nil)),
		"DialSSH":             reflect.TypeOf((func(servers.Server, *ssh.ClientConfig) (SSHConnection, error))(nil)),
		"RunCommand":          reflect.TypeOf((func(context.Context, SSHConnection, string, io.Reader, time.Duration) (string, string, error))(nil)),
		"RunStreamingCommand": reflect.TypeOf((func(context.Context, SSHConnection, string, io.Reader, time.Duration, HostCommandOutputHandler) (string, string, error))(nil)),
		"SSHConnectTimeout":   reflect.TypeOf(time.Duration(0)),
		"Sleep":               reflect.TypeOf((func(time.Duration))(nil)),
		"Logf":                reflect.TypeOf((func(string, ...any))(nil)),
	}
	if typeInfo.NumField() != len(want) {
		t.Fatalf("ProductionHostMaintenanceSessionDeps has %d fields, want the %d-field primitive allowlist", typeInfo.NumField(), len(want))
	}
	for index := 0; index < typeInfo.NumField(); index++ {
		field := typeInfo.Field(index)
		wantType, allowed := want[field.Name]
		if !allowed {
			t.Errorf("ProductionHostMaintenanceSessionDeps exposes non-primitive dependency %q", field.Name)
			continue
		}
		if field.Type != wantType {
			t.Errorf("ProductionHostMaintenanceSessionDeps.%s type = %v, want %v", field.Name, field.Type, wantType)
		}
		delete(want, field.Name)
	}
	for missing := range want {
		t.Errorf("ProductionHostMaintenanceSessionDeps lost primitive dependency %q", missing)
	}
}
