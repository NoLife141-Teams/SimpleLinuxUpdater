package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	internalbackup "debian-updater/internal/backup"
	policypkg "debian-updater/internal/policies"

	"github.com/gin-gonic/gin"
)

const restoreIsolationResumeRoute = "/api/backup/restore-isolation/resume"
const restoreIsolationMessage = "Installation is isolated after restore. Review and resume external work in Admin."

func isolateHostMaintenanceSessions(isolation *internalbackup.RestoreIsolation, factory HostMaintenanceSessionFactory) HostMaintenanceSessionFactory {
	return HostMaintenanceSessionFactoryFunc(func(ctx context.Context, req HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		if isolation.BlocksExternalWork() {
			return nil, errors.New("installation is isolated after restore")
		}
		return factory.Open(ctx, req)
	})
}

func guardRestoredAPTSession(validation *restoreAPTValidation) HostMaintenanceSessionFactory {
	return HostMaintenanceSessionFactoryFunc(func(ctx context.Context, req HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		var key string
		if req.PackageMaintenance {
			var err error
			key, err = validation.authorize(ctx, req.Server)
			if err != nil {
				return nil, err
			}
			if key != "" {
				verify := req.VerifyHostKey
				hostKeyRejected := req.OnHostKeyRejected
				req.VerifyHostKey = func(fingerprint string) error {
					if fingerprint != key {
						return errRestoredAPTKeyChanged
					}
					if verify != nil {
						return verify(fingerprint)
					}
					return nil
				}
				req.OnHostKeyRejected = func(ctx context.Context) {
					validation.invalidateHostKey(ctx, req.Server, key)
					if hostKeyRejected != nil {
						hostKeyRejected(ctx)
					}
				}
			}
		}
		session, err := validation.sessions.Open(ctx, req)
		if key != "" && err != nil && restoreAPTHostKeyError(err) {
			validation.invalidateHostKey(ctx, req.Server, key)
		}
		return session, err
	})
}

func restoreIsolationMiddleware(isolation *internalbackup.RestoreIsolation) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodPost && requiresExternalWork(c.FullPath()) && isolation.BlocksExternalWork() {
			setNoStoreHeaders(c)
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": restoreIsolationMessage, "code": "restore_isolated"})
			return
		}
		c.Next()
	}
}

func requiresExternalWork(route string) bool {
	switch route {
	case "/api/update/:name", "/api/autoremove/:name", "/api/repair/:name", "/api/reboot/:name",
		"/api/servers/:name/facts/refresh", "/api/servers/:name/postchecks/test",
		"/api/approve/:name", "/api/approve-security/:name", "/api/approve-security-kept-back/:name", "/api/approve-full/:name",
		"/api/status/batches", "/api/sudoers/:name", "/api/sudoers/disable/:name",
		"/api/hostkeys/scan", "/api/hostkeys/trust", "/api/servers/onboarding", "/api/servers/onboarding/check",
		"/api/notifications/test":
		return true
	}
	return false
}

func handleRestoreIsolationResume(c *gin.Context, deps AppDeps) {
	setNoStoreHeaders(c)
	var confirmed struct {
		Confirmation string `json:"confirmation"`
		PreviewToken string `json:"preview_token"`
	}
	if c.ShouldBindJSON(&confirmed) != nil || confirmed.Confirmation != "RESUME" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Confirm with RESUME before allowing external work."})
		return
	}
	activation, active, err := deps.RestoreIsolation.Activation()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Restore isolation state is unavailable."})
		return
	}
	if !active {
		c.JSON(http.StatusOK, gin.H{"restore_isolation": "inactive"})
		return
	}
	if maintenanceExclusiveLeaseFromContext(c) == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Exclusive installation admission is required."})
		return
	}
	ctx := c.Request.Context()
	now := deps.Now().UTC()
	snapshot := collectRestoreResumeSnapshot(ctx, deps, activation, now)
	deadline := snapshot.confirmationDeadline(confirmed.PreviewToken, now)
	reviewChanged := func(snapshot restoreResumeSnapshot, now time.Time) {
		c.JSON(http.StatusConflict, gin.H{"error": "Review the current resumption impact before confirming.",
			"code": "restore_resume_preview_changed", "preview": snapshot.preview(now)})
	}
	if deadline.IsZero() {
		reviewChanged(snapshot, now)
		return
	}
	minute := now.Truncate(time.Minute)
	if err := prepareRestoreIsolationResume(deps, minute); err != nil {
		log.Printf("restore isolation scheduler rebase failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Scheduling could not be safely rebased. External work stays paused."})
		return
	}
	if err := deps.AuditService.Record(actorFromContext(c), clientIPFromContext(c), "backup.restore_isolation.resume",
		"backup", "state", "success", "Approved explicit resumption of external work after isolated restore",
		nil); err != nil {
		log.Printf("restore isolation resumption audit failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "The resumption decision could not be recorded. External work stays paused."})
		return
	}
	if err := ctx.Err(); err != nil {
		return
	}
	approvalAt := deps.Now().UTC()
	if !approvalAt.Before(deadline) {
		reviewChanged(collectRestoreResumeSnapshot(ctx, deps, activation, approvalAt), approvalAt)
		return
	}
	if deps.RestoreAPTValidation != nil {
		if err := deps.RestoreAPTValidation.approve(ctx, approvalAt); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "APT validation could not be recorded. External work stays paused."})
			return
		}
	}
	if err := deps.RestoreIsolation.Reactivate(deps.Now, deadline); err != nil {
		if errors.Is(err, internalbackup.ErrRestoreResumptionExpired) {
			changedAt := deps.Now().UTC()
			reviewChanged(collectRestoreResumeSnapshot(ctx, deps, activation, changedAt), changedAt)
			return
		}
		log.Printf("restore isolation release failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "The resumption decision could not be applied. Retry in Admin."})
		return
	}
	deps.NotifyDashboardEvent("restore-isolation-resumed")
	c.JSON(http.StatusOK, gin.H{"restore_isolation": "inactive"})
}

func prepareRestoreIsolationResume(deps AppDeps, minute time.Time) error {
	if err := policypkg.ClearSchedulerDenials(deps.DB()); err != nil {
		return err
	}
	deps.PolicyService.ClearMissedTicks()
	if store, ok := deps.PolicyRepository.(policySchedulerWatermarkRepository); ok {
		return store.SaveSchedulerWatermark(minute)
	}
	return nil
}
