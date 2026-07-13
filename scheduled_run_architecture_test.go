package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"

	auditpkg "debian-updater/internal/audit"
	jobspkg "debian-updater/internal/jobs"
	maintenancepkg "debian-updater/internal/maintenance"
	policypkg "debian-updater/internal/policies"
	scheduledrunspkg "debian-updater/internal/scheduledruns"
	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"
)

func TestScheduledRunLifecycleDepsExposeExactLifecycleKnowledge(t *testing.T) {
	want := map[string]reflect.Type{
		"AuditService":                    reflect.TypeOf((*auditpkg.Service)(nil)),
		"CurrentJobManager":               reflect.TypeOf((func() *jobspkg.Manager)(nil)),
		"JobTimestampNow":                 reflect.TypeOf((func() string)(nil)),
		"LoadRetryPolicy":                 reflect.TypeOf((func() updatespkg.RetryPolicy)(nil)),
		"MaintenanceCoordinator":          reflect.TypeOf((*maintenancepkg.Coordinator)(nil)),
		"PolicyRepository":                reflect.TypeOf((*scheduledrunspkg.RunRepository)(nil)).Elem(),
		"ServerState":                     reflect.TypeOf((*serverpkg.State)(nil)),
		"StartJobRunner":                  reflect.TypeOf((func(string, func()))(nil)),
		"StartScheduledRunReconciliation": reflect.TypeOf((func(int64, string))(nil)),
		"UpdateService":                   reflect.TypeOf((*updatespkg.Service)(nil)),
	}
	typeInfo := reflect.TypeOf(scheduledrunspkg.Deps{})
	if typeInfo.NumField() != len(want) {
		t.Fatalf("ScheduledRunLifecycleDeps has %d fields, want %d", typeInfo.NumField(), len(want))
	}
	for index := 0; index < typeInfo.NumField(); index++ {
		field := typeInfo.Field(index)
		wantType, exists := want[field.Name]
		if !exists {
			t.Errorf("ScheduledRunLifecycleDeps exposes unrelated dependency %q", field.Name)
			continue
		}
		if field.Type != wantType {
			t.Errorf("ScheduledRunLifecycleDeps.%s type = %v, want %v", field.Name, field.Type, wantType)
		}
		delete(want, field.Name)
	}
	for missing := range want {
		t.Errorf("ScheduledRunLifecycleDeps lost lifecycle dependency %q", missing)
	}
}

func TestScheduledRunLifecycleExposesExactInterface(t *testing.T) {
	want := map[string]reflect.Type{
		"ExecuteRun":         reflect.TypeOf((func(*scheduledrunspkg.Lifecycle, policypkg.Run, policypkg.Policy, serverpkg.Server))(nil)),
		"HandleScheduledRun": reflect.TypeOf((func(*scheduledrunspkg.Lifecycle, policypkg.ScheduledRunRequest) policypkg.ScheduledRunResult)(nil)),
		"LoadJobBehavior":    reflect.TypeOf((func(*scheduledrunspkg.Lifecycle, string) updatespkg.ScheduledJobBehavior)(nil)),
		"ReconcileJob":       reflect.TypeOf((func(*scheduledrunspkg.Lifecycle, int64, jobspkg.Record))(nil)),
		"UpdateJobDiscovery": reflect.TypeOf((func(*scheduledrunspkg.Lifecycle, string, updatespkg.PackageDiscoveryOutcome))(nil)),
		"WatchJob":           reflect.TypeOf((func(*scheduledrunspkg.Lifecycle, int64, string))(nil)),
	}
	typeInfo := reflect.TypeOf((*scheduledrunspkg.Lifecycle)(nil))
	if typeInfo.NumMethod() != len(want) {
		t.Fatalf("Lifecycle exposes %d methods, want %d", typeInfo.NumMethod(), len(want))
	}
	for index := 0; index < typeInfo.NumMethod(); index++ {
		method := typeInfo.Method(index)
		wantType, exists := want[method.Name]
		if !exists {
			t.Errorf("Lifecycle exposes unapproved method %q", method.Name)
			continue
		}
		if method.Type != wantType {
			t.Errorf("Lifecycle.%s type = %v, want %v", method.Name, method.Type, wantType)
		}
		delete(want, method.Name)
	}
	for missing := range want {
		t.Errorf("Lifecycle lost approved method %q", missing)
	}
}

func TestProcessPackageDoesNotOwnScheduledRunLifecycleSemantics(t *testing.T) {
	forbiddenFunctions := map[string]struct{}{
		"recordSkippedCandidate":           {},
		"markMaintenanceSkipped":           {},
		"runUpdate":                        {},
		"runScan":                          {},
		"updatePolicyRunFromJobRecord":     {},
		"recordScheduledScanTerminalAudit": {},
		"watchUpdatePolicyRunForJob":       {},
		"loadScheduledJobBehavior":         {},
		"updateScheduledJobDiscoveryMeta":  {},
	}
	forbiddenTypes := map[string]struct{}{
		"scheduledRunLifecycle":     {},
		"ScheduledRunLifecycleDeps": {},
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir(.) error = %v", err)
	}
	files := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(files, name, nil, 0)
		if parseErr != nil {
			t.Fatalf("ParseFile(%s) error = %v", name, parseErr)
		}
		for _, declaration := range parsed.Decls {
			switch typed := declaration.(type) {
			case *ast.FuncDecl:
				if _, forbidden := forbiddenFunctions[typed.Name.Name]; forbidden {
					t.Errorf("process package restores Scheduled Run Lifecycle function %s in %s", typed.Name.Name, name)
				}
			case *ast.GenDecl:
				for _, spec := range typed.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if _, forbidden := forbiddenTypes[typeSpec.Name.Name]; forbidden {
						t.Errorf("process package restores Scheduled Run Lifecycle type %s in %s", typeSpec.Name.Name, name)
					}
				}
			}
		}
	}
}

func TestProcessPackageScheduledRunHelpersAreOnlyCompositionAdapters(t *testing.T) {
	want := map[string]struct{}{
		"scheduledRunLifecycleDepsFromApp":     {},
		"scheduledRunLifecycleFromApp":         {},
		"scheduledRunLifecycleFromComposedApp": {},
		"defaultScheduledRunLifecycle":         {},
	}
	source, err := os.ReadFile("scheduled_run_adapter.go")
	if err != nil {
		t.Fatalf("ReadFile(scheduled_run_adapter.go) error = %v", err)
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), "scheduled_run_adapter.go", source, 0)
	if err != nil {
		t.Fatalf("ParseFile(scheduled_run_adapter.go) error = %v", err)
	}
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if _, approved := want[function.Name.Name]; !approved {
			t.Errorf("scheduled run adapter restores unapproved helper %q", function.Name.Name)
			continue
		}
		delete(want, function.Name.Name)
	}
	for missing := range want {
		t.Errorf("scheduled run adapter lost composition helper %q", missing)
	}
}

func TestRuntimeCompositionContainsScheduledRunAdaptersNotDecisions(t *testing.T) {
	source, err := os.ReadFile("runtime_composition.go")
	if err != nil {
		t.Fatalf("ReadFile(runtime_composition.go) error = %v", err)
	}
	for _, forbidden := range []string{"schedule.run.", ".CreateRun(", ".UpdateRun(", ".BeginAction(", ".RestoreStatusSnapshot("} {
		if strings.Contains(string(source), forbidden) {
			t.Errorf("Runtime Composition contains Scheduled Run decision token %q", forbidden)
		}
	}
}
