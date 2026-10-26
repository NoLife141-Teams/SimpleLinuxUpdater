package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/alexedwards/argon2id"
)

const SessionAccountKey = "auth_account_id"
const SessionGenerationKey = "auth_account_generation"

type Account struct {
	ID              int64  `json:"id"`
	Username        string `json:"username"`
	Role            string `json:"role"`
	Disabled        bool   `json:"disabled"`
	MFAEnabled      bool   `json:"mfa_enabled"`
	Generation      int64  `json:"-"`
	LoginGeneration int64  `json:"-"`
	PasswordHash    string `json:"-"`
}

func (s *Service) ForAccount(id int64) *Service {
	clone := *s
	clone.accountID = id
	if r, ok := s.repo.(*SQLiteRepository); ok {
		repo := *r
		repo.accountID = id
		clone.repo = &repo
	}
	return &clone
}

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Username, &a.Role, &a.Disabled, &a.Generation, &a.PasswordHash, &a.MFAEnabled, &a.LoginGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, nil
	}
	return a, err
}

const accountColumns = "id,username,role,disabled,generation,password_hash,(totp_secret_enc!=''),login_generation"

type preparedAccountKey struct{}

// Capture the account epoch before password verification. Changes to another
// identity have no effect on this login or MFA challenge.
func (s *Service) PrepareAccountAuthentication(ctx context.Context, username string) (context.Context, error) {
	a, err := s.AccountByName(username)
	if err != nil {
		return ctx, err
	}
	return WithPreparedAccount(ctx, a), nil
}
func WithPreparedAccount(ctx context.Context, a Account) context.Context {
	return context.WithValue(ctx, preparedAccountKey{}, a)
}

func (s *Service) AccountByName(name string) (Account, error) {
	return scanAccount(s.db().QueryRow("SELECT "+accountColumns+" FROM auth_users WHERE username=?", strings.TrimSpace(name)))
}
func (s *Service) Account(id int64) (Account, error) {
	return scanAccount(s.db().QueryRow("SELECT "+accountColumns+" FROM auth_users WHERE id=?", id))
}
func (s *Service) Accounts() ([]Account, error) {
	rows, err := s.db().Query("SELECT " + accountColumns + " FROM auth_users ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func validRole(role string) bool { return role == "reader" || role == "operator" || role == "admin" }
func (s *Service) CreateAccount(username, password, role string) (Account, error) {
	username = strings.TrimSpace(username)
	if err := ValidateUsername(username); err != nil {
		return Account{}, err
	}
	if err := ValidatePasswordPolicy(password); err != nil {
		return Account{}, err
	}
	if !validRole(role) {
		return Account{}, NewValidationError("invalid role")
	}
	hash, err := argon2id.CreateHash(password, argon2id.DefaultParams)
	if err != nil {
		return Account{}, err
	}
	now := s.now().UTC().Format(time.RFC3339)
	result, err := s.db().Exec("INSERT INTO auth_users(username,password_hash,role,created_at,updated_at) VALUES(?,?,?,?,?)", username, hash, role, now, now)
	if err != nil {
		return Account{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Account{}, err
	}
	return s.Account(id)
}
func (s *Service) UpdateAccount(id int64, role string, disabled bool) error {
	if !validRole(role) {
		return NewValidationError("invalid role")
	}
	result, err := s.db().Exec("UPDATE auth_users SET role=?,disabled=?,updated_at=? WHERE id=?", role, disabled, s.now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return NewValidationError("account not found")
	}
	return err
}

func randomSecret(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func secretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

var RoleScopes = map[string][]string{
	"reader":   {"operational:read"},
	"operator": {"operational:read", "maintenance:write", "approvals:write", "logs:read"},
	"admin":    {"operational:read", "maintenance:write", "approvals:write", "logs:read", "inventory:write", "settings:write", "policies:write", "audit:read", "backup:write"},
}

func RoleAllows(role, scope string) bool {
	for _, allowed := range RoleScopes[role] {
		if scope == allowed {
			return true
		}
	}
	return false
}

// RewrapFactors is used by Backup Operation Lifecycle before persistence replacement.
type factorDB interface {
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
	Exec(string, ...any) (sql.Result, error)
}

func RewrapFactors(db factorDB, decrypt, encrypt func(string) (string, error)) error {
	var exists int
	if err := db.QueryRow("SELECT count(*) FROM pragma_table_info('auth_users') WHERE name='totp_secret_enc'").Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	rows, err := db.Query("SELECT id,totp_secret_enc,totp_pending_enc FROM auth_users")
	if err != nil {
		return err
	}
	type factor struct {
		id              int64
		secret, pending string
	}
	var factors []factor
	for rows.Next() {
		var f factor
		if err = rows.Scan(&f.id, &f.secret, &f.pending); err != nil {
			rows.Close()
			return err
		}
		factors = append(factors, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, f := range factors {
		values := []string{f.secret, f.pending}
		for i, v := range values {
			if v != "" {
				plain, e := decrypt(v)
				if e != nil {
					return e
				}
				values[i], e = encrypt(plain)
				if e != nil {
					return e
				}
			}
		}
		if _, err = db.Exec("UPDATE auth_users SET totp_secret_enc=?,totp_pending_enc=? WHERE id=?", values[0], values[1], f.id); err != nil {
			return err
		}
	}
	return nil
}
