package auth

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "modernc.org/sqlite"
)

const testPassword = "StrongPass123"

func TestValidatePasswordPolicyAndUsername(t *testing.T) {
	passwordTests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "valid", value: testPassword},
		{name: "too short", value: "Short1", wantErr: true},
		{name: "missing digit", value: "StrongPassword", wantErr: true},
		{name: "missing letter", value: "1234567890", wantErr: true},
		{name: "too long", value: strings.Repeat("A", MaxPasswordLen) + "1", wantErr: true},
	}
	for _, tt := range passwordTests {
		t.Run("password "+tt.name, func(t *testing.T) {
			err := ValidatePasswordPolicy(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidatePasswordPolicy(%q) err=%v, wantErr=%v", tt.value, err, tt.wantErr)
			}
		})
	}

	usernameTests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "valid", value: "admin.user-1"},
		{name: "empty", value: " ", wantErr: true},
		{name: "too long", value: strings.Repeat("a", 65), wantErr: true},
		{name: "invalid char", value: "admin/user", wantErr: true},
	}
	for _, tt := range usernameTests {
		t.Run("username "+tt.name, func(t *testing.T) {
			err := ValidateUsername(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateUsername(%q) err=%v, wantErr=%v", tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestServiceSingleUserLifecycle(t *testing.T) {
	db := newTestDB(t)
	svc := NewService(ServiceOptions{
		DB: func() *sql.DB { return db },
		Now: func() time.Time {
			return time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
		},
	})
	required, err := svc.SetupRequired()
	if err != nil {
		t.Fatalf("SetupRequired() error = %v", err)
	}
	if !required {
		t.Fatalf("SetupRequired() = false, want true")
	}
	if ok, err := svc.Authenticate("admin", testPassword); ok || err != ErrSetupRequired {
		t.Fatalf("Authenticate(before setup) = %v, %v, want false/%v", ok, err, ErrSetupRequired)
	}
	if err := svc.CreateInitialUser("admin", testPassword); err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}
	if err := svc.CreateInitialUser("second", testPassword); err != ErrSetupAlreadyCompleted {
		t.Fatalf("CreateInitialUser(second) error = %v, want %v", err, ErrSetupAlreadyCompleted)
	}
	if ok, err := svc.Authenticate("admin", testPassword); err != nil || !ok {
		t.Fatalf("Authenticate(valid) = %v, %v, want true/nil", ok, err)
	}
	if ok, err := svc.Authenticate("admin", "wrong"); err != nil || ok {
		t.Fatalf("Authenticate(wrong) = %v, %v, want false/nil", ok, err)
	}
	if err := svc.ChangePassword(testPassword, "NewStrongPass123", "NewStrongPass123"); err != nil {
		t.Fatalf("ChangePassword() error = %v", err)
	}
	if ok, err := svc.Authenticate("admin", "NewStrongPass123"); err != nil || !ok {
		t.Fatalf("Authenticate(new password) = %v, %v, want true/nil", ok, err)
	}
}

func TestServiceSessionCountAndClear(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec("INSERT INTO sessions(token, data, expiry) VALUES('one', x'00', '2026-05-18T12:00:00Z')"); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	svc := NewService(ServiceOptions{DB: func() *sql.DB { return db }})
	count, err := svc.CountSessions()
	if err != nil {
		t.Fatalf("CountSessions() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("CountSessions() = %d, want 1", count)
	}
	deleted, err := svc.ClearSessions()
	if err != nil {
		t.Fatalf("ClearSessions() error = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("ClearSessions() = %d, want 1", deleted)
	}
}

func TestServiceSessionInventoryAndRevocation(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 27, 21, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`
		INSERT INTO sessions(token, data, expiry) VALUES
			('current-token', x'00', julianday('2026-08-27T21:00:00Z')),
			('other-token', x'00', julianday('2026-08-20T18:00:00Z'))
	`); err != nil {
		t.Fatalf("insert sessions: %v", err)
	}
	svc := NewService(ServiceOptions{
		DB:  func() *sql.DB { return db },
		Now: func() time.Time { return now },
		Encrypt: func(value string) (string, error) {
			return "encrypted:" + value, nil
		},
		Decrypt: func(value string) (string, error) {
			return strings.TrimPrefix(value, "encrypted:"), nil
		},
	})
	if err := svc.TouchSession("current-token", "192.168.4.55", "192.168.4.x", "Chrome · Windows"); err != nil {
		t.Fatalf("TouchSession() error = %v", err)
	}

	sessions, err := svc.ListSessions("current-token")
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("ListSessions() length = %d, want 2", len(sessions))
	}
	current := sessions[0]
	if !current.Current || current.ID == "" || current.ClientIP != "192.168.4.x" || current.ClientLabel != "Chrome · Windows" {
		t.Fatalf("current session = %+v", current)
	}
	if current.CreatedAt != now.Format(time.RFC3339) || current.LastSeenAt != now.Format(time.RFC3339) {
		t.Fatalf("current timestamps = %q / %q, want %q", current.CreatedAt, current.LastSeenAt, now.Format(time.RFC3339))
	}
	if current.ExpiresAt != "2026-08-27T21:00:00Z" {
		t.Fatalf("current expiry = %q", current.ExpiresAt)
	}
	revealedIP, found, err := svc.RevealSessionIP(current.ID)
	if err != nil || !found || revealedIP != "192.168.4.55" {
		t.Fatalf("RevealSessionIP() = %q, %v, %v", revealedIP, found, err)
	}
	if sessions[1].Current || sessions[1].ID == "" || sessions[1].ID == current.ID {
		t.Fatalf("other session = %+v", sessions[1])
	}

	revoked, err := svc.RevokeSession(sessions[1].ID)
	if err != nil || !revoked {
		t.Fatalf("RevokeSession() = %v, %v, want true/nil", revoked, err)
	}
	deleted, err := svc.ClearOtherSessions("current-token")
	if err != nil || deleted != 0 {
		t.Fatalf("ClearOtherSessions() = %d, %v, want 0/nil", deleted, err)
	}
	count, err := svc.CountSessions()
	if err != nil || count != 1 {
		t.Fatalf("CountSessions() = %d, %v, want 1/nil", count, err)
	}
}

func TestServiceClearOtherSessionsPreservesCurrent(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`
		INSERT INTO sessions(token, data, expiry) VALUES
			('current-token', x'00', julianday('2026-08-27T21:00:00Z')),
			('other-token-1', x'00', julianday('2026-08-20T18:00:00Z')),
			('other-token-2', x'00', julianday('2026-08-20T18:00:00Z'))
	`); err != nil {
		t.Fatalf("insert sessions: %v", err)
	}
	svc := NewService(ServiceOptions{DB: func() *sql.DB { return db }})
	deleted, err := svc.ClearOtherSessions("current-token")
	if err != nil || deleted != 2 {
		t.Fatalf("ClearOtherSessions() = %d, %v, want 2/nil", deleted, err)
	}
	var token string
	if err := db.QueryRow("SELECT token FROM sessions").Scan(&token); err != nil {
		t.Fatalf("load preserved session: %v", err)
	}
	if token != "current-token" {
		t.Fatalf("preserved token = %q", token)
	}
}

func TestNewSessionManagerPreservesCookieOptions(t *testing.T) {
	db := newTestDB(t)
	t.Setenv(SessionCookieSecureEnv, "true")
	t.Setenv(SessionIdleTimeoutHoursEnv, "2")
	sm, err := NewSessionManager(db, SessionManagerOptions{})
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	if sm.Cookie.Name != DefaultSessionCookieName || !sm.Cookie.HttpOnly || !sm.Cookie.Secure || !sm.Cookie.Persist || sm.Cookie.Path != "/" {
		t.Fatalf("unexpected cookie options: %+v", sm.Cookie)
	}
	if sm.IdleTimeout != 2*time.Hour {
		t.Fatalf("IdleTimeout = %s, want 2h", sm.IdleTimeout)
	}
}

func TestRateLimiterAllowLimitedAndRecordFailure(t *testing.T) {
	limiter := NewRateLimiter(time.Minute, 2)
	defer limiter.Stop()
	if !limiter.Allow("client") {
		t.Fatalf("first Allow() = false, want true")
	}
	if !limiter.Allow("client") {
		t.Fatalf("second Allow() = false, want true")
	}
	if limiter.Allow("client") {
		t.Fatalf("third Allow() = true, want false")
	}
	other := NewRateLimiter(time.Minute, 2)
	defer other.Stop()
	other.RecordFailure("client")
	other.RecordFailure("client")
	if !other.Limited("client") {
		t.Fatalf("Limited() = false, want true after recorded failures")
	}
}

func TestSameOriginRequestAndWriteMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	req.Host = "localhost"
	req.Header.Set("Origin", "http://localhost")
	req.Header.Set("Referer", "http://localhost/")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	ctx.Request = req
	if !SameOriginRequest(ctx) {
		t.Fatalf("SameOriginRequest() = false, want true")
	}

	router := gin.New()
	router.Use(SameOriginWriteMiddleware())
	router.POST("/write", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	blockedRec := httptest.NewRecorder()
	blockedReq := httptest.NewRequest(http.MethodPost, "/write", nil)
	blockedReq.Host = "localhost"
	router.ServeHTTP(blockedRec, blockedReq)
	if blockedRec.Code != http.StatusForbidden {
		t.Fatalf("blocked status = %d, want %d", blockedRec.Code, http.StatusForbidden)
	}
}

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ensureTestSchema(t, db)
	return db
}

func ensureTestSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	if err := EnsureSchema(db); err != nil {
		t.Fatalf("EnsureSchema() error = %v", err)
	}
}
