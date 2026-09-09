package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type regressionGatedAccount struct {
	authSessionAccount
	authenticated chan struct{}
	release       chan struct{}
}

func (a regressionGatedAccount) Authenticate(username, password string) (bool, error) {
	ok, err := a.authSessionAccount.Authenticate(username, password)
	close(a.authenticated)
	<-a.release
	return ok, err
}

type regressionGatedSession struct {
	scsAuthSessionLifecycle
	staged  chan struct{}
	release chan struct{}
}

func (s regressionGatedSession) Stage(ctx context.Context, username string) error {
	err := s.scsAuthSessionLifecycle.Stage(ctx, username)
	close(s.staged)
	<-s.release
	return err
}

func TestLoginRejectsObsoleteCredentialsAtSessionCommit(t *testing.T) {
	for _, phase := range []string{"verified", "staged"} {
		for _, invalidate := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/revoke=%v", phase, invalidate), func(t *testing.T) {
				testLoginRejectsObsoleteCredentialsAtSessionCommit(t, phase, invalidate)
			})
		}
	}
}

func testLoginRejectsObsoleteCredentialsAtSessionCommit(t *testing.T, phase string, invalidate bool) {
	app := newIsolatedTestApp(t)
	oldPassword := "ReviewOldPass123"
	if err := app.Deps.AuthService.CreateInitialUser("reviewadmin", oldPassword); err != nil {
		t.Fatal(err)
	}
	ownerReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"reviewadmin","password":"ReviewOldPass123"}`))
	ownerReq.Header.Set("Content-Type", "application/json")
	markSameOriginAuthRequest(ownerReq)
	ownerRec := httptest.NewRecorder()
	app.Handler.ServeHTTP(ownerRec, ownerReq)
	if ownerRec.Code != http.StatusOK {
		t.Fatal("owner login failed")
	}
	gated := regressionGatedAccount{authSessionAccount: app.Deps.AuthService, authenticated: make(chan struct{}), release: make(chan struct{})}
	// Pause only after real Argon2 verification; all session operations and persistence remain production implementations.
	if phase == "verified" {
		app.Deps.AuthSessionCommands.deps.Account = gated
	} else {
		app.Deps.AuthSessionCommands.deps.Session = regressionGatedSession{
			scsAuthSessionLifecycle: app.Deps.AuthSessionCommands.deps.Session.(scsAuthSessionLifecycle), staged: gated.authenticated, release: gated.release,
		}
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"reviewadmin","password":"ReviewOldPass123"}`))
	req.Header.Set("Content-Type", "application/json")
	markSameOriginAuthRequest(req)
	done := make(chan struct{})
	go func() { defer close(done); app.Handler.ServeHTTP(recorder, req) }()
	select {
	case <-gated.authenticated:
	case <-time.After(5 * time.Second):
		t.Fatal("login did not reach gate")
	}
	rotationReq := httptest.NewRequest(http.MethodPut, "/api/auth/password", strings.NewReader(fmt.Sprintf(`{"current_password":"ReviewOldPass123","new_password":"ReviewNewPass456","confirm_password":"ReviewNewPass456","invalidate_other_sessions":%t}`, invalidate)))
	rotationReq.Header.Set("Content-Type", "application/json")
	markSameOriginAuthRequest(rotationReq)
	for _, cookie := range ownerRec.Result().Cookies() {
		rotationReq.AddCookie(cookie)
	}
	rotationRec := httptest.NewRecorder()
	app.Handler.ServeHTTP(rotationRec, rotationReq)
	close(gated.release)
	<-done
	if rotationRec.Code != http.StatusOK {
		t.Fatalf("password rotation failed: %d %s", rotationRec.Code, rotationRec.Body.String())
	}
	if ok, err := app.Deps.AuthService.Authenticate("reviewadmin", oldPassword); ok || err != nil {
		t.Fatalf("old password not invalidated: %v %v", ok, err)
	}
	t.Logf("rotation HTTP=%d; in-flight old-password login HTTP=%d", rotationRec.Code, recorder.Code)
	check := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	for _, cookie := range recorder.Result().Cookies() {
		check.AddCookie(cookie)
	}
	checked := httptest.NewRecorder()
	app.Handler.ServeHTTP(checked, check)
	t.Logf("protected API using late session HTTP=%d", checked.Code)
	if checked.Code != http.StatusUnauthorized {
		t.Fatal("old-password login must be unauthorized after password rotation")
	}
}
