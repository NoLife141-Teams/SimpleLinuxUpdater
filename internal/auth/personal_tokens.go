package auth

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type PersonalToken struct {
	ID             string   `json:"id"`
	AccountID      int64    `json:"account_id"`
	Name           string   `json:"name"`
	Scopes         []string `json:"scopes"`
	CreatedAt      string   `json:"created_at"`
	ExpiresAt      string   `json:"expires_at"`
	Revoked        bool     `json:"revoked"`
	LastUsedAt     string   `json:"last_used_at,omitempty"`
	LastUsedOrigin string   `json:"last_used_origin,omitempty"`
}

func (s *Service) CreatePersonalToken(id int64, name string, scopes []string, expires time.Time) (PersonalToken, string, error) {
	var out PersonalToken
	a, err := s.Account(id)
	if err != nil {
		return out, "", err
	}
	name = strings.TrimSpace(name)
	if a.ID == 0 || a.Disabled || len(name) < 1 || len(name) > 80 || len(scopes) < 1 || len(scopes) > 9 || !expires.After(s.now()) || expires.After(s.now().Add(365*24*time.Hour)) {
		return out, "", NewValidationError("name, allowed scopes and expiry within one year are required")
	}
	seen := map[string]bool{}
	for _, scope := range scopes {
		if !RoleAllows(a.Role, scope) || !PersonalScope(scope) || seen[scope] {
			return out, "", NewValidationError("scope is not allowed by current role")
		}
		seen[scope] = true
	}
	tokenID, err := randomSecret(16)
	if err != nil {
		return out, "", err
	}
	secret, err := randomSecret(32)
	if err != nil {
		return out, "", err
	}
	secret = "slu_" + secret
	out = PersonalToken{ID: tokenID, AccountID: id, Name: name, Scopes: scopes, CreatedAt: s.now().UTC().Format(time.RFC3339), ExpiresAt: expires.UTC().Format(time.RFC3339)}
	encoded, _ := json.Marshal(scopes)
	result, err := s.db().Exec("INSERT INTO auth_personal_tokens(id,account_id,name,secret_hash,scopes,created_at,expires_at) SELECT ?,?,?,?,?,?,? FROM auth_users WHERE id=? AND disabled=0 AND role=? AND generation=?", out.ID, id, name, secretHash(secret), string(encoded), out.CreatedAt, out.ExpiresAt, id, a.Role, a.Generation)
	if err != nil {
		return PersonalToken{}, "", err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		return PersonalToken{}, "", ErrAuthenticationChanged
	}
	return out, secret, nil
}
func (s *Service) PersonalTokens(id int64) ([]PersonalToken, error) {
	rows, err := s.db().Query("SELECT id,account_id,name,scopes,created_at,expires_at,revoked,last_used_at,last_used_origin FROM auth_personal_tokens WHERE account_id=? ORDER BY created_at DESC,id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PersonalToken{}
	for rows.Next() {
		var t PersonalToken
		var scopes string
		if err = rows.Scan(&t.ID, &t.AccountID, &t.Name, &scopes, &t.CreatedAt, &t.ExpiresAt, &t.Revoked, &t.LastUsedAt, &t.LastUsedOrigin); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(scopes), &t.Scopes); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Service) RevokePersonalToken(id int64, tokenID string) (bool, error) {
	result, err := s.db().Exec("UPDATE auth_personal_tokens SET revoked=1 WHERE account_id=? AND id=?", id, tokenID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

var ErrTokenInvalid = errors.New("invalid bearer token")

func (s *Service) VerifyPersonalToken(secret, origin string) (Account, PersonalToken, error) {
	if !strings.HasPrefix(secret, "slu_") || len(secret) != 68 {
		return Account{}, PersonalToken{}, ErrTokenInvalid
	}
	var t PersonalToken
	var scopes string
	err := s.db().QueryRow("SELECT id,account_id,scopes,expires_at FROM auth_personal_tokens WHERE secret_hash=? AND revoked=0", secretHash(secret)).Scan(&t.ID, &t.AccountID, &scopes, &t.ExpiresAt)
	if err != nil {
		return Account{}, PersonalToken{}, ErrTokenInvalid
	}
	expires, err := time.Parse(time.RFC3339, t.ExpiresAt)
	if err != nil || !s.now().Before(expires) {
		return Account{}, PersonalToken{}, ErrTokenInvalid
	}
	a, err := s.Account(t.AccountID)
	if err != nil || a.ID == 0 || a.Disabled {
		return Account{}, PersonalToken{}, ErrTokenInvalid
	}
	if err = json.Unmarshal([]byte(scopes), &t.Scopes); err != nil {
		return Account{}, PersonalToken{}, ErrTokenInvalid
	}
	// Only masked origin facts reach storage. Transport owns IP interpretation.
	_, err = s.db().Exec("UPDATE auth_personal_tokens SET last_used_at=?,last_used_origin=? WHERE id=? AND revoked=0", s.now().UTC().Format(time.RFC3339), origin, t.ID)
	return a, t, err
}
func (t PersonalToken) Allows(a Account, scope string) bool {
	if !RoleAllows(a.Role, scope) {
		return false
	}
	for _, allowed := range t.Scopes {
		if allowed == scope {
			return true
		}
	}
	return false
}

// Personal tokens automate operational workflows; credential and installation
// administration remain in the cookie-authenticated human surface.
func PersonalScope(scope string) bool {
	switch scope {
	case "operational:read", "maintenance:write", "approvals:write", "logs:read", "audit:read":
		return true
	}
	return false
}
func PersonalScopesForRole(role string) []string {
	out := []string{}
	for _, scope := range RoleScopes[role] {
		if PersonalScope(scope) {
			out = append(out, scope)
		}
	}
	return out
}
