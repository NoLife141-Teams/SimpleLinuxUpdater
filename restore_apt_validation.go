package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	internalbackup "debian-updater/internal/backup"
	runtimepkg "debian-updater/internal/runtime"
	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh/knownhosts"
)

const restoreAPTValidationRoute = "/api/backup/restore-isolation/apt-check"
const (
	restoreAPTProofLifetime    = 15 * time.Minute
	restoreAPTCheckConcurrency = 4
	restoreAPTCheckBatchLimit  = 12
	// Keep time for the response within the HTTP server's write deadline.
	restoreAPTCheckBatchTimeout = 50 * time.Second
)

var errRestoredAPTKeyChanged = errors.New("SSH host key changed after restore validation. Recheck the server in Installation Health")

type restoreAPTValidation struct {
	db             func() *sql.DB
	now            func() time.Time
	isolation      *internalbackup.RestoreIsolation
	sessions       HostMaintenanceSessionFactory
	checkSlotsOnce sync.Once
	checkSlots     chan struct{}
}

type restoreAPTObservation struct {
	target, key, result string
	checkedAt           int64
	approved            bool
}

type restoreAPTHostFacts struct {
	Name           string     `json:"name"`
	Identity       string     `json:"identity"`
	TargetRevision string     `json:"target_revision,omitempty"`
	Disabled       bool       `json:"disabled"`
	Checking       bool       `json:"checking,omitempty"`
	State          string     `json:"state"`
	CheckedAt      *time.Time `json:"checked_at,omitempty"`
}

func restoreAPTTargetRevision(target string) string {
	if target == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(target))
	return hex.EncodeToString(digest[:])
}

func ensureRestoreAPTValidationSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS restore_apt_validation_targets (
		endpoint TEXT PRIMARY KEY CHECK(endpoint <> '')
	);
	CREATE TABLE IF NOT EXISTS restore_apt_validation (
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
	protected, err := loadRestoreAPTTargets(ctx, v.db())
	if err != nil {
		return false, err
	}
	isolate = isolate || len(protected) != 0
	if !isolate {
		var hasInventory bool
		err := v.db().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM servers)`).Scan(&hasInventory)
		if err != nil {
			return false, err
		}
		if !hasInventory {
			// The encryption key can be retained while SQLite is rebuilt.
			// There is no live inventory to reconcile in that case.
			err = staged.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM servers)`).Scan(&isolate)
			if err != nil {
				return false, err
			}
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM restore_apt_validation_targets`); err != nil {
		return false, err
	}
	if isolate {
		// Keep the destination's prior obligations even when an older archive
		// omits those endpoints. The copied archive cannot authorize them.
		for _, endpoint := range protected {
			if _, err := tx.ExecContext(ctx, `INSERT INTO restore_apt_validation_targets(endpoint) VALUES (?)`, endpoint); err != nil {
				return false, err
			}
		}
		if err := reconcileRestoredAPTInventory(ctx, tx, true); err != nil {
			return false, err
		}
	}
	return isolate, tx.Commit()
}

func loadRestoreAPTTargets(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT endpoint FROM restore_apt_validation_targets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var endpoints []string
	for rows.Next() {
		var endpoint string
		if err := rows.Scan(&endpoint); err != nil {
			return nil, err
		}
		endpoints = append(endpoints, endpoint)
	}
	return endpoints, rows.Err()
}

// Retain the SSH endpoints even if their inventory records are deleted. A
// replacement identity at any of those endpoints must establish its own proof.
func reconcileRestoredAPTInventory(ctx context.Context, tx *sql.Tx, restored bool) error {
	if !restored {
		var guarded bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM restore_apt_validation_targets)`).Scan(&guarded); err != nil || !guarded {
			return err
		}
	}
	inventory, err := aptRestoreInventory(ctx, tx)
	if err != nil {
		return err
	}
	for target, server := range inventory.byTarget {
		endpoint := serverpkg.NormalizeServerEndpoint(server.Host, server.Port).String()
		if _, err := tx.ExecContext(ctx, `INSERT INTO restore_apt_validation_targets (endpoint)
			SELECT ? WHERE ? OR EXISTS(SELECT 1 FROM restore_apt_validation WHERE identity=?)
			ON CONFLICT(endpoint) DO NOTHING`, endpoint, restored, server.Identity); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO restore_apt_validation (identity,target)
			SELECT ?,? WHERE EXISTS(SELECT 1 FROM restore_apt_validation_targets WHERE endpoint=?)
			ON CONFLICT(identity) DO UPDATE SET
				target=excluded.target, host_key='', result='target_changed', checked_at=0, approved=0
			WHERE restore_apt_validation.target<>excluded.target`, server.Identity, target, endpoint); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM restore_apt_validation WHERE
		NOT EXISTS(SELECT 1 FROM servers WHERE servers.identity=restore_apt_validation.identity)`)
	return err
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

func (observation restoreAPTObservation) facts(server Server, now time.Time, isolated, needsReconciliation bool) restoreAPTHostFacts {
	target := server.APTTarget()
	facts := restoreAPTHostFacts{
		Name: server.Name, Identity: server.Identity, Disabled: server.Disabled,
		TargetRevision: restoreAPTTargetRevision(target), State: observation.result,
	}
	if needsReconciliation {
		facts.State = "reconciliation_required"
	}
	if observation.target != "" && observation.target != target {
		if !needsReconciliation {
			facts.State = "target_changed"
		}
		return facts
	}
	if observation.checkedAt != 0 {
		at := time.Unix(observation.checkedAt, 0).UTC()
		facts.CheckedAt = &at
	}
	if !needsReconciliation && observation.result == "healthy" {
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

func (v *restoreAPTValidation) hostFacts(ctx context.Context, state *serverpkg.State, now time.Time, isolated bool) []restoreAPTHostFacts {
	observations, err := v.all(ctx)
	if err != nil || len(observations) == 0 {
		return nil
	}
	state.Lock()
	defer state.Unlock()
	var facts []restoreAPTHostFacts
	for _, server := range state.Servers() {
		if observation, required := observations[server.Identity]; required {
			status := state.StatusMap()[server.Name]
			host := observation.facts(server, now, isolated, status.RequiresAPTReconciliation())
			host.Checking = status != nil && status.Status == runtimepkg.StatusRestoreAPTCheck
			facts = append(facts, host)
		}
	}
	return facts
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
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
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

type restoreAPTCheckTarget struct {
	Name           string `json:"name"`
	Identity       string `json:"identity"`
	TargetRevision string `json:"target_revision,omitempty"`
	target         string
}

type restoreAPTCheckResult struct {
	restoreAPTHostFacts
	Error  string `json:"error,omitempty"`
	status int
}

func (target restoreAPTCheckTarget) failure(status int, message string) restoreAPTCheckResult {
	return restoreAPTCheckResult{
		restoreAPTHostFacts: restoreAPTHostFacts{Name: target.Name, Identity: target.Identity},
		Error:               message, status: status,
	}
}

func handleRestoreAPTValidation(c *gin.Context, deps AppDeps) {
	setNoStoreHeaders(c)
	v := deps.RestoreAPTValidation
	if v == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Restore APT checks are unavailable."})
		return
	}
	var selection struct {
		restoreAPTCheckTarget
		Targets *[]restoreAPTCheckTarget `json:"targets"`
	}
	if err := c.ShouldBindJSON(&selection); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Select one or more restored servers to check."})
		return
	}
	single := selection.Targets == nil
	targets := []restoreAPTCheckTarget{selection.restoreAPTCheckTarget}
	if !single {
		targets = *selection.Targets
		if selection.Name != "" || selection.Identity != "" || len(targets) == 0 || len(targets) > restoreAPTCheckBatchLimit {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Select between 1 and 12 restored servers to check."})
			return
		}
	}
	names, identities := make(map[string]bool), make(map[string]bool)
	for _, target := range targets {
		if target.Name == "" || target.Identity == "" || names[target.Name] || identities[target.Identity] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Select each restored server once with its current identity."})
			return
		}
		names[target.Name], identities[target.Identity] = true, true
	}

	bindRestoreAPTCheckTargets(deps.ServerState, targets)

	ctx, cancel := context.WithTimeout(c.Request.Context(), restoreAPTCheckBatchTimeout)
	defer cancel()
	actor, ip := actorFromContext(c), clientIPFromContext(c)
	if single {
		result := v.check(ctx, deps, targets[0], actor, ip)
		if c.Request.Context().Err() != nil {
			return
		}
		if result.Error != "" {
			c.JSON(result.status, gin.H{"error": result.Error})
			return
		}
		c.JSON(http.StatusOK, result.restoreAPTHostFacts)
		return
	}

	results := make([]restoreAPTCheckResult, len(targets))
	next := make(chan int, len(targets))
	for index := range targets {
		next <- index
	}
	close(next)
	var workers sync.WaitGroup
	for range min(restoreAPTCheckConcurrency, len(targets)) {
		workers.Go(func() {
			for index := range next {
				results[index] = v.check(ctx, deps, targets[index], actor, ip)
			}
		})
	}
	workers.Wait()
	if c.Request.Context().Err() == nil {
		c.JSON(http.StatusOK, gin.H{"results": results})
	}
}

// Bind the current SSH endpoint at admission so an inventory edit while a
// target waits for a slot cannot redirect the check to a different host.
func bindRestoreAPTCheckTargets(state *serverpkg.State, targets []restoreAPTCheckTarget) {
	state.Lock()
	defer state.Unlock()
	for i := range targets {
		selection := &targets[i]
		if server, ok := state.FindByNameLocked(selection.Name); ok && server.Identity == selection.Identity && !server.Disabled {
			selection.target = server.APTTarget()
		}
	}
}

func (v *restoreAPTValidation) check(ctx context.Context, deps AppDeps, target restoreAPTCheckTarget, actor, ip string) restoreAPTCheckResult {
	if target.target == "" {
		return target.failure(http.StatusConflict, "Select the current enabled restored server.")
	}
	targetChanged := func() restoreAPTCheckResult {
		return target.failure(http.StatusConflict, "The server's SSH target changed. Select the server again.")
	}
	if target.TargetRevision != "" && target.TargetRevision != restoreAPTTargetRevision(target.target) {
		return targetChanged()
	}
	cancelled := func() restoreAPTCheckResult {
		return target.failure(http.StatusRequestTimeout, "The APT check did not finish. Select the server to try again.")
	}
	v.checkSlotsOnce.Do(func() { v.checkSlots = make(chan struct{}, restoreAPTCheckConcurrency) })
	select {
	case v.checkSlots <- struct{}{}:
		defer func() { <-v.checkSlots }()
	case <-ctx.Done():
		return cancelled()
	}
	if ctx.Err() != nil {
		return cancelled()
	}

	server, previousStatus, err := deps.ServerState.BeginTransientActionForIdentity(target.Name, runtimepkg.StatusRestoreAPTCheck, target.Identity)
	if err != nil {
		message := "Select the current enabled restored server."
		if errors.Is(err, serverpkg.ErrActionInProgress) {
			message = "Wait for current server work to finish before checking APT."
		}
		return target.failure(http.StatusConflict, message)
	}
	checkStarted := false
	defer func() {
		deps.ServerState.FinishTransientAction(server, runtimepkg.StatusRestoreAPTCheck, previousStatus)
		if checkStarted {
			deps.NotifyDashboardEvent("restore-apt-checked")
		}
	}()
	if server.APTTarget() != target.target {
		return targetChanged()
	}
	previous, required, err := v.observe(ctx, server)
	if err != nil || !required {
		return target.failure(http.StatusConflict, "This server has no pending restore validation.")
	}
	isolated, err := v.isolation.State()
	if err != nil {
		return target.failure(http.StatusServiceUnavailable, "Restore isolation state is unavailable.")
	}
	// Once a new check starts, only its recorded result may authorize this
	// host. Cancellation or a failed audit leaves the gate closed.
	if _, err := v.db().ExecContext(ctx, `UPDATE restore_apt_validation
		SET result='pending',host_key='',checked_at=0,approved=0
		WHERE identity=? AND target=? AND result='healthy'`, server.Identity, previous.target); err != nil {
		return target.failure(http.StatusServiceUnavailable, "The previous APT check could not be cleared.")
	}
	checkStarted = true
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	observation := v.probe(probeCtx, server)
	probeErr := probeCtx.Err()
	cancel()
	if ctx.Err() != nil {
		return cancelled()
	}
	if probeErr != nil {
		observation.result = "unreachable"
	}
	current, found := deps.ServerState.FindByName(target.Name)
	if !found || current.Disabled || current.APTTarget() != observation.target {
		return target.failure(http.StatusConflict, "The server changed during the APT check.")
	}
	now := v.now()
	observation.checkedAt = now.Unix()
	observation.approved = !isolated && observation.result == "healthy"
	if err := deps.AuditService.Record(actor, ip, "backup.restore_apt.check",
		"server", server.Name, observation.result, "Remote APT state checked after restore", nil); err != nil {
		return target.failure(http.StatusServiceUnavailable, "The APT check could not be audited.")
	}
	saved, err := v.db().ExecContext(ctx, `UPDATE restore_apt_validation
		SET target=?,host_key=?,result=?,checked_at=?,approved=? WHERE identity=? AND target=?`, observation.target,
		observation.key, observation.result, observation.checkedAt, observation.approved, server.Identity, previous.target)
	if err != nil {
		return target.failure(http.StatusServiceUnavailable, "The APT check could not be saved.")
	}
	if affected, err := saved.RowsAffected(); err != nil || affected != 1 {
		return target.failure(http.StatusConflict, "The server changed during the APT check.")
	}
	status := deps.ServerState.CurrentStatusSnapshot(server.Name)
	return restoreAPTCheckResult{
		restoreAPTHostFacts: observation.facts(server, now, isolated, status.RequiresAPTReconciliation()),
	}
}
