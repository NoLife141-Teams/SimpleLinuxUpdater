package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

var ErrFactorInvalid = errors.New("invalid or replayed authentication factor")
var ErrFactorRateLimited = errors.New("too many authentication factor attempts")

// HOTP implements RFC 4226 section 5; TOTP uses RFC 6238's 30-second
// counter with SHA-1, six digits and a bounded adjacent-step clock tolerance.
func HOTP(secret []byte, counter uint64, digits int) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], counter)
	mac := hmac.New(sha1.New, secret)
	_, _ = mac.Write(b[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 15
	n := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	modulus := uint32(1000000)
	if digits == 8 {
		modulus = 100000000
	}
	return fmt.Sprintf("%0*d", digits, n%modulus)
}
func totpStep(secret, code string, at time.Time, last int64) int64 {
	if len(code) != 6 {
		return -1
	}
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		return -1
	}
	for delta := int64(-1); delta <= 1; delta++ {
		step := at.Unix()/30 + delta
		if step > last && step >= 0 && subtle.ConstantTimeCompare([]byte(HOTP(raw, uint64(step), 6)), []byte(code)) == 1 {
			return step
		}
	}
	return -1
}

type Enrollment struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`
}

func (s *Service) BeginMFA(id int64) (Enrollment, error) {
	var out Enrollment
	a, err := s.Account(id)
	if err != nil {
		return out, err
	}
	if a.ID == 0 || a.Disabled || a.MFAEnabled {
		return out, NewValidationError("MFA already enabled or account unavailable")
	}
	raw := make([]byte, 20)
	if _, err = rand.Read(raw); err != nil {
		return out, err
	}
	out.Secret = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	if s.encrypt == nil {
		return Enrollment{}, errors.New("factor encryption unavailable")
	}
	encrypted, err := s.encrypt(out.Secret)
	if err != nil {
		return Enrollment{}, err
	}
	_, err = s.db().Exec("UPDATE auth_users SET totp_pending_enc=?,totp_pending_until=? WHERE id=? AND totp_secret_enc='' AND disabled=0", encrypted, s.now().Add(10*time.Minute).Unix(), id)
	out.URI = "otpauth://totp/" + url.PathEscape("SimpleLinuxUpdater:"+a.Username) + "?secret=" + out.Secret + "&issuer=SimpleLinuxUpdater&algorithm=SHA1&digits=6&period=30"
	return out, err
}
func (s *Service) ConfirmMFA(id int64, code string) ([]string, error) {
	tx, err := s.db().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var encrypted string
	var until int64
	if err = tx.QueryRow("SELECT totp_pending_enc,totp_pending_until FROM auth_users WHERE id=? AND disabled=0 AND totp_secret_enc=''", id).Scan(&encrypted, &until); err != nil {
		return nil, ErrFactorInvalid
	}
	if encrypted == "" || until < s.now().Unix() || s.decrypt == nil {
		return nil, ErrFactorInvalid
	}
	secret, err := s.decrypt(encrypted)
	if err != nil {
		return nil, err
	}
	step := totpStep(secret, code, s.now(), -1)
	if step < 0 {
		return nil, ErrFactorInvalid
	}
	result, err := tx.Exec("UPDATE auth_users SET totp_secret_enc=totp_pending_enc,totp_pending_enc='',totp_pending_until=0,totp_last_step=?,generation=generation+1 WHERE id=? AND totp_pending_enc=?", step, id, encrypted)
	if err != nil {
		return nil, err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return nil, ErrFactorInvalid
	}
	codes, err := newRecoveryCodes(tx, id)
	if err != nil {
		return nil, err
	}
	if err = invalidateIdentityTx(tx, id); err != nil {
		return nil, err
	}
	return codes, tx.Commit()
}
func newRecoveryCodes(tx *sql.Tx, id int64) ([]string, error) {
	if _, err := tx.Exec("DELETE FROM auth_recovery_codes WHERE account_id=?", id); err != nil {
		return nil, err
	}
	codes := make([]string, 10)
	for i := range codes {
		code, err := randomSecret(16)
		if err != nil {
			return nil, err
		}
		codes[i] = code
		if _, err = tx.Exec("INSERT INTO auth_recovery_codes VALUES(?,?)", id, secretHash(code)); err != nil {
			return nil, err
		}
	}
	return codes, nil
}
func invalidateIdentityTx(tx *sql.Tx, id int64) error {
	for _, query := range []string{"DELETE FROM sessions WHERE account_id=?", "DELETE FROM auth_challenges WHERE account_id=?", "UPDATE auth_personal_tokens SET revoked=1 WHERE account_id=?"} {
		if _, err := tx.Exec(query, id); err != nil {
			return err
		}
	}
	_, err := tx.Exec("UPDATE auth_login_generation SET generation=generation+1 WHERE id=1")
	return err
}
func (s *Service) DisableMFA(id int64) error {
	tx, err := s.db().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE auth_users SET totp_secret_enc='',totp_pending_enc='',totp_pending_until=0,totp_last_step=-1,generation=generation+1 WHERE id=?", id); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM auth_recovery_codes WHERE account_id=?", id); err != nil {
		return err
	}
	if err = invalidateIdentityTx(tx, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Service) ConsumeFactor(id int64, code string) error {
	tx, err := s.db().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	factorErr := s.consumeFactorTx(tx, id, code)
	if err = tx.Commit(); err != nil {
		return err
	}
	return factorErr
}
func (s *Service) consumeFactorTx(tx *sql.Tx, id int64, code string) error {
	// A durable account-wide limit supplements the transport's IP limit and
	// survives restarts and distributed attempts with a known first factor.
	if _, err := tx.Exec(`INSERT INTO auth_factor_attempts(account_id,attempts,window_until) VALUES(?,1,?)
	 ON CONFLICT(account_id) DO UPDATE SET attempts=CASE WHEN window_until<=? THEN 1 ELSE attempts+1 END,
	 window_until=CASE WHEN window_until<=? THEN excluded.window_until ELSE window_until END`, id, s.now().Add(10*time.Minute).Unix(), s.now().Unix(), s.now().Unix()); err != nil {
		return err
	}
	var attempts int
	if err := tx.QueryRow("SELECT attempts FROM auth_factor_attempts WHERE account_id=?", id).Scan(&attempts); err != nil {
		return err
	}
	if attempts > 10 {
		return ErrFactorRateLimited
	}
	var encrypted string
	var last int64
	if err := tx.QueryRow("SELECT totp_secret_enc,totp_last_step FROM auth_users WHERE id=? AND disabled=0", id).Scan(&encrypted, &last); err != nil {
		return ErrFactorInvalid
	}
	if encrypted == "" {
		return nil
	}
	if s.decrypt == nil {
		return ErrFactorInvalid
	}
	secret, err := s.decrypt(encrypted)
	if err != nil {
		return err
	}
	step := totpStep(secret, strings.TrimSpace(code), s.now(), last)
	if step >= 0 {
		result, err := tx.Exec("UPDATE auth_users SET totp_last_step=? WHERE id=? AND totp_last_step<?", step, id, step)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n == 1 {
			return nil
		}
		return ErrFactorInvalid
	}
	result, err := tx.Exec("DELETE FROM auth_recovery_codes WHERE account_id=? AND code_hash=?", id, secretHash(strings.TrimSpace(code)))
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrFactorInvalid
	}
	return nil
}
func (s *Service) NewLoginChallenge(ctx context.Context, username string) (string, error) {
	a, err := s.AccountByName(username)
	if err != nil {
		return "", err
	}
	if a.ID == 0 || a.Disabled || !a.MFAEnabled {
		return "", ErrFactorInvalid
	}
	secret, err := randomSecret(32)
	if err != nil {
		return "", err
	}

	prepared, ok := ctx.Value(preparedAccountKey{}).(Account)
	if !ok {
		prepared = a
	}
	epoch, _ := ctx.Value(installationEpochContextKey{}).(string)
	if epoch == "" {
		if err := s.db().QueryRow("SELECT epoch FROM auth_installation_epoch WHERE id=1").Scan(&epoch); err != nil {
			return "", err
		}
	}
	result, err := s.db().Exec(`INSERT INTO auth_challenges(hash,account_id,generation,login_generation,expires)
 SELECT ?,id,generation,login_generation,? FROM auth_users WHERE id=? AND generation=? AND login_generation=? AND disabled=0
 AND EXISTS(SELECT 1 FROM auth_installation_epoch WHERE epoch=?)`, secretHash(secret), s.now().Add(5*time.Minute).Unix(), prepared.ID, prepared.Generation, prepared.LoginGeneration, epoch)
	if err == nil {
		n, _ := result.RowsAffected()
		if n != 1 {
			return "", ErrAuthenticationChanged
		}
	}

	// Bound abandoned challenges without retaining plaintext challenge credentials.
	if err == nil {
		_, err = s.db().Exec("DELETE FROM auth_challenges WHERE expires<?", s.now().Unix())
	}
	return secret, err
}
func (s *Service) CompleteLoginChallenge(challenge, code string) (Account, error) {
	tx, err := s.db().Begin()
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback()
	a, err := scanAccount(tx.QueryRow("SELECT "+"u.id,u.username,u.role,u.disabled,u.generation,u.password_hash,(u.totp_secret_enc!=''),u.login_generation"+" FROM auth_users u JOIN auth_challenges c ON c.account_id=u.id WHERE c.hash=? AND c.expires>? AND c.generation=u.generation AND c.login_generation=u.login_generation AND u.disabled=0 AND u.totp_secret_enc!=''", secretHash(challenge), s.now().Unix()))
	if err != nil || a.ID == 0 {
		return Account{}, ErrFactorInvalid
	}
	// Consume the challenge even on a rejected code: the caller must start again.
	if _, err = tx.Exec("DELETE FROM auth_challenges WHERE hash=?", secretHash(challenge)); err != nil {
		return Account{}, err
	}
	if err = s.consumeFactorTx(tx, a.ID, code); err != nil {
		if commitErr := tx.Commit(); commitErr != nil {
			return Account{}, commitErr
		}
		return Account{}, err
	}
	return a, tx.Commit()
}
