package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh"
)

type onboardingHostSession struct{ HostMaintenanceSession }

func (s onboardingHostSession) Read(ctx context.Context, command string) (string, error) {
	result, err := s.RunCommand(ctx, updatespkg.HostCommandRequest{Operation: "server_onboarding", Command: command, Effect: updatespkg.HostCommandEffectReadOnly, ReplayPolicy: updatespkg.ReplayNever})
	return result.Stdout, err
}

func newServerOnboardingService(deps AppDeps) *serverpkg.OnboardingService {
	return serverpkg.NewOnboardingService(serverpkg.OnboardingDeps{
		Inventory: deps.ServerInventoryService,
		ResolveGlobalKey: func(ctx context.Context) (string, error) {
			resolved, err := deps.GlobalSSHCredential.Resolve(ctx, "")
			return resolved.Key, err
		},
		Open: func(ctx context.Context, server Server, fingerprint string) (serverpkg.OnboardingSession, error) {
			callback, err := serverpkg.OnboardingHostKeyCallback(appKnownHostsDeps(deps.DBPath), fingerprint)
			if err != nil {
				return nil, err
			}
			factory := newHostMaintenanceSessionFactory(serverpkg.BuildAuthMethods, func() (ssh.HostKeyCallback, error) { return callback, nil }, func(server Server, config *ssh.ClientConfig) (sshConnection, error) {
				return getDialSSHConnection()(server, config)
			})
			session, err := factory.Open(ctx, HostMaintenanceSessionRequest{Server: server, CommandTimeout: 8 * time.Second, RetryPolicy: RetryPolicy{MaxAttempts: 1}, DialOperation: "server_onboarding"})
			if err != nil {
				return nil, err
			}
			return onboardingHostSession{session}, nil
		},
	})
}

func registerServerOnboardingRoutes(r *gin.Engine, deps AppDeps) {
	for _, route := range []struct {
		path   string
		create bool
	}{{"/api/servers/onboarding/check", false}, {"/api/servers/onboarding", true}} {
		create := route.create
		r.POST(route.path, func(c *gin.Context) {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128*1024)
			var draft serverpkg.OnboardingDraft
			if err := c.ShouldBindJSON(&draft); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid or oversized server details"})
				return
			}
			if create {
				result, report, err := deps.ServerOnboardingService.Create(c.Request.Context(), draft)
				if err != nil {
					writeOnboardingError(c, err)
					return
				}
				if !report.Ready {
					c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "Server verification failed. Correct the failed checks and retry.", "report": report})
					return
				}
				writeServerInventoryCommandResult(c, result, http.StatusCreated, result.Server)
				if result.Succeeded() {
					deps.NotifyDashboardEvent("server-created")
				}
				return
			}
			report, err := deps.ServerOnboardingService.Check(c.Request.Context(), draft)
			if err != nil {
				writeOnboardingError(c, err)
				return
			}
			c.JSON(http.StatusOK, report)
		})
	}
}

func writeOnboardingError(c *gin.Context, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, serverpkg.ErrNameExists) || errors.Is(err, serverpkg.ErrEndpointExists) {
		status = http.StatusConflict
	}
	if errors.Is(err, serverpkg.ErrOnboardingBusy) {
		status = http.StatusTooManyRequests
		c.Header("Retry-After", "2")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		status = http.StatusRequestTimeout
	}
	c.JSON(status, gin.H{"error": err.Error()})
}
