package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestOperatorApplicationShellExposesExactPageFacts(t *testing.T) {
	typeInfo := reflect.TypeOf(operatorApplicationShellView{})
	want := map[string]reflect.Type{
		"ActiveSection": reflect.TypeOf(""),
		"PageLabel":     reflect.TypeOf(""),
	}
	if typeInfo.NumField() != len(want) {
		t.Fatalf("operatorApplicationShellView has %d fields, want %d", typeInfo.NumField(), len(want))
	}
	for index := 0; index < typeInfo.NumField(); index++ {
		field := typeInfo.Field(index)
		wantType, exists := want[field.Name]
		if !exists {
			t.Errorf("Operator Application Shell exposes unapproved page fact %q", field.Name)
			continue
		}
		if field.Type != wantType {
			t.Errorf("Operator Application Shell fact %s type = %v, want %v", field.Name, field.Type, wantType)
		}
		delete(want, field.Name)
	}
	for missing := range want {
		t.Errorf("Operator Application Shell lost page fact %q", missing)
	}

	wantConstructor := reflect.TypeOf((func(string, string) operatorApplicationShellView)(nil))
	if got := reflect.TypeOf(newOperatorApplicationShellView); got != wantConstructor {
		t.Errorf("newOperatorApplicationShellView type = %v, want %v", got, wantConstructor)
	}
}

func TestOperatorPageTemplatesDelegateShellOwnershipOnce(t *testing.T) {
	for _, path := range []string{
		"templates/index.html",
		"templates/manage.html",
		"templates/observability.html",
		"templates/admin.html",
	} {
		t.Run(path, func(t *testing.T) {
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s) error = %v", path, err)
			}
			body := string(source)
			if got := strings.Count(body, `{{template "operator-application-shell" .}}`); got != 1 {
				t.Errorf("%s shared shell calls = %d, want 1", path, got)
			}
			for _, forbidden := range []string{`<header class="app-header`, `class="brand-lockup"`, `class="app-nav"`} {
				if strings.Contains(body, forbidden) {
					t.Errorf("%s restores duplicated shell markup %q", path, forbidden)
				}
			}
		})
	}
}

func TestPageStylesheetsDoNotOwnApplicationShellDecisions(t *testing.T) {
	for _, path := range []string{
		"static/css/index.css",
		"static/css/manage.css",
		"static/css/observability.css",
		"static/css/admin.css",
	} {
		t.Run(path, func(t *testing.T) {
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s) error = %v", path, err)
			}
			for _, forbidden := range []string{".app-header", ".brand-lockup", ".brand-logo", ".app-nav"} {
				if strings.Contains(string(source), forbidden) {
					t.Errorf("%s restores Operator Application Shell selector %q", path, forbidden)
				}
			}
		})
	}
}
