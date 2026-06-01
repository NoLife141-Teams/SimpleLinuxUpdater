package main

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"

	serverpkg "debian-updater/internal/servers"
)

func TestDemoSeedResetEnabled(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "empty disabled", raw: "", want: false},
		{name: "arbitrary disabled", raw: "variant-b", want: false},
		{name: "one enabled", raw: "1", want: true},
		{name: "true enabled", raw: "true", want: true},
		{name: "yes enabled", raw: "yes", want: true},
		{name: "reset enabled", raw: "reset", want: true},
		{name: "variant c enabled", raw: "variant-c", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := demoSeedResetEnabled(tt.raw); got != tt.want {
				t.Fatalf("demoSeedResetEnabled(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestSeedVariantCDemoRuntimeRestoresStateOnSaveFailure(t *testing.T) {
	state := newServerState()
	originalServers := []Server{{Name: "existing", Host: "existing.example.test", Port: 22, User: "root", Tags: []string{"prod"}}}
	originalStatuses := map[string]*ServerStatus{
		"existing": {Name: "existing", Host: "existing.example.test", Port: 22, User: "root", Status: "idle", Tags: []string{"prod"}},
	}
	state.Lock()
	state.SetServers(cloneServers(originalServers))
	state.SetStatusMap(cloneStatusMap(originalStatuses))
	state.Unlock()

	saveErr := errors.New("save failed")
	service := serverpkg.NewService(serverpkg.ServiceDeps{
		State:      state,
		Repository: failingDemoSeedRepository{err: saveErr},
	})

	err := seedVariantCDemoRuntime(AppDeps{
		ServerState:            state,
		ServerInventoryService: service,
		DB:                     func() *sql.DB { return nil },
	})
	if !errors.Is(err, saveErr) {
		t.Fatalf("seedVariantCDemoRuntime() error = %v, want %v", err, saveErr)
	}
	if got := state.CloneServers(); !reflect.DeepEqual(got, originalServers) {
		t.Fatalf("servers after failed seed = %+v, want %+v", got, originalServers)
	}
	if got := state.CloneStatusMap(); !reflect.DeepEqual(got, originalStatuses) {
		t.Fatalf("status map after failed seed = %+v, want %+v", got, originalStatuses)
	}
}

type failingDemoSeedRepository struct {
	err error
}

func (r failingDemoSeedRepository) Load() ([]serverpkg.Server, error) {
	return nil, nil
}

func (r failingDemoSeedRepository) Save([]serverpkg.Server, serverpkg.TxHook) error {
	return r.err
}

func (r failingDemoSeedRepository) UpdateServerKey(string, string) error {
	return nil
}
