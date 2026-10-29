package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAccessDeniedHTMLNegotiation(t *testing.T) {
	for _, tc := range []struct {
		accept string
		html   bool
	}{
		{"", false}, {"*/*", false}, {"text/*", false}, {"application/json", false},
		{"text/html", true}, {"TEXT/HTML", true},
		{"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", true},
		{"text/html;q=0", false}, {"text/html;q=0,*/*;q=1", false},
		{"text/html;q=0.5,application/json;q=0.9", false},
		{"text/html;q=0.9,application/json;q=0.5", true},
		{"text/html,application/json", false},
		{"text/html;q=0.5,*/*;q=1", false},
		{"text/html,application/json;q=0,*/*;q=1", true},
		{"text/html;q=bogus", false}, {"text/html;q=NaN", false},
		{"text/html;q=1.1", false}, {"text/html;q=-0.5", false},
	} {
		t.Run(tc.accept, func(t *testing.T) {
			if got := acceptsAccessDeniedHTML(tc.accept); got != tc.html {
				t.Fatalf("got %v want %v", got, tc.html)
			}
		})
	}
}

func accessDeniedRequest(app *testApp, path, accept string, cookie *http.Cookie, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Accept", accept)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	app.Handler.ServeHTTP(rec, req)
	return rec
}

func TestAccessDeniedPagesUseAuthenticatedRoleAndKeepTechnicalContracts(t *testing.T) {
	app := newIsolatedTestApp(t)
	admin := app.authenticate(t)
	for _, role := range []string{"admin", "operator", "reader"} {
		cookie := admin
		id := int64(1)
		if role != "admin" {
			a, err := app.Deps.AuthService.CreateAccount(role, testPasswordStrong, role)
			if err != nil {
				t.Fatal(err)
			}
			id = a.ID
			cookie = identityLogin(t, app, role)
		}
		_, token, err := app.Deps.AuthService.CreatePersonalToken(id, "read-only", []string{"operational:read"}, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		for _, route := range []string{"/admin", "/manage"} {
			for _, accept := range []string{"text/html", "application/json", "*/*", "", "text/html;q=0", "text/html;q=0.2,application/json"} {
				rec := accessDeniedRequest(app, route, accept, cookie, "")
				want := 403
				if role == "admin" {
					want = 200
				}
				if rec.Code != want {
					t.Fatalf("%s %s %q: %d", role, route, accept, rec.Code)
				}
				if role == "admin" {
					continue
				}
				if accept == "text/html" {
					body := rec.Body.String()
					for _, fragment := range []string{`<title>SimpleLinuxUpdater - Access denied</title>`, `<h1 id="access-denied-heading">Access denied</h1>`, `data-account-role="` + role + `"`, `href="/account"`, `href="/"`} {
						if !strings.Contains(body, fragment) {
							t.Errorf("missing %q in %s refusal", fragment, role)
						}
					}
					for _, fragment := range []string{`href="/admin"`, `href="/manage"`, `/static/js/admin.js`, `/static/js/manage.js`} {
						if strings.Contains(body, fragment) {
							t.Errorf("privileged content in %s refusal: %q", role, fragment)
						}
					}
					if !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") || !strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), "Accept") {
						t.Fatal("refusal may be cached without representation boundary")
					}
					assertSecurityHeaders(t, rec.Header(), false)
				} else {
					assertPermissionDeniedJSON(t, rec)
				}
				bearer := accessDeniedRequest(app, route, accept, nil, token)
				if bearer.Code != 401 || !strings.HasPrefix(bearer.Header().Get("Content-Type"), "application/json") {
					t.Fatal("Bearer accepted on human page")
				}
			}
		}
		// Valid admin token with insufficient scopes must also remain JSON403.
		for _, accept := range []string{"text/html", "application/json", "*/*"} {
			assertPermissionDeniedJSON(t, accessDeniedRequest(app, "/api/jobs/missing", accept, nil, token))
			assertPermissionDeniedJSON(t, accessDeniedRequest(app, "/unknown-page", accept, cookie, ""))
			req := httptest.NewRequest("POST", "/admin", nil)
			req.Header.Set("Accept", accept)
			req.AddCookie(cookie)
			markSameOriginAuthRequest(req)
			unsupported := httptest.NewRecorder()
			app.Handler.ServeHTTP(unsupported, req)
			assertPermissionDeniedJSON(t, unsupported)
			unknownBearer := accessDeniedRequest(app, "/unknown-page", accept, nil, token)
			if unknownBearer.Code != 401 || !strings.HasPrefix(unknownBearer.Header().Get("Content-Type"), "application/json") {
				t.Fatal("unknown Bearer route changed")
			}
		}
	}
	for _, accept := range []string{"text/html", "application/json", "*/*"} {
		for _, route := range []string{"/admin", "/manage", "/unknown-page"} {
			page := accessDeniedRequest(app, route, accept, nil, "")
			if page.Code != 302 || page.Header().Get("Location") != "/login" {
				t.Fatal("anonymous page admission changed")
			}
		}
		api := accessDeniedRequest(app, "/api/auth/accounts", accept, nil, "")
		if api.Code != 401 || !strings.HasPrefix(api.Header().Get("Content-Type"), "application/json") {
			t.Fatal("anonymous API admission changed")
		}
	}
}

func assertPermissionDeniedJSON(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var payload map[string]any
	if rec.Code != 403 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || json.Unmarshal(rec.Body.Bytes(), &payload) != nil || len(payload) != 1 || payload["error"] != "permission denied" {
		t.Fatalf("JSON refusal changed: %d %s", rec.Code, rec.Body.String())
	}
}
