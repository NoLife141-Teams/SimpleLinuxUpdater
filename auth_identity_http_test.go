package main

import (
	"bufio"
	"bytes"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authpkg "debian-updater/internal/auth"
	"debian-updater/internal/events"
	"github.com/gin-gonic/gin"
)

func identityRequest(app *testApp, method, path string, cookie *http.Cookie, token string, body any) *httptest.ResponseRecorder {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
		markSameOriginAuthRequest(req)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	app.Handler.ServeHTTP(rec, req)
	return rec
}
func identityLogin(t *testing.T, app *testApp, username string) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"`+username+`","password":"`+testPasswordStrong+`"}`))
	req.Header.Set("Content-Type", "application/json")
	markSameOriginAuthRequest(req)
	rec := httptest.NewRecorder()
	app.Handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("login %s: %d %s", username, rec.Code, rec.Body.String())
	}
	return testSessionCookieFromRecorder(t, rec)
}

func TestIdentityStatusPageExposesAuthenticatedRole(t *testing.T) {
	app := newIsolatedTestApp(t)
	admin := app.authenticate(t)
	for _, role := range []string{"reader", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			cookie := admin
			if role != "admin" {
				if _, err := app.Deps.AuthService.CreateAccount(role, testPasswordStrong, role); err != nil {
					t.Fatal(err)
				}
				cookie = identityLogin(t, app, role)
			}
			rec := identityRequest(app, "GET", "/", cookie, "", nil)
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), `data-account-role="`+role+`"`) {
				t.Fatalf("Status page does not expose authenticated role %q (HTTP %d)", role, rec.Code)
			}
		})
	}
}

func TestIdentityEveryRoutePermissionMatrixWithRealCookiesAndPersonalTokens(t *testing.T) {
	app := newIsolatedTestApp(t)
	adminCookie := app.authenticate(t)
	cookies := map[string]*http.Cookie{"admin": adminCookie}
	tokens := map[string]string{}
	for _, role := range []string{"reader", "operator", "admin"} {
		name := "admin"
		id := int64(1)
		if role != "admin" {
			name = role
			a, err := app.Deps.AuthService.CreateAccount(name, testPasswordStrong, role)
			if err != nil {
				t.Fatal(err)
			}
			id = a.ID
			cookies[role] = identityLogin(t, app, name)
		}
		_, secret, err := app.Deps.AuthService.CreatePersonalToken(id, "matrix", authpkg.PersonalScopesForRole(role), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		tokens[role] = secret
	}
	// Exercise the production authentication/authorization middleware with a
	// transport probe after admission; no authorization or session is mocked.
	probe := gin.New()
	probe.HTMLRender = app.Router.HTMLRender
	probe.Use(authRuntimeMiddleware(app.Deps), authGateMiddleware(), sameOriginWriteMiddleware())
	public := map[string]bool{"GET /setup": true, "GET /login": true, "POST /api/auth/setup": true, "POST /api/auth/login": true, "GET /api/auth/status": true, "POST /api/auth/mfa/challenge": true, "GET /api/maintenance": true, "GET /metrics": true}
	count := 0
	for _, route := range app.Router.Routes() {
		key := route.Method + " " + route.Path
		if strings.HasPrefix(route.Path, "/static/") || public[key] {
			continue
		}
		if _, ok := routePermissions[key]; !ok {
			t.Errorf("protected route has no explicit permission: %s", key)
			continue
		}
		count++
		probe.Handle(route.Method, route.Path, func(c *gin.Context) { c.Status(204) })
	}
	probe.GET("/api/undeclared", func(c *gin.Context) { t.Error("undeclared route reached handler"); c.Status(204) })
	handler := app.Deps.CurrentSessionManager().LoadAndSave(probe)
	for i, route := range probe.Routes() {
		path := route.Path
		for _, parameter := range []string{":name", ":server", ":id", ":revision"} {
			path = strings.ReplaceAll(path, parameter, "missing")
		}
		permission, declared := routePermissions[route.Method+" "+route.Path]
		for _, role := range []string{"reader", "operator", "admin"} {
			for _, transport := range []string{"cookie", "bearer", "anonymous"} {
				label := fmt.Sprintf("%s %s/%s/%s", route.Method, route.Path, role, transport)
				req := httptest.NewRequest(route.Method, path, nil)
				req.RemoteAddr = fmt.Sprintf("198.51.%d.%d:10000", i/200, i%200+1)
				want := 204
				switch transport {
				case "cookie":
					req.AddCookie(cookies[role])
					markSameOriginAuthRequest(req)
					if !declared || (permission.Scope != "self" && !authpkg.RoleAllows(role, permission.Scope)) {
						want = 403
					}
				case "bearer":
					req.Header.Set("Authorization", "Bearer "+tokens[role])
					if !declared || !permission.Bearer {
						want = 401
					} else if !authpkg.RoleAllows(role, permission.Scope) {
						want = 403
					}
				case "anonymous":
					want = 401
					if !strings.HasPrefix(path, "/api/") {
						want = 302
					}
				}
				accepts := []string{""}
				if want == 403 || (transport == "bearer" && want == 401) {
					accepts = append(accepts, "application/json", "text/html", "*/*")
				}
				for _, accept := range accepts {
					request := req.Clone(req.Context())
					request.Header.Set("Accept", accept)
					rec := httptest.NewRecorder()
					handler.ServeHTTP(rec, request)
					if rec.Code != want {
						t.Errorf("%s Accept=%q: status %d want %d; %s", label, accept, rec.Code, want, rec.Body.String())
					}
					if want != 403 && !(transport == "bearer" && want == 401) {
						continue
					}
					humanHTML := want == 403 && transport == "cookie" && accept == "text/html" && route.Method == "GET" && (route.Path == "/admin" || route.Path == "/manage")
					if humanHTML {
						if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "Access denied") {
							t.Errorf("%s: missing human refusal", label)
						}
					} else {
						var payload map[string]any
						if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || json.Unmarshal(rec.Body.Bytes(), &payload) != nil {
							t.Errorf("%s Accept=%q: technical refusal changed format", label, accept)
						} else if want == 403 && (len(payload) != 1 || payload["error"] != "permission denied") {
							t.Errorf("%s: technical refusal changed body: %v", label, payload)
						}
					}
				}
			}
		}
	}
	if count < 80 {
		t.Fatalf("only %d routes covered", count)
	}
	if rec := identityRequest(app, "POST", "/api/auth/logout", nil, tokens["admin"], nil); rec.Code != 401 {
		t.Fatal("bearer accepted on logout", rec.Code)
	}
	if rec := identityRequest(app, "POST", "/api/backup/restore", nil, tokens["admin"], nil); rec.Code != 401 {
		t.Fatal("bearer accepted on identity-replacing restore", rec.Code)
	}
}
func TestIdentityHTTPAccountIsolationPasswordAndImmediateDowngrade(t *testing.T) {
	app := newIsolatedTestApp(t)
	admin := app.authenticate(t)
	reader, err := app.Deps.AuthService.CreateAccount("reader", testPasswordStrong, "reader")
	if err != nil {
		t.Fatal(err)
	}
	cookie := identityLogin(t, app, "reader")
	sessions := decodeContractJSON(t, identityRequest(app, "GET", "/api/auth/sessions", admin, "", nil))["sessions"].([]any)
	adminID := sessions[0].(map[string]any)["id"].(string)
	if rec := identityRequest(app, "DELETE", "/api/auth/sessions/"+adminID, cookie, "", nil); rec.Code != 404 {
		t.Fatalf("cross-account revoke %d %s", rec.Code, rec.Body.String())
	}
	if rec := identityRequest(app, "POST", "/api/auth/sessions/"+adminID+"/reveal-ip", cookie, "", gin.H{"current_password": testPasswordStrong}); rec.Code != 404 {
		t.Fatal("cross-account reveal", rec.Code)
	}
	if rec := identityRequest(app, "PUT", "/api/auth/password", cookie, "", gin.H{"current_password": testPasswordStrong, "new_password": "NamedAccountNew123!", "confirm_password": "NamedAccountNew123!", "invalidate_other_sessions": true}); rec.Code != 200 {
		t.Fatal("own password", rec.Code, rec.Body.String())
	}
	if ok, err := app.Deps.AuthService.Authenticate("admin", testPasswordStrong); err != nil || !ok {
		t.Fatal("another account password changed", err)
	}
	if rec := identityRequest(app, "DELETE", "/api/auth/sessions", cookie, "", nil); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if rec := identityRequest(app, "GET", "/api/servers", admin, "", nil); rec.Code != 200 {
		t.Fatal("another account session revoked", rec.Code)
	}
	if rec := identityRequest(app, "PUT", "/api/auth/accounts/1", admin, "", gin.H{"role": "reader", "disabled": false}); rec.Code != 409 {
		t.Fatal("last admin protection", rec.Code)
	}
	operator, err := app.Deps.AuthService.CreateAccount("operator", testPasswordStrong, "operator")
	if err != nil {
		t.Fatal(err)
	}
	opCookie := identityLogin(t, app, "operator")
	_, secret, err := app.Deps.AuthService.CreatePersonalToken(operator.ID, "maintenance", []string{"operational:read", "maintenance:write"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = app.Deps.AuthService.UpdateAccount(operator.ID, "reader", false); err != nil {
		t.Fatal(err)
	}
	if rec := identityRequest(app, "POST", "/api/update/missing", opCookie, "", nil); rec.Code != 401 {
		t.Fatal("downgraded cookie retained", rec.Code)
	}
	if rec := identityRequest(app, "POST", "/api/update/missing", nil, secret, nil); rec.Code != 403 {
		t.Fatal("downgraded token retained maintenance scope", rec.Code)
	}
	if err = app.Deps.AuthService.UpdateAccount(reader.ID, "reader", true); err != nil {
		t.Fatal(err)
	}
}
func TestIdentityHTTPMFAChallengeSessionRateLimitReauthenticationAndSecretRedaction(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	enroll := identityRequest(app, "POST", "/api/auth/mfa/enroll", cookie, "", nil)
	if enroll.Code != 200 {
		t.Fatal(enroll.Code, enroll.Body.String())
	}
	secret := decodeContractJSON(t, enroll)["secret"].(string)
	raw, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	code := authpkg.HOTP(raw, uint64(time.Now().Unix()/30), 6)
	confirmed := identityRequest(app, "POST", "/api/auth/mfa/confirm", cookie, "", gin.H{"code": code})
	if confirmed.Code != 200 {
		t.Fatal(confirmed.Code, confirmed.Body.String())
	}
	codes := decodeContractJSON(t, confirmed)["recovery_codes"].([]any)
	if rec := identityRequest(app, "GET", "/api/servers", cookie, "", nil); rec.Code != 401 {
		t.Fatal("enrollment did not revoke session", rec.Code)
	}
	login := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"admin","password":"`+testPasswordStrong+`"}`))
		req.Header.Set("Content-Type", "application/json")
		markSameOriginAuthRequest(req)
		rec := httptest.NewRecorder()
		app.Handler.ServeHTTP(rec, req)
		return rec
	}
	pending := login()
	if pending.Code != 202 {
		t.Fatal(pending.Code, pending.Body.String())
	}
	challenge := decodeContractJSON(t, pending)["challenge"].(string)
	for _, cookie := range pending.Result().Cookies() {
		if cookie.Name == authpkg.DefaultSessionCookieName && cookie.Value != "" {
			t.Fatal("incomplete MFA produced a session")
		}
	}
	verify := func(challenge, code string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/auth/mfa/challenge", strings.NewReader(fmt.Sprintf(`{"challenge":%q,"code":%q}`, challenge, code)))
		req.Header.Set("Content-Type", "application/json")
		markSameOriginAuthRequest(req)
		rec := httptest.NewRecorder()
		app.Handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := verify(challenge, code); rec.Code != 401 {
		t.Fatal("replayed enrollment OTP authenticated", rec.Code)
	}
	pending = login()
	challenge = decodeContractJSON(t, pending)["challenge"].(string)
	complete := verify(challenge, codes[0].(string))
	if complete.Code != 200 {
		t.Fatal(complete.Code, complete.Body.String())
	}
	authenticated := testSessionCookieFromRecorder(t, complete)
	if rec := verify(challenge, codes[1].(string)); rec.Code != 401 {
		t.Fatal("challenge reused", rec.Code)
	}
	pending = login()
	if rec := verify(decodeContractJSON(t, pending)["challenge"].(string), codes[0].(string)); rec.Code != 401 {
		t.Fatal("recovery code reused", rec.Code)
	}
	tokenResp := identityRequest(app, "POST", "/api/auth/tokens", authenticated, "", gin.H{"name": "one time", "scopes": []string{"operational:read"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	if tokenResp.Code != 201 {
		t.Fatal(tokenResp.Code, tokenResp.Body.String())
	}
	tokenSecret := decodeContractJSON(t, tokenResp)["secret"].(string)
	listed := identityRequest(app, "GET", "/api/auth/tokens", authenticated, "", nil)
	if strings.Contains(listed.Body.String(), tokenSecret) || strings.Contains(listed.Body.String(), secret) {
		t.Fatal("token/factor secret returned by listing")
	}
	var stored string
	if err := app.Deps.DB().QueryRow("SELECT totp_secret_enc FROM auth_users WHERE id=1").Scan(&stored); err != nil || strings.Contains(stored, secret) {
		t.Fatal("MFA secret unencrypted", err)
	}
	// Expire only the recent-confirmation flag in an actual authenticated session.
	sm := app.Deps.CurrentSessionManager()
	ctx, err := sm.Load(t.Context(), authenticated.Value)
	if err != nil {
		t.Fatal(err)
	}
	sm.Put(ctx, "auth_reauthenticated_at", time.Now().Add(-6*time.Minute).Unix())
	if _, _, err = sm.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rec := identityRequest(app, "POST", "/api/auth/tokens", authenticated, "", gin.H{"name": "blocked"})
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "reauthentication_required") {
		t.Fatal("stale reauthentication accepted", rec.Code, rec.Body.String())
	}
	for i := 0; i < authpkg.PasswordChangeMaxAttempts+1; i++ {
		rec = identityRequest(app, "POST", "/api/auth/reauthenticate", authenticated, "", gin.H{"password": testPasswordStrong, "code": "wrong"})
	}
	if rec.Code != 429 {
		t.Fatal("MFA reauthentication not rate limited", rec.Code)
	}
	if rec = identityRequest(app, "GET", "/metrics", nil, tokenSecret, nil); rec.Code == 200 {
		t.Fatal("PAT was accepted as metrics credential")
	}
}
func TestIdentitySSEReaderRedactionAndLiveDowngrade(t *testing.T) {
	for _, cause := range []string{"reader", "downgrade", "disabled"} {
		t.Run(cause, func(t *testing.T) {
			app := newIsolatedTestApp(t)
			_ = app.authenticate(t)
			role := "operator"
			if cause == "reader" {
				role = "reader"
			}
			a, err := app.Deps.AuthService.CreateAccount("stream", testPasswordStrong, role)
			if err != nil {
				t.Fatal(err)
			}
			cookie := identityLogin(t, app, "stream")
			server := httptest.NewServer(app.Handler)
			defer server.Close()
			req, _ := http.NewRequest("GET", server.URL+"/api/dashboard/events", nil)
			req.AddCookie(cookie)
			client := server.Client()
			client.Timeout = 5 * time.Second
			response, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			reader := bufio.NewReader(response.Body)
			readDashboardEventUntil(t, reader, `"connected"`)
			if cause != "reader" {
				if err = app.Deps.AuthService.UpdateAccount(a.ID, "reader", cause == "disabled"); err != nil {
					t.Fatal(err)
				}
			}
			app.Deps.DashboardEventBroker.PublishEvent(events.Event{Reason: "job.log", ServerName: "synthetic-host", Data: "PRIVATE_COMMAND_OUTPUT"})
			if cause == "reader" {
				readDashboardEventUntil(t, reader, `"job.log"`)
				response.Body.Close()
			} else {
				data, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), "PRIVATE_COMMAND_OUTPUT") {
					t.Fatal("revoked stream leaked future log")
				}
			}
		})
	}
}

func TestIdentityFailedLoginCannotImpersonateAccountNamedUnknownInAudit(t *testing.T) {
	app := newIsolatedTestApp(t)
	_ = app.authenticate(t)
	a, err := app.Deps.AuthService.CreateAccount("unknown", testPasswordStrong, "reader")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"admin","password":"WrongPassword123!"}`))
	req.Header.Set("Content-Type", "application/json")
	markSameOriginAuthRequest(req)
	rec := httptest.NewRecorder()
	app.Handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var actor string
	if err = app.Deps.DB().QueryRow("SELECT actor FROM audit_events WHERE action='auth.login' AND status='failure' ORDER BY id DESC LIMIT 1").Scan(&actor); err != nil || actor != "unknown" {
		t.Fatal("unverified login attributed to named account", actor, err)
	}
	_ = identityLogin(t, app, "unknown")
	if err = app.Deps.DB().QueryRow("SELECT actor FROM audit_events WHERE action='auth.login' AND status='success' ORDER BY id DESC LIMIT 1").Scan(&actor); err != nil || actor != "account:"+formatAccountID(a.ID) {
		t.Fatal("verified named account lost stable audit identity", actor, err)
	}
}
