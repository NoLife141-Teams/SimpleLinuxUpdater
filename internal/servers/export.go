package servers

import (
	"encoding/json"
	"errors"
	"sort"
)

// InventoryExportEntry is the closed inventory JSON v1 import schema. Keep it
// independent of Server so credentials and operational facts cannot leak.
type InventoryExportEntry struct {
	Name string   `json:"name"`
	Host string   `json:"host"`
	Port int      `json:"port"`
	User string   `json:"user"`
	Tags []string `json:"tags"`
}
type InventoryExportRequest struct {
	Scope string   `json:"scope"`
	Names []string `json:"names"`
	Count int      `json:"count"`
}

var ErrExportSelection = errors.New("invalid or changed export selection; review scope and count again")
var ErrInventoryExportLimit = errors.New("inventory export exceeds import limits (1000 hosts or 2 MiB); select fewer hosts")

func (s *Service) ExportInventory(request InventoryExportRequest) ([]byte, error) {
	if request.Scope != "selection" && request.Scope != "filtered" && request.Scope != "all" {
		return nil, ErrExportSelection
	}
	names := map[string]bool{}
	for _, name := range request.Names {
		if name == "" || names[name] {
			return nil, ErrExportSelection
		}
		names[name] = true
	}
	if request.Scope == "all" && len(names) > 0 {
		return nil, ErrExportSelection
	}
	if request.Count < 0 || request.Count > MaxImportRows || len(names) > MaxImportRows {
		return nil, ErrInventoryExportLimit
	}
	state := s.state()
	state.Lock()
	defer state.Unlock()
	entries := []InventoryExportEntry{}
	for _, server := range state.Servers() {
		if request.Scope != "all" && !names[server.Name] {
			continue
		}
		if len(entries) >= MaxImportRows {
			return nil, ErrInventoryExportLimit
		}
		tags := append([]string{}, server.Tags...)
		entries = append(entries, InventoryExportEntry{Name: server.Name, Host: server.Host, Port: NormalizePort(server.Port), User: server.User, Tags: tags})
	}
	if len(entries) != request.Count || (request.Scope != "all" && len(entries) != len(names)) {
		return nil, ErrExportSelection
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	raw, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxImportBytes {
		return nil, ErrInventoryExportLimit
	}
	return raw, nil
}
