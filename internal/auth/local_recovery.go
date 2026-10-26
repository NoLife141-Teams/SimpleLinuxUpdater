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
	return ResetLocalFactors(ctx, db, username, password, confirmation, true, false)
}

// ResetLocalFactors resets only the explicitly selected account and factors.
// The caller holds the existing exclusive installation lock throughout.
func ResetLocalFactors(ctx context.Context, db *sql.DB, username, password, confirmation string, resetPassword, resetMFA bool) (LocalRecoveryResult, error) {
	var result LocalRecoveryResult
	if !resetPassword && !resetMFA {
		return result, NewValidationError("select at least one recovery factor")
	}
	username = strings.TrimSpace(username)
	if err := ValidateUsername(username); err != nil {
		return result, err
	}
	if resetPassword && password != confirmation {
		return result, ErrPasswordMismatch
	}
	if err := ValidatePasswordPolicy(password); resetPassword && err != nil {
		return result, err
	}
	if db == nil {
		return result, ErrRecoveryFailed
	}
	var hash string
	var err error
	if resetPassword {
		hash, err = argon2id.CreateHash(password, argon2id.DefaultParams)
	}
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
	if resetPassword {
		updated, err := tx.ExecContext(ctx, "UPDATE auth_users SET password_hash=?,updated_at=? WHERE id=? AND username=?", hash, now, id, username)
		if err != nil {
			return result, ErrRecoveryFailed
		}
		if n, e := updated.RowsAffected(); e != nil || n != 1 {
			return result, ErrRecoveryFailed
		}
		if err = tx.QueryRowContext(ctx, "SELECT generation FROM auth_login_generation WHERE id=1").Scan(&after); err != nil || after <= before {
			return result, ErrRecoveryFailed
		}
	}
	var modern int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('auth_users') WHERE name='role'").Scan(&modern); err != nil {
		return result, ErrRecoveryFailed
	}
	if resetMFA {
		if modern == 0 {
			return result, ErrRecoveryFailed
		}
		if _, err = tx.ExecContext(ctx, "UPDATE auth_users SET totp_secret_enc='',totp_pending_enc='',totp_pending_until=0,totp_last_step=-1,generation=generation+1 WHERE id=?", id); err != nil {
			return result, ErrRecoveryFailed
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM auth_recovery_codes WHERE account_id=?", id); err != nil {
			return result, ErrRecoveryFailed
		}
	}
	deleteQuery := "DELETE FROM sessions"
	var deleteArgs []any
	if modern != 0 {
		deleteQuery += " WHERE account_id=?"
		deleteArgs = []any{id}
	}
	deleted, err := tx.ExecContext(ctx, deleteQuery, deleteArgs...)
	if err != nil {
		return result, ErrRecoveryFailed
	}
	if result.RevokedSessions, err = deleted.RowsAffected(); err != nil {
		return LocalRecoveryResult{}, ErrRecoveryFailed
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM auth_session_metadata WHERE token NOT IN (SELECT token FROM sessions)"); err != nil {
		return LocalRecoveryResult{}, ErrRecoveryFailed
	}
	if modern != 0 {
		if _, err = tx.ExecContext(ctx, "DELETE FROM auth_challenges WHERE account_id=?", id); err != nil {
			return result, ErrRecoveryFailed
		}
		if _, err = tx.ExecContext(ctx, "UPDATE auth_personal_tokens SET revoked=1 WHERE account_id=?", id); err != nil {
			return result, ErrRecoveryFailed
		}
		if _, err = tx.ExecContext(ctx, "UPDATE auth_login_generation SET generation=generation+1 WHERE id=1 AND ?=0", resetPassword); err != nil {
			return result, ErrRecoveryFailed
		}
	}
	meta := audit.SanitizeMeta(map[string]any{"recovery_method": "local_cli", "account_id": id, "sessions_revoked": result.RevokedSessions, "primary_factor_reset": resetPassword, "factor_mfa_reset": resetMFA})
	if err = audit.WriteTx(tx, audit.Event{CreatedAt: now, Actor: "local-recovery", Action: "auth.password.recover", TargetType: "user", TargetName: username, Status: "success", Message: "Selected local account factors recovered; account sessions and personal tokens revoked", MetaJSON: meta}); err != nil {
		return LocalRecoveryResult{}, ErrRecoveryFailed
	}
	if err = tx.Commit(); err != nil {
		return LocalRecoveryResult{}, ErrRecoveryCommit
	}
	return result, nil
}
