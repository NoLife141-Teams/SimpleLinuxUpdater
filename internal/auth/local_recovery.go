package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"debian-updater/internal/audit"
	"github.com/alexedwards/argon2id"
)

var ErrRecoveryAccountAbsent = errors.New("target account does not exist; recovery does not create accounts")
var ErrRecoveryFailed = errors.New("local recovery failed; account update, session revocation and audit were rolled back")
var ErrRecoveryCommit = errors.New("local recovery commit could not be confirmed; verify access before retrying")

type LocalRecoveryResult struct{ RevokedSessions int64 }

// ResetLocalPassword requires the caller to hold the installation process lock.
// It intentionally performs no schema creation, setup, or business-data migration.
func ResetLocalPassword(ctx context.Context, db *sql.DB, username, password, confirmation string) (LocalRecoveryResult, error) {
	var result LocalRecoveryResult
	username = strings.TrimSpace(username)
	if err := ValidateUsername(username); err != nil {
		return result, err
	}
	if password != confirmation {
		return result, ErrPasswordMismatch
	}
	if err := ValidatePasswordPolicy(password); err != nil {
		return result, err
	}
	if db == nil {
		return result, ErrRecoveryFailed
	}
	hash, err := argon2id.CreateHash(password, argon2id.DefaultParams)
	if err != nil {
		return result, ErrRecoveryFailed
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, ErrRecoveryFailed
	}
	defer tx.Rollback()
	var id int64
	if err = tx.QueryRowContext(ctx, "SELECT id FROM auth_users WHERE username = ?", username).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return result, ErrRecoveryAccountAbsent
	} else if err != nil {
		return result, ErrRecoveryFailed
	}
	var before, after int64
	if err = tx.QueryRowContext(ctx, "SELECT generation FROM auth_login_generation WHERE id=1").Scan(&before); err != nil {
		return result, ErrRecoveryFailed
	}
	now := time.Now().UTC().Format(time.RFC3339)
	updated, err := tx.ExecContext(ctx, "UPDATE auth_users SET password_hash=?,updated_at=? WHERE id=? AND username=?", hash, now, id, username)
	if err != nil {
		return result, ErrRecoveryFailed
	}
	if n, countErr := updated.RowsAffected(); countErr != nil || n != 1 {
		return result, ErrRecoveryFailed
	}
	// The established password trigger advances generation atomically. Fail closed
	// if that schema invariant is missing rather than accepting stale login data.
	if err = tx.QueryRowContext(ctx, "SELECT generation FROM auth_login_generation WHERE id=1").Scan(&after); err != nil || after <= before {
		return result, ErrRecoveryFailed
	}
	deleted, err := tx.ExecContext(ctx, "DELETE FROM sessions")
	if err != nil {
		return result, ErrRecoveryFailed
	}
	if result.RevokedSessions, err = deleted.RowsAffected(); err != nil {
		return LocalRecoveryResult{}, ErrRecoveryFailed
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM auth_session_metadata"); err != nil {
		return LocalRecoveryResult{}, ErrRecoveryFailed
	}
	meta := audit.SanitizeMeta(map[string]any{"recovery_method": "local_cli", "account_id": id, "sessions_revoked": result.RevokedSessions})
	if err = audit.WriteTx(tx, audit.Event{CreatedAt: now, Actor: "local-recovery", Action: "auth.password.recover", TargetType: "user", TargetName: username, Status: "success", Message: "Administrator password recovered locally; sessions revoked", MetaJSON: meta}); err != nil {
		return LocalRecoveryResult{}, ErrRecoveryFailed
	}
	if err = tx.Commit(); err != nil {
		return LocalRecoveryResult{}, ErrRecoveryCommit
	}
	return result, nil
}
