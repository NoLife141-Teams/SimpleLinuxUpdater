package main

import (
	"net/http"
	"testing"
)

type routeInventoryEntry struct {
	method string
	path   string
}

func criticalRouteInventory() []routeInventoryEntry {
	return []routeInventoryEntry{
		{http.MethodGet, "/setup"},
		{http.MethodGet, "/login"},
		{http.MethodPost, "/api/auth/setup"},
		{http.MethodPost, "/api/auth/login"},
		{http.MethodGet, "/api/auth/status"},
		{http.MethodGet, "/api/maintenance"},
		{http.MethodGet, "/metrics"},
		{http.MethodGet, "/"},
		{http.MethodGet, "/manage"},
		{http.MethodGet, "/observability"},
		{http.MethodGet, "/admin"},
		{http.MethodPost, "/api/auth/logout"},
		{http.MethodGet, "/api/auth/sessions"},
		{http.MethodPut, "/api/auth/password"},
		{http.MethodDelete, "/api/auth/sessions"},
		{http.MethodGet, "/api/metrics/token"},
		{http.MethodPost, "/api/metrics/token"},
		{http.MethodDelete, "/api/metrics/token"},
		{http.MethodGet, "/api/backup/status"},
		{http.MethodGet, "/api/dashboard/events"},
		{http.MethodGet, "/api/app-settings/timezone"},
		{http.MethodPut, "/api/app-settings/timezone"},
		{http.MethodGet, "/api/notifications/settings"},
		{http.MethodPut, "/api/notifications/settings"},
		{http.MethodPost, "/api/notifications/test"},
		{http.MethodPost, "/api/backup/export"},
		{http.MethodPost, "/api/backup/restore"},
		{http.MethodPost, "/api/backup/verify"},
		{http.MethodGet, "/api/update-policies"},
		{http.MethodPost, "/api/update-policies"},
		{http.MethodPost, "/api/update-policies/preview"},
		{http.MethodGet, "/api/update-policies/runs"},
		{http.MethodGet, "/api/update-policies/settings"},
		{http.MethodPut, "/api/update-policies/settings"},
		{http.MethodGet, "/api/update-policies/:id/overrides"},
		{http.MethodPut, "/api/update-policies/:id/overrides/:server"},
		{http.MethodPut, "/api/update-policies/:id"},
		{http.MethodDelete, "/api/update-policies/:id"},
		{http.MethodGet, "/api/audit-events"},
		{http.MethodGet, "/api/reports/audit/:id"},
		{http.MethodGet, "/api/reports/jobs/:id"},
		{http.MethodGet, "/api/jobs/:id"},
		{http.MethodGet, "/api/observability/summary"},
		{http.MethodGet, "/api/dashboard/summary"},
		{http.MethodPost, "/api/audit-events/prune"},
		{http.MethodGet, "/api/servers"},
		{http.MethodPost, "/api/servers"},
		{http.MethodPut, "/api/servers/:name"},
		{http.MethodDelete, "/api/servers/:name"},
		{http.MethodDelete, "/api/servers/:name/password"},
		{http.MethodPost, "/api/servers/:name/key"},
		{http.MethodDelete, "/api/servers/:name/key"},
		{http.MethodPost, "/api/keys/global"},
		{http.MethodDelete, "/api/keys/global"},
		{http.MethodGet, "/api/keys/global"},
		{http.MethodPost, "/api/servers/:name/facts/refresh"},
		{http.MethodPost, "/api/hostkeys/scan"},
		{http.MethodPost, "/api/hostkeys/trust"},
		{http.MethodPost, "/api/hostkeys/clear"},
		{http.MethodPost, "/api/update/:name"},
		{http.MethodPost, "/api/autoremove/:name"},
		{http.MethodPost, "/api/sudoers/:name"},
		{http.MethodPost, "/api/sudoers/disable/:name"},
		{http.MethodPost, "/api/approve/:name"},
		{http.MethodPost, "/api/approve-security/:name"},
		{http.MethodPost, "/api/approve-security-kept-back/:name"},
		{http.MethodPost, "/api/approve-full/:name"},
		{http.MethodPost, "/api/cancel/:name"},
	}
}

func TestRegisterRoutesInventory(t *testing.T) {
	app := newTestApp(t, testAppOptions{})

	registered := make(map[string]bool)
	for _, route := range app.Router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}

	expected := criticalRouteInventory()
	for _, route := range expected {
		key := route.method + " " + route.path
		if !registered[key] {
			t.Fatalf("route %s was not registered", key)
		}
	}
}
