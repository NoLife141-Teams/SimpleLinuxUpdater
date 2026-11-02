package main

import (
	"encoding/json"
	"testing"
)

func TestSecurityActionsReaderRouteIsReadOnlyAndBounded(t *testing.T) {
	app := newIsolatedTestApp(t)
	app.authenticate(t)
	if _, err := app.Deps.AuthService.CreateAccount("actions-reader", testPasswordStrong, "reader"); err != nil {
		t.Fatal(err)
	}
	reader := identityLogin(t, app, "actions-reader")
	response := identityRequest(app, "GET", "/api/security-actions?page_size=1", reader, "", nil)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var data struct {
		Fixes, Alerts, Lifecycle []json.RawMessage
		Pages                    int
	}
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Fixes) > 1 || len(data.Alerts) > 1 || len(data.Lifecycle) > 1 || data.Pages < 1 {
		t.Fatal("unbounded response")
	}
	if response.Header().Get("Cache-Control") == "" {
		t.Fatal("security inventory response may be cached")
	}
	if r := identityRequest(app, "GET", "/api/security-actions?page_size=1000", reader, "", nil); r.Code != 400 {
		t.Fatal("invalid pagination accepted")
	}
}
