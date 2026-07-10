package main

import (
	"reflect"
	"testing"

	serverpkg "debian-updater/internal/servers"
)

func TestAppDepsExposeGlobalSSHCredentialModuleWithoutLegacyFunctions(t *testing.T) {
	typeInfo := reflect.TypeOf(AppDeps{})
	forbidden := []string{"GetGlobalKey", "SetGlobalKey", "ClearGlobalKey", "HasGlobalKey"}
	for _, fieldName := range forbidden {
		if _, exists := typeInfo.FieldByName(fieldName); exists {
			t.Errorf("AppDeps must not expose legacy Global SSH Credential dependency %q", fieldName)
		}
	}
	field, exists := typeInfo.FieldByName("GlobalSSHCredential")
	if !exists {
		t.Fatal("AppDeps must expose GlobalSSHCredential")
	}
	want := reflect.TypeOf((*serverpkg.GlobalSSHCredential)(nil))
	if field.Type != want {
		t.Fatalf("GlobalSSHCredential type = %v, want %v", field.Type, want)
	}
}
