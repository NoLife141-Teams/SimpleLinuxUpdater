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
	"time"

	notificationpkg "debian-updater/internal/notifications"
	policypkg "debian-updater/internal/policies"

	"github.com/gin-gonic/gin"
)

const restoreResumePreviewRoute = "/api/backup/restore-isolation/resume-preview"
const restoreResumePreviewLifetime = 5 * time.Minute

var restoreResumePreviewKey = []byte(rand.Text())

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
}

type restoreResumeImpact struct {
	Hosts         []restoreResumeHost                    `json:"hosts"`
	Schedules     []restoreResumeSchedule                `json:"schedules"`
	Notifications []notificationpkg.RestoreDeliveryQueue `json:"notifications"`
}

type restoreResumePreview struct {
	Impact    restoreResumeImpact `json:"impact"`
	Token     string              `json:"token"`
	ExpiresAt time.Time           `json:"expires_at"`
}

type restoreResumeSnapshot struct {
	Impact               restoreResumeImpact
	Activation           string
	Inventory            []restoreResumeHostBinding
	PolicyRevision       int64
	NotificationRevision string
}

type restoreResumeHostBinding struct {
	Name, Identity, Target, HostKey string
}

func collectRestoreResumeSnapshot(ctx context.Context, deps AppDeps, activation string, now time.Time) restoreResumeSnapshot {
	snapshot := restoreResumeSnapshot{Activation: activation}
	observations, aptErr := deps.RestoreAPTValidation.all(ctx)
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
	snapshot.Impact.Schedules, snapshot.PolicyRevision = restoreResumeSchedules(deps, inventory, nextTick)
	if notifications, ok := deps.NotificationService.(interface {
		RestoreDeliveryResumption(context.Context, time.Time) ([]notificationpkg.RestoreDeliveryQueue, string, error)
	}); ok {
		queues, revision, err := notifications.RestoreDeliveryResumption(ctx, now)
		if err == nil {
			snapshot.Impact.Notifications, snapshot.NotificationRevision = queues, revision
		}
	}
	return snapshot
}

func restoreResumeSchedules(deps AppDeps, servers []Server, nextTick time.Time) ([]restoreResumeSchedule, int64) {
	repository, ok := deps.PolicyRepository.(policySchedulerStateRevisionRepository)
	var revision int64
	if ok {
		var err error
		revision, err = repository.LoadSchedulerStateRevision()
		if err != nil {
			return nil, 0
		}
	}
	projected, err := deps.PolicyService.ProjectSchedule(policypkg.ScheduleProjectionRequest{Now: nextTick, Servers: servers})
	if err != nil {
		return nil, revision
	}
	if ok {
		if current, err := repository.LoadSchedulerStateRevision(); err != nil || current != revision {
			return nil, revision
		}
	}
	rows := make([]restoreResumeSchedule, 0, len(projected.Servers))
	for server, projection := range projected.Servers {
		next := projection.NextRun
		if next.ScheduledForUTC == "" {
			continue
		}
		rows = append(rows, restoreResumeSchedule{Server: server, Policy: next.PolicyName,
			At: next.ScheduledForUTC, Reason: next.Reason})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].At != rows[j].At {
			return rows[i].At < rows[j].At
		}
		return rows[i].Server < rows[j].Server
	})
	return rows, revision
}

func (s restoreResumeSnapshot) signature(expires string) []byte {
	mac := hmac.New(sha256.New, restoreResumePreviewKey)
	_, _ = mac.Write([]byte(expires))
	_ = json.NewEncoder(mac).Encode(s)
	return mac.Sum(nil)
}

func (s restoreResumeSnapshot) preview(now time.Time) restoreResumePreview {
	expires := now.Add(restoreResumePreviewLifetime).Truncate(time.Second)
	stamp := strconv.FormatInt(expires.Unix(), 10)
	return restoreResumePreview{
		Impact: s.Impact, ExpiresAt: expires,
		Token: stamp + "." + hex.EncodeToString(s.signature(stamp)),
	}
}

func (s restoreResumeSnapshot) confirms(token string, now time.Time) bool {
	stamp, signature, found := strings.Cut(token, ".")
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if !found || err != nil || seconds <= now.Unix() || len(signature) != 2*sha256.Size {
		return false
	}
	supplied, err := hex.DecodeString(signature)
	return err == nil && hmac.Equal(s.signature(stamp), supplied)
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
