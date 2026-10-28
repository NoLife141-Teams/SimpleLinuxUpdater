package servers

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestInventoryExportSelectionAndImportBounds(t *testing.T) {
	var mu sync.Mutex
	inventory := []Server{{Name: "one", Host: "192.0.2.1", User: "root", Pass: "secret", Key: "private", Tags: []string{"prod"}}, {Name: "two", Host: "192.0.2.2", User: "root", Disabled: true}}
	statuses := map[string]*ServerStatus{}
	state := NewState(&mu, &inventory, &statuses, nil)
	service := NewService(ServiceDeps{State: state})
	for _, scope := range []string{"selection", "filtered"} {
		raw, err := service.ExportInventory(InventoryExportRequest{Scope: scope, Names: []string{"two"}, Count: 1})
		if err != nil || !strings.Contains(string(raw), `"name":"two"`) || strings.Contains(string(raw), "one") {
			t.Fatalf("explicit scope %s: %s %v", scope, raw, err)
		}
	}
	for _, request := range []InventoryExportRequest{{Scope: "selection", Names: []string{"one", "one"}, Count: 2}, {Scope: "all", Names: []string{"one"}, Count: 2}, {Scope: "selection", Names: []string{"missing"}, Count: 1}, {Scope: "all", Count: 1}} {
		if _, err := service.ExportInventory(request); !errors.Is(err, ErrExportSelection) {
			t.Fatalf("invalid selection accepted: %+v %v", request, err)
		}
	}
	inventory = make([]Server, MaxImportRows+1)
	if _, err := service.ExportInventory(InventoryExportRequest{Scope: "all", Count: MaxImportRows + 1}); !errors.Is(err, ErrInventoryExportLimit) {
		t.Fatal("row bound not enforced")
	}
	inventory = []Server{{Name: "oversized", Tags: []string{strings.Repeat("x", MaxImportBytes)}}}
	if _, err := service.ExportInventory(InventoryExportRequest{Scope: "all", Count: 1}); !errors.Is(err, ErrInventoryExportLimit) {
		t.Fatal("byte bound not enforced")
	}
}
