package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestEncodedSlashInventoryNameCanBeUpdatedAndDeleted(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(cookie)
		markSameOriginAuthRequest(req)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		app.Handler.ServeHTTP(rec, req)
		return rec
	}
	for _, name := range []string{"prod/db", "prod/db+replica", "prod%2Fdb", "prod+db", "prod/db space"} {
		t.Run(name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]string{"name": name, "host": "192.0.2.99", "user": "root"})
			if err != nil {
				t.Fatal(err)
			}
			create := request(http.MethodPost, "/api/servers", string(payload))
			if create.Code != http.StatusCreated {
				t.Fatalf("create: %d %s", create.Code, create.Body.String())
			}
			path := "/api/servers/" + url.PathEscape(name)
			update := request(http.MethodPut, path, string(payload))
			if update.Code != http.StatusOK {
				t.Fatalf("update: %d %s", update.Code, update.Body.String())
			}
			removed := request(http.MethodDelete, path, "")
			if removed.Code != http.StatusOK {
				t.Fatalf("delete: %d %s", removed.Code, removed.Body.String())
			}
		})
	}
}
