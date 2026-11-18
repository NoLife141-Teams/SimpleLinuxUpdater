package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	auditpkg "debian-updater/internal/audit"
	authpkg "debian-updater/internal/auth"
	maintenancepkg "debian-updater/internal/maintenance"

	"github.com/gin-gonic/gin"
)

const statusBatchWorkers = 4
const statusBatchLimit = 100

type statusBatchService struct {
	deps     AppDeps
	slots    chan struct{}
	activeMu sync.RWMutex
	active   map[string]struct{}
}

func registerStatusBatchRoutes(r *gin.Engine, deps AppDeps) {
	service := &statusBatchService{
		deps: deps, slots: make(chan struct{}, statusBatchWorkers), active: make(map[string]struct{}),
	}
	r.POST("/api/status/batches", service.submit)
	r.GET("/api/status/batches/latest", service.get)
	r.GET("/api/status/batches/:id", service.get)
}

func batchActionScope(action string) string {
	switch action {
	case "update", "autoremove", "refresh_facts":
		return "maintenance:write"
	case "approve_all", "approve_security", "approve_security_kept_back", "cancel":
		return "approvals:write"
	}
	return ""
}

func validBatchID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && strings.ToLower(id) == id
}

func (request *statusBatchRequest) valid() bool {
	if !validBatchID(request.ID) || batchActionScope(request.Action) == "" ||
		request.SkippedCount < 0 || len(request.Targets) == 0 || len(request.Targets) > statusBatchLimit ||
		(request.RetryOf != "" && (!validBatchID(request.RetryOf) || request.RetryOf == request.ID)) {
		return false
	}
	seen := make(map[string]bool, len(request.Targets))
	for _, target := range request.Targets {
		if target.Name == "" || target.ServerIdentity == "" || seen[target.Name] {
			return false
		}
		seen[target.Name] = true
	}
	return true
}

func (s *statusBatchService) submit(c *gin.Context) {
	setNoStoreHeaders(c)
	var request statusBatchRequest
	if err := c.ShouldBindJSON(&request); err != nil || !request.valid() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid maintenance batch"})
		return
	}
	principal, ok := principalForContext(c)
	if !ok || !contextAllows(c, batchActionScope(request.Action)) {
		c.JSON(http.StatusForbidden, gin.H{"error": "This account cannot submit this maintenance action"})
		return
	}
	lease, decision := s.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkInteractive)
	if !decision.Allowed {
		writeMaintenanceBlockedSnapshotResponseWithTime(c, decision.State, s.deps.ApplicationTime)
		return
	}
	handedOff := false
	defer func() {
		if !handedOff {
			lease.Close()
		}
	}()

	db := s.deps.DB()
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Maintenance batch storage is unavailable"})
		return
	}
	ctx := c.Request.Context()
	serialized, err := json.Marshal(request)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid maintenance batch"})
		return
	}
	fingerprint := sha256.Sum256(serialized)
	actor, clientIP := actorFromContext(c), clientIPFromContext(c)
	s.activeMu.Lock()
	_, sourceActive := s.active[request.RetryOf]
	created, err := storeStatusBatch(ctx, db, request, hex.EncodeToString(fingerprint[:]), principal.Account.ID, actor,
		s.deps.Now().UTC().Format("2006-01-02T15:04:05.000000000Z"), sourceActive)
	if created && err == nil {
		s.active[request.ID] = struct{}{}
	}
	s.activeMu.Unlock()
	if err != nil {
		var retryConflict *statusBatchRetryConflict
		switch {
		case errors.Is(err, errBatchKeyConflict):
			c.JSON(http.StatusConflict, gin.H{"error": "This batch identifier already belongs to a different request"})
		case errors.Is(err, errBatchRetryPlanChanged):
			c.JSON(http.StatusConflict, gin.H{"error": staleApprovalMessage})
		case errors.As(err, &retryConflict):
			if retryConflict.batchID != "" {
				c.JSON(http.StatusConflict, gin.H{"error": "This host already has a retry attempt. Open it from the original batch.", "retry_batch_id": retryConflict.batchID})
			} else {
				c.JSON(http.StatusConflict, gin.H{"error": "Only hosts refused or interrupted before admission and clear for retry can be retried"})
			}
		default:
			log.Printf("persist maintenance batch %s: %v", request.ID, err)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Could not record the maintenance batch"})
		}
		return
	}
	if created {
		handedOff = true
		startTrackedActionRunner(func() {
			defer lease.Close()
			s.process(currentApplicationMaintenanceContext(), db, request, principal.Account.ID, actor, clientIP)
			s.activeMu.Lock()
			delete(s.active, request.ID)
			s.activeMu.Unlock()
		})
	}
	c.JSON(http.StatusAccepted, gin.H{"id": request.ID})
}

func (s *statusBatchService) process(ctx context.Context, db *sql.DB, request statusBatchRequest, account int64, actor, clientIP string) {
	targets := make(chan statusBatchTarget, len(request.Targets))
	for _, target := range request.Targets {
		targets <- target
	}
	close(targets)
	var workers sync.WaitGroup
	for range min(statusBatchWorkers, len(request.Targets)) {
		workers.Go(func() {
			for target := range targets {
				select {
				case s.slots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				s.processHost(ctx, db, request, target, account, actor, clientIP)
				<-s.slots
			}
		})
	}
	workers.Wait()
}

func (s *statusBatchService) processHost(ctx context.Context, db *sql.DB, request statusBatchRequest, target statusBatchTarget, account int64, actor, clientIP string) {
	if ctx.Err() != nil {
		return
	}
	if err := setStatusBatchHost(db, request.ID, target.Name, "queued", "admitting", "", ""); err != nil {
		log.Printf("claim maintenance batch %s host: %v", request.ID, err)
		return
	}
	recordAudit := func(action, targetType, targetName, status, message string, meta map[string]any) {
		meta = auditpkg.CloneMeta(meta)
		meta["account_id"] = account
		meta["maintenance_batch_id"] = request.ID
		if request.RetryOf != "" {
			meta["maintenance_batch_retry_of"] = request.RetryOf
		}
		if err := s.deps.AuditService.Record(actor, clientIP, action, targetType, targetName, status, message, meta); err != nil {
			log.Printf("record maintenance batch %s audit: %v", request.ID, err)
		}
	}
	result := jsonResult(http.StatusForbidden, "The account no longer has permission for this action")
	current, err := s.deps.AuthService.Account(account)
	if err == nil && !current.Disabled && authpkg.RoleAllows(current.Role, batchActionScope(request.Action)) {
		result = s.apply(ctx, request.Action, target, actor, clientIP, recordAudit)
	} else {
		recordAudit("maintenance_batch.admission", "server", target.Name, "ignored",
			"The account no longer has permission for this action", map[string]any{"batch_action": request.Action})
	}
	if result.statusCode != http.StatusOK {
		state := "refused"
		message, _ := result.body["error"].(string)
		if request.Action == "refresh_facts" && result.statusCode == http.StatusBadGateway {
			state = "failed"
		} else if result.statusCode >= http.StatusInternalServerError {
			state, message = "uncertain", "Check the job and audit history before attempting this command again."
		}
		if err := setStatusBatchHost(db, request.ID, target.Name, "admitting", state, message, ""); err != nil {
			log.Printf("record maintenance batch %s host %s: %v", request.ID, state, err)
		}
		return
	}
	jobID, _ := result.body["job_id"].(string)
	if request.Action == "cancel" || batchActionScope(request.Action) == "approvals:write" {
		jobID = target.Approval.JobID
	}
	state := "accepted"
	if jobID == "" {
		state = "completed"
	}
	if err := setStatusBatchHost(db, request.ID, target.Name, "admitting", state, "", jobID); err != nil {
		log.Printf("record accepted maintenance batch %s host: %v", request.ID, err)
	}
	waitForBatchHostJob(ctx, db, jobID)
}

func (s *statusBatchService) apply(ctx context.Context, action string, target statusBatchTarget, actor, clientIP string, recordAudit func(string, string, string, string, string, map[string]any)) serverActionLifecycleResult {
	lifecycle := newServerActionLifecycle(s.deps, recordAudit)
	lifecycle.expectedServerIdentity = target.ServerIdentity
	identity := target.Approval
	switch action {
	case "update":
		return lifecycle.StartUpdate(target.Name, actor, clientIP, target.ServerIdentity)
	case "autoremove":
		return lifecycle.startAutoremove(target.Name, actor, clientIP, target.ServerIdentity)
	case "refresh_facts":
		_, result := runServerFactsRefresh(ctx, s.deps, s.deps.ServerState, target.Name, target.ServerIdentity, func(status, message string, meta map[string]any) {
			recordAudit(serverFactsRefreshAction, "server", target.Name, status, message, meta)
		})
		return result
	case "approve_all":
		return lifecycle.ApproveAll(target.Name, identity)
	case "approve_security":
		return lifecycle.ApproveSecurity(target.Name, identity)
	case "approve_security_kept_back":
		return lifecycle.ApproveKeptBackSecurity(target.Name, false, identity)
	case "cancel":
		return lifecycle.Cancel(target.Name, identity)
	default:
		return jsonResult(http.StatusBadRequest, "Unknown maintenance action")
	}
}

func waitForBatchHostJob(ctx context.Context, db *sql.DB, jobID string) {
	if jobID == "" {
		return
	}
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		var status string
		err := db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && status != jobStatusQueued && status != jobStatusRunning) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *statusBatchService) get(c *gin.Context) {
	setNoStoreHeaders(c)
	id := c.Param("id")
	if id != "" && !validBatchID(id) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Maintenance batch not found"})
		return
	}
	principal, ok := principalForContext(c)
	if !ok {
		c.Status(http.StatusForbidden)
		return
	}
	db := s.deps.DB()
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Maintenance batch storage is unavailable"})
		return
	}
	batch, err := s.readBatch(c.Request.Context(), db, id, principal.Account.ID)
	if errors.Is(err, sql.ErrNoRows) && id == "" {
		c.JSON(http.StatusOK, gin.H{"batch": nil})
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Maintenance batch not found"})
		return
	}
	if err != nil {
		log.Printf("read maintenance batch %s: %v", id, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Could not read maintenance batch"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"batch": batch})
}

func (s *statusBatchService) readBatch(ctx context.Context, db *sql.DB, id string, account int64) (statusBatch, error) {
	// Take the durable snapshot and the active marker together. A worker may
	// finish writing during the read, but it cannot remove the marker until
	// after it is safe for a reader to infer that no further admission can occur.
	s.activeMu.RLock()
	batch, err := readStatusBatch(ctx, db, id, account)
	_, active := s.active[batch.ID]
	s.activeMu.RUnlock()
	if err == nil {
		s.projectBatch(db, &batch, active)
	}
	return batch, err
}

func (s *statusBatchService) projectBatch(db *sql.DB, batch *statusBatch, active bool) {
	for i := range batch.Hosts {
		host := &batch.Hosts[i]
		host.Retryable = host.canRetry(active, batch.Action)
		switch host.State {
		case "queued":
			if !active {
				host.State, host.Message = "interrupted", "The server stopped before admitting this command."
			}
		case "admitting":
			if !active {
				host.State, host.Message = "uncertain", "Check the job and audit history before attempting this command again."
				s.reconcileHostFromAudit(db, batch, host)
			}
		case "uncertain":
			s.reconcileHostFromAudit(db, batch, host)
		}
		if host.JobID == "" || host.State != "accepted" {
			continue
		}
		switch host.JobStatus {
		case "":
			host.State, host.Message = "uncertain", "The associated job is unavailable. Check the audit history."
		case jobStatusRunning:
			host.State = "running"
		case jobStatusWaitingApproval:
			host.State = "waiting_approval"
		case jobStatusSucceeded, jobStatusFailed, jobStatusCancelled, jobStatusInterrupted:
			host.State = "completed"
		}
	}
}

func (s *statusBatchService) reconcileHostFromAudit(db *sql.DB, batch *statusBatch, host *statusBatchHost) {
	var status, jobID, jobStatus, message string
	err := db.QueryRow(`SELECT a.status, COALESCE(json_extract(a.meta_json, '$.job_id'), ''), COALESCE(j.status, ''), a.message
		FROM audit_events a LEFT JOIN jobs j ON j.id = json_extract(a.meta_json, '$.job_id')
		WHERE a.actor = ? AND a.target_type = 'server' AND a.target_name = ? AND json_valid(a.meta_json)
		  AND json_extract(a.meta_json, '$.maintenance_batch_id') = ?
		  AND (a.status IN ('started', 'success') OR (a.action = ? AND a.status = 'failure' AND json_extract(a.meta_json, '$.collection_attempted') = 1))
		ORDER BY a.id DESC LIMIT 1`, batch.Actor, host.Name, batch.ID, serverFactsRefreshAction).Scan(&status, &jobID, &jobStatus, &message)
	if err == nil {
		if status == "failure" {
			host.State, host.Message = "failed", message
			return
		}
		host.Message = ""
		host.JobID = jobID
		host.JobStatus = jobStatus
		host.State = "accepted"
		if jobID == "" && status == "success" {
			host.State = "completed"
		}
	}
}
