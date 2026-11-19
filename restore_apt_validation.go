package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync"
	"time"

	internalbackup "debian-updater/internal/backup"
	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh/knownhosts"
)

const restoreAPTValidationRoute = "/api/backup/restore-isolation/apt-check"
const restoreAPTProofLifetime = 15 * time.Minute

var errRestoredAPTKeyChanged = errors.New("SSH host key changed after restore validation. Recheck the server in Installation Health")

type restoreAPTValidation struct {
	db        func() *sql.DB
	now       func() time.Time
	isolation *internalbackup.RestoreIsolation
	sessions  HostMaintenanceSessionFactory
	checkMu   sync.Mutex
}

type restoreAPTObservation struct {
	target, key, result string
	checkedAt           int64
	approved            bool
}

type restoreAPTHostFacts struct {
	Name      string     `json:"name"`
	Identity  string     `json:"identity"`
	Disabled  bool       `json:"disabled"`
	State     string     `json:"state"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
}

func ensureRestoreAPTValidationSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS restore_apt_validation (
		identity TEXT PRIMARY KEY CHECK(identity <> ''),
		target TEXT NOT NULL DEFAULT '',
		host_key TEXT NOT NULL DEFAULT '',
		result TEXT NOT NULL DEFAULT 'pending',
		checked_at INTEGER NOT NULL DEFAULT 0,
		approved INTEGER NOT NULL DEFAULT 0
	)`)
	return err
}

// The required identities are installed in the staged database. Replacement
// and rollback therefore move the gate together with its inventory.
func (v *restoreAPTValidation) prepare(ctx context.Context, staged *sql.DB, isolate bool) (bool, error) {
	if !isolate {
		err := v.db().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM restore_apt_validation)`).Scan(&isolate)
		if err != nil {
			return false, err
		}
	}
	tx, err := staged.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM restore_apt_validation`); err != nil {
		return false, err
	}
	if isolate {
		if _, err := tx.ExecContext(ctx, `INSERT INTO restore_apt_validation (identity)
			SELECT identity FROM servers WHERE identity <> ''`); err != nil {
			return false, err
		}
	}
	return isolate, tx.Commit()
}

func (v *restoreAPTValidation) observe(ctx context.Context, server Server) (restoreAPTObservation, bool, error) {
	var observation restoreAPTObservation
	err := v.db().QueryRowContext(ctx, `SELECT target,host_key,result,checked_at,approved
		FROM restore_apt_validation WHERE identity=?`, server.Identity).
		Scan(&observation.target, &observation.key, &observation.result, &observation.checkedAt, &observation.approved)
	if errors.Is(err, sql.ErrNoRows) {
		return observation, false, nil
	}
	return observation, true, err
}

func (observation restoreAPTObservation) facts(server Server, now time.Time, isolated bool) restoreAPTHostFacts {
	facts := restoreAPTHostFacts{Name: server.Name, Identity: server.Identity, Disabled: server.Disabled, State: observation.result}
	if observation.target != "" && observation.target != server.APTTarget() {
		facts.State = "target_changed"
		return facts
	}
	if observation.checkedAt != 0 {
		at := time.Unix(observation.checkedAt, 0).UTC()
		facts.CheckedAt = &at
	}
	if observation.result == "healthy" {
		switch {
		case observation.approved && !isolated:
			facts.State = "approved"
		case observation.checkedAt < now.Add(-restoreAPTProofLifetime).Unix() || observation.checkedAt > now.Unix():
			facts.State = "expired"
		}
	}
	return facts
}

func (v *restoreAPTValidation) authorize(ctx context.Context, server Server) (string, error) {
	if v == nil || v.db() == nil {
		return "", nil
	}
	observation, required, err := v.observe(ctx, server)
	if err != nil {
		return "", serverpkg.ErrRestoreAPTValidation
	}
	if !required {
		return "", nil
	}
	if !observation.authorizes(server) {
		return "", serverpkg.ErrRestoreAPTValidation
	}
	return observation.key, nil
}

func (o restoreAPTObservation) authorizes(server Server) bool {
	return o.approved && o.result == "healthy" && o.key != "" && o.target != "" && o.target == server.APTTarget()
}

func (v *restoreAPTValidation) all(ctx context.Context) (map[string]restoreAPTObservation, error) {
	db := v.db()
	if db == nil {
		return nil, errors.New("restore APT persistence is unavailable")
	}
	rows, err := db.QueryContext(ctx, `SELECT identity,target,host_key,result,checked_at,approved FROM restore_apt_validation`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]restoreAPTObservation)
	for rows.Next() {
		var identity string
		var observation restoreAPTObservation
		if err := rows.Scan(&identity, &observation.target, &observation.key, &observation.result,
			&observation.checkedAt, &observation.approved); err != nil {
			return nil, err
		}
		result[identity] = observation
	}
	return result, rows.Err()
}

func (v *restoreAPTValidation) blocked(ctx context.Context, servers []Server) map[string]bool {
	observations, err := v.all(ctx)
	blocked := make(map[string]bool)
	isolated := v.isolation.BlocksExternalWork()
	for _, server := range servers {
		observation, required := observations[server.Identity]
		if err != nil || (required && (isolated || !observation.authorizes(server))) {
			blocked[server.Name] = true
		}
	}
	return blocked
}

func (v *restoreAPTValidation) admit(server Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := v.authorize(ctx, server)
	return err
}

func (v *restoreAPTValidation) approve(ctx context.Context, now time.Time) error {
	_, err := v.db().ExecContext(ctx, `UPDATE restore_apt_validation SET approved=(
		result='healthy' AND host_key<>'' AND target<>'' AND checked_at BETWEEN ? AND ?)`,
		now.Add(-restoreAPTProofLifetime).Unix(), now.Unix())
	return err
}

func (v *restoreAPTValidation) invalidateHostKey(ctx context.Context, server Server, key string) {
	// A newer explicit check may have accepted a different key while the
	// failed connection was being reported. Preserve that newer evidence.
	if db := v.db(); db != nil {
		_, _ = db.ExecContext(ctx, `UPDATE restore_apt_validation
			SET approved=0,result='host_key_unverified',checked_at=?
			WHERE identity=? AND target=? AND host_key=?`, v.now().Unix(),
			server.Identity, server.APTTarget(), key)
	}
}

func restoreAPTHostKeyError(err error) bool {
	var keyErr *knownhosts.KeyError
	var revokedErr *knownhosts.RevokedError
	return errors.Is(err, errRestoredAPTKeyChanged) || errors.As(err, &keyErr) || errors.As(err, &revokedErr) ||
		updatespkg.HostMaintenanceErrorStageOf(err) == updatespkg.HostMaintenanceStageHostKey
}

func (v *restoreAPTValidation) probe(ctx context.Context, server Server) restoreAPTObservation {
	observation := restoreAPTObservation{target: server.APTTarget(), result: "unreachable"}
	if observation.target == "" || v.sessions == nil {
		return observation
	}
	session, err := v.sessions.Open(ctx, HostMaintenanceSessionRequest{
		Server: server, DialOperation: "restore_apt_check.ssh", CommandTimeout: 5 * time.Second,
		RetryPolicy: RetryPolicy{MaxAttempts: 1},
		VerifyHostKey: func(fingerprint string) error {
			if fingerprint == "" {
				return errors.New("SSH host key could not be verified")
			}
			if observation.key != "" && observation.key != fingerprint {
				return errors.New("SSH host key changed during the APT check")
			}
			return nil
		},
		OnConnectedHostKey: func(fingerprint string) { observation.key = fingerprint },
	})
	if err != nil {
		if restoreAPTHostKeyError(err) {
			observation.result = "host_key_unverified"
		}
		return observation
	}
	defer session.Close()
	if observation.key == "" {
		observation.result = "host_key_unverified"
		return observation
	}
	observation.result = session.CheckRestoredAPT(ctx)
	return observation
}

func handleRestoreAPTValidation(c *gin.Context, deps AppDeps) {
	setNoStoreHeaders(c)
	v := deps.RestoreAPTValidation
	if v == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Restore APT checks are unavailable."})
		return
	}
	var selection struct {
		Name     string `json:"name"`
		Identity string `json:"identity"`
	}
	if err := c.ShouldBindJSON(&selection); err != nil || selection.Name == "" || selection.Identity == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Select a restored server to check."})
		return
	}
	if !v.checkMu.TryLock() {
		c.JSON(http.StatusConflict, gin.H{"error": "Another restored server is being checked. Try again in a moment."})
		return
	}
	defer v.checkMu.Unlock()
	server, found := deps.ServerState.FindByName(selection.Name)
	if !found || server.Identity != selection.Identity || server.Disabled {
		c.JSON(http.StatusConflict, gin.H{"error": "Select the current enabled restored server."})
		return
	}
	if active, _ := deps.ServerState.ActionStatusInProgress(selection.Name); active {
		c.JSON(http.StatusConflict, gin.H{"error": "Wait for current server work to finish before checking APT."})
		return
	}
	ctx := c.Request.Context()
	if _, required, err := v.observe(ctx, server); err != nil || !required {
		c.JSON(http.StatusConflict, gin.H{"error": "This server has no pending restore validation."})
		return
	}
	isolated, err := v.isolation.State()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Restore isolation state is unavailable."})
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	observation := v.probe(probeCtx, server)
	probeErr := probeCtx.Err()
	cancel()
	if ctx.Err() != nil {
		return
	}
	if probeErr != nil {
		observation.result = "unreachable"
	}
	current, found := deps.ServerState.FindByName(selection.Name)
	if !found || current.Disabled || current.APTTarget() != observation.target {
		c.JSON(http.StatusConflict, gin.H{"error": "The server changed during the APT check."})
		return
	}
	now := v.now()
	observation.checkedAt = now.Unix()
	observation.approved = !isolated && observation.result == "healthy"
	if err := deps.AuditService.Record(actorFromContext(c), clientIPFromContext(c), "backup.restore_apt.check",
		"server", server.Name, observation.result, "Remote APT state checked after restore", nil); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "The APT check could not be audited."})
		return
	}
	if _, err := v.db().ExecContext(ctx, `UPDATE restore_apt_validation
		SET target=?,host_key=?,result=?,checked_at=?,approved=? WHERE identity=?`, observation.target,
		observation.key, observation.result, observation.checkedAt, observation.approved, server.Identity); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "The APT check could not be saved."})
		return
	}
	deps.NotifyDashboardEvent("restore-apt-checked")
	c.JSON(http.StatusOK, observation.facts(server, now, isolated))
}
