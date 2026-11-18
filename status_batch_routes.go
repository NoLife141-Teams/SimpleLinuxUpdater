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
	deps   AppDeps
	slots  chan struct{}
	active sync.Map
	create sync.Mutex
}

func registerStatusBatchRoutes(r *gin.Engine, deps AppDeps) {
	service := &statusBatchService{deps: deps, slots: make(chan struct{}, statusBatchWorkers)}
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
		if batchActionScope(request.Action) == "approvals:write" && (target.Approval.JobID == "" || target.Approval.Generation == 0) {
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
	if request.RetryOf != "" && !s.canRetry(ctx, db, request, principal.Account.ID) {
		c.JSON(http.StatusConflict, gin.H{"error": "Only refused or interrupted hosts from the original batch can be retried"})
		return
	}
	serialized, err := json.Marshal(request)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid maintenance batch"})
		return
	}
	fingerprint := sha256.Sum256(serialized)
	actor, clientIP := actorFromContext(c), clientIPFromContext(c)
	s.create.Lock()
	created, err := storeStatusBatch(ctx, db, request, hex.EncodeToString(fingerprint[:]), principal.Account.ID, actor, s.deps.Now().UTC().Format("2006-01-02T15:04:05.000000000Z"))
	if created && err == nil {
		s.active.Store(request.ID, struct{}{})
	}
	s.create.Unlock()
	if err != nil {
		if errors.Is(err, errBatchKeyConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		} else {
			log.Printf("persist maintenance batch %s: %v", request.ID, err)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Could not record the maintenance batch"})
		}
		return
	}
	if created {
		handedOff = true
		startTrackedActionRunner(func() {
			defer lease.Close()
			defer s.active.Delete(request.ID)
			s.process(currentApplicationMaintenanceContext(), db, request, principal.Account.ID, actor, clientIP)
		})
	}
	c.JSON(http.StatusAccepted, gin.H{"id": request.ID})
}

func (s *statusBatchService) canRetry(ctx context.Context, db *sql.DB, request statusBatchRequest, account int64) bool {
	original, err := readStatusBatch(ctx, db, request.RetryOf, account)
	if err != nil || original.Action != request.Action {
		return false
	}
	s.projectBatch(db, &original)
	retryable := make(map[string]string, len(original.Hosts))
	for _, host := range original.Hosts {
		if host.Retryable {
			retryable[host.Name] = host.ServerIdentity
		}
	}
	for _, target := range request.Targets {
		if retryable[target.Name] != target.ServerIdentity {
			return false
		}
	}
	return true
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
		if result.statusCode >= http.StatusInternalServerError {
			state, message = "uncertain", "Check the job and audit history before attempting this command again."
		}
		if err := setStatusBatchHost(db, request.ID, target.Name, "admitting", state, message, ""); err != nil {
			log.Printf("record refused maintenance batch %s host: %v", request.ID, err)
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
	s.waitForHostJob(ctx, jobID)
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

func (s *statusBatchService) waitForHostJob(ctx context.Context, jobID string) {
	if jobID == "" {
		return
	}
	manager := s.deps.CurrentJobManager()
	if manager == nil {
		return
	}
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := manager.GetJob(jobID)
		if err != nil || (job.Status != jobStatusQueued && job.Status != jobStatusRunning) {
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
	batch, err := readStatusBatch(c.Request.Context(), db, id, principal.Account.ID)
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
	s.projectBatch(db, &batch)
	c.JSON(http.StatusOK, gin.H{"batch": batch})
}

func (s *statusBatchService) projectBatch(db *sql.DB, batch *statusBatch) {
	_, active := s.active.Load(batch.ID)
	for i := range batch.Hosts {
		host := &batch.Hosts[i]
		switch host.State {
		case "queued":
			if !active {
				host.State, host.Message = "interrupted", "The server stopped before admitting this command."
				host.Retryable = true
			}
		case "admitting":
			if !active {
				host.State, host.Message = "uncertain", "Check the job and audit history before attempting this command again."
				s.reconcileHostFromAudit(db, batch, host)
			}
		case "uncertain":
			s.reconcileHostFromAudit(db, batch, host)
		case "refused":
			host.Retryable = true
		}
		if host.JobID == "" || host.State != "accepted" {
			continue
		}
		manager := s.deps.CurrentJobManager()
		if manager == nil {
			continue
		}
		job, err := manager.GetJob(host.JobID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				host.State, host.Message = "uncertain", "The associated job is unavailable. Check the audit history."
			}
			continue
		}
		host.JobStatus = job.Status
		switch job.Status {
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
	var status, jobID string
	err := db.QueryRow(`SELECT status, COALESCE(json_extract(meta_json, '$.job_id'), '') FROM audit_events
		WHERE actor = ? AND target_type = 'server' AND target_name = ? AND json_valid(meta_json)
		  AND json_extract(meta_json, '$.maintenance_batch_id') = ? AND status IN ('started', 'success')
		ORDER BY id DESC LIMIT 1`, batch.Actor, host.Name, batch.ID).Scan(&status, &jobID)
	if err == nil {
		host.Message = ""
		host.JobID = jobID
		host.State = "accepted"
		if jobID == "" && status == "success" {
			host.State = "completed"
		}
	}
}
