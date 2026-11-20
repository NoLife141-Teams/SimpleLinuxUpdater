package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	notificationpkg "debian-updater/internal/notifications"
	policypkg "debian-updater/internal/policies"

	"github.com/gin-gonic/gin"
)

const restoreResumePreviewRoute = "/api/backup/restore-isolation/resume-preview"
const restoreResumePreviewLifetime = 5 * time.Minute

var restoreResumePreviewKey = []byte(rand.Text())

type restoreResumeReview struct {
	revision atomic.Uint64
}

type restoreResumeHost struct {
	Name      string     `json:"name"`
	Allowed   bool       `json:"allowed"`
	Reason    string     `json:"reason"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
}

type restoreResumeSchedule struct {
	Server string `json:"server"`
	Policy string `json:"policy"`
	At     string `json:"at"`
	Reason string `json:"reason,omitempty"`
	Status string `json:"status"`
}

type restoreResumeImpact struct {
	Hosts         []restoreResumeHost                    `json:"hosts"`
	Schedules     []restoreResumeSchedule                `json:"schedules"`
	Notifications []notificationpkg.RestoreDeliveryQueue `json:"notifications"`
}

type restoreResumePreview struct {
	Impact    restoreResumeImpact `json:"impact"`
	Complete  bool                `json:"complete"`
	Token     string              `json:"token,omitempty"`
	ExpiresAt time.Time           `json:"expires_at,omitzero"`
}

type restoreResumeSnapshot struct {
	Impact               restoreResumeImpact
	Activation           string
	Inventory            []restoreResumeHostBinding
	PolicyFingerprint    string
	NotificationRevision string
	ReviewRevision       uint64
}

type restoreResumeHostBinding struct {
	Name, Identity, Target, HostKey string
}

func collectRestoreResumeSnapshot(ctx context.Context, deps AppDeps, activation string, now time.Time) restoreResumeSnapshot {
	snapshot := restoreResumeSnapshot{Activation: activation, ReviewRevision: deps.restoreResumeReview.revision.Load()}
	observations, legacy, aptErr := deps.RestoreAPTValidation.all(ctx)
	state := deps.ServerState
	state.Lock()
	inventory := state.CloneServers()
	sort.Slice(inventory, func(i, j int) bool { return inventory[i].Name < inventory[j].Name })
	if aptErr == nil {
		snapshot.Impact.Hosts = make([]restoreResumeHost, 0, len(inventory))
	}
	for _, server := range inventory {
		observation, required := observations[server.Identity]
		snapshot.Inventory = append(snapshot.Inventory, restoreResumeHostBinding{
			Name: server.Name, Identity: server.Identity, Target: server.APTTarget(), HostKey: observation.key,
		})
		if aptErr != nil {
			continue
		}
		needsReconciliation := state.StatusMap()[server.Name].RequiresAPTReconciliation()
		host := restoreResumeHost{Name: server.Name, Allowed: true, Reason: "no_validation_required"}
		switch {
		case server.Disabled:
			host.Allowed, host.Reason = false, "disabled"
		case needsReconciliation:
			host.Allowed, host.Reason = false, "reconciliation_required"
		case legacyAPTIncidentForServer(legacy, server).jobID != "":
			host.Allowed, host.Reason = false, "legacy_target_unverified"
		case required:
			facts := observation.facts(server, now, true, false)
			host.CheckedAt = facts.CheckedAt
			host.Allowed = facts.State == "healthy" && observation.key != "" &&
				observation.target != "" && observation.target == server.APTTarget()
			host.Reason = facts.State
			if facts.State == "healthy" && !host.Allowed {
				host.Reason = "pending"
			}
		}
		snapshot.Impact.Hosts = append(snapshot.Impact.Hosts, host)
	}
	state.Unlock()

	nextTick := now.Truncate(time.Minute).Add(time.Minute)
	snapshot.Impact.Schedules, snapshot.PolicyFingerprint = restoreResumeSchedules(deps, inventory, nextTick)
	if notifications, ok := deps.NotificationService.(interface {
		RestoreDeliveryResumption(context.Context, time.Time) ([]notificationpkg.RestoreDeliveryQueue, string, error)
	}); ok {
		queues, revision, err := notifications.RestoreDeliveryResumption(ctx, now)
		if err == nil {
			snapshot.Impact.Notifications, snapshot.NotificationRevision = queues, revision
		}
	}
	if !snapshot.Impact.complete() {
		// A recovered snapshot may be identical to one issued before a read
		// failure. Require a fresh review even when all the values return.
		deps.restoreResumeReview.revision.Add(1)
	}
	return snapshot
}

func (impact restoreResumeImpact) complete() bool {
	return impact.Hosts != nil && impact.Schedules != nil && impact.Notifications != nil
}

func restoreResumeSchedules(deps AppDeps, servers []Server, nextTick time.Time) ([]restoreResumeSchedule, string) {
	projected, fingerprint, err := deps.PolicyService.ProjectRestoreResumption(policypkg.ScheduleProjectionRequest{Now: nextTick, Servers: servers})
	if err != nil {
		return nil, fingerprint
	}
	rows := make([]restoreResumeSchedule, 0, len(servers))
	for _, server := range servers {
		next := projected.Servers[server.Name].NextRun
		if next.ScheduledForUTC == "" {
			continue
		}
		rows = append(rows, restoreResumeSchedule{Server: server.Name, Policy: next.PolicyName,
			At: next.ScheduledForUTC, Reason: next.Reason, Status: next.Status})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].At != rows[j].At {
			return rows[i].At < rows[j].At
		}
		return rows[i].Server < rows[j].Server
	})
	return rows, fingerprint
}

func (s restoreResumeSnapshot) signature(expires string) []byte {
	mac := hmac.New(sha256.New, restoreResumePreviewKey)
	_, _ = mac.Write([]byte(expires))
	_ = json.NewEncoder(mac).Encode(s)
	return mac.Sum(nil)
}

func (s restoreResumeSnapshot) preview(now time.Time) restoreResumePreview {
	if !s.Impact.complete() {
		return restoreResumePreview{Impact: s.Impact}
	}
	expires := now.Add(restoreResumePreviewLifetime).Truncate(time.Second)
	stamp := strconv.FormatInt(expires.Unix(), 10)
	return restoreResumePreview{
		Impact: s.Impact, Complete: true, ExpiresAt: expires,
		Token: stamp + "." + hex.EncodeToString(s.signature(stamp)),
	}
}

func (s restoreResumeSnapshot) confirmationDeadline(token string, now time.Time) time.Time {
	if !s.Impact.complete() {
		return time.Time{}
	}
	stamp, signature, found := strings.Cut(token, ".")
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if !found || err != nil || seconds <= now.Unix() || len(signature) != 2*sha256.Size {
		return time.Time{}
	}
	supplied, err := hex.DecodeString(signature)
	if err != nil || !hmac.Equal(s.signature(stamp), supplied) {
		return time.Time{}
	}
	deadline := time.Unix(seconds, 0)
	for _, host := range s.Impact.Hosts {
		if host.Allowed && host.CheckedAt != nil {
			deadline = earlierResumptionDeadline(deadline, host.CheckedAt.Add(restoreAPTProofLifetime+time.Second))
		}
	}
	for _, run := range s.Impact.Schedules {
		if scheduledAt, err := time.Parse(time.RFC3339Nano, run.At); err == nil {
			deadline = earlierResumptionDeadline(deadline, scheduledAt)
		}
	}
	for _, queue := range s.Impact.Notifications {
		if queue.RateLimitedUntil != nil {
			deadline = earlierResumptionDeadline(deadline, *queue.RateLimitedUntil)
		}
	}
	return deadline
}

func earlierResumptionDeadline(current, candidate time.Time) time.Time {
	if candidate.Before(current) {
		return candidate
	}
	return current
}

func handleRestoreResumePreview(c *gin.Context, deps AppDeps) {
	setNoStoreHeaders(c)
	activation, active, err := deps.RestoreIsolation.Activation()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Restore isolation state is unavailable."})
		return
	}
	if !active {
		c.JSON(http.StatusConflict, gin.H{"error": "There is no active restore isolation to review."})
		return
	}
	now := deps.Now().UTC()
	snapshot := collectRestoreResumeSnapshot(c.Request.Context(), deps, activation, now)
	c.JSON(http.StatusOK, snapshot.preview(now))
}
