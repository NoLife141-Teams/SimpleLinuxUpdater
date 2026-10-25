package updates

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"debian-updater/internal/postchecks"
	"debian-updater/internal/servers"
)

func applicationCheckCommand(check postchecks.Check) string {
	if check.Kind == "systemd_active" {
		return "if command -v systemctl >/dev/null 2>&1; then systemctl is-active --quiet -- " + shellQuote(check.Unit) + " 2>/dev/null; else printf SLU_TOOL_MISSING; exit 127; fi"
	}
	return "if command -v curl >/dev/null 2>&1; then curl --disable --globoff --silent --noproxy '*' --proto '=http,https' --proto-redir '=http,https' --max-redirs 0 --connect-timeout 3 --max-time 5 --output /dev/null --write-out '%{http_code}' --url " + shellQuote(check.URL) + " 2>/dev/null; else printf SLU_TOOL_MISSING; exit 127; fi"
}

func runApplicationChecks(ctx context.Context, session HostMaintenanceSession, profile *postchecks.Snapshot, now func() time.Time, sleep func(context.Context, time.Duration) error) []postchecks.Result {
	if profile == nil {
		return nil
	}
	results := make([]postchecks.Result, 0, len(profile.Checks))
	for _, check := range profile.Checks {
		started := now()
		result := postchecks.Result{ID: check.ID, Name: check.Name, Kind: check.Kind, Mode: check.Mode}
		if err := postchecks.ValidateCheck(check); err != nil {
			result.Details = "Invalid application check configuration"
			results = append(results, result)
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, time.Duration(check.TimeoutSeconds)*time.Second)
		deadline := started.Add(time.Duration(check.TimeoutSeconds) * time.Second)
		for {
			if checkCtx.Err() != nil || !now().Before(deadline) {
				result.Details = applicationCheckStoppedDetail(result.Details, ctx.Err())
				break
			}
			result.Attempts++
			output, err := session.RunCommand(checkCtx, HostCommandRequest{Operation: "postcheck.application." + check.ID, Command: applicationCheckCommand(check), Effect: HostCommandEffectReadOnly, ReplayPolicy: ReplayNever})
			if checkCtx.Err() != nil {
				result.Details = applicationCheckStoppedDetail(result.Details, ctx.Err())
				break
			}
			if strings.TrimSpace(output.Stdout) == "SLU_TOOL_MISSING" {
				result.Details = "Required remote tool is unavailable"
				break
			}
			if check.Kind == "systemd_active" {
				result.Passed = err == nil
				result.Details = "Systemd unit is not active or could not be inspected"
				if result.Passed {
					result.Details = "Systemd unit is active"
				}
			} else {
				code, parseErr := strconv.Atoi(strings.TrimSpace(output.Stdout))
				result.Passed = err == nil && parseErr == nil && code == check.ExpectedStatus
				result.Details = "HTTP request failed or returned an unexpected status"
				if err == nil && parseErr == nil && code >= 100 && code <= 599 {
					result.Details = fmt.Sprintf("HTTP status %d; expected %d", code, check.ExpectedStatus)
				}
			}
			if result.Passed {
				break
			}
			remaining := deadline.Sub(now())
			delay := time.Duration(check.IntervalSeconds) * time.Second
			if delay > remaining {
				delay = remaining
			}
			if delay <= 0 || sleep(checkCtx, delay) != nil {
				result.Details = applicationCheckStoppedDetail(result.Details, ctx.Err())
				break
			}
		}
		cancel()
		result.DurationMS = now().Sub(started).Milliseconds()
		results = append(results, result)
		if ctx.Err() != nil {
			break
		}
	}
	return results
}

func (s *Service) TestApplicationChecks(ctx context.Context, server servers.Server, profile *postchecks.Snapshot) ([]postchecks.Result, error) {
	if profile == nil {
		return nil, fmt.Errorf("no application post-check profile is assigned")
	}
	deps := s.EnsureDeps()
	session, err := deps.HostMaintenanceSessions.Open(ctx, HostMaintenanceSessionRequest{Server: server, RetryPolicy: RetryPolicy{MaxAttempts: 1}, DialOperation: "postchecks.test.ssh_dial", CommandTimeout: deps.LoadCommandTimeout()})
	if err != nil {
		return nil, fmt.Errorf("could not open the read-only Host Maintenance Session")
	}
	defer func() { _ = session.Close() }()
	results := runApplicationChecks(ctx, session, profile, deps.Now, deps.SleepContext)
	if ctx.Err() != nil {
		return results, ctx.Err()
	}
	return results, nil
}
func (r *withActorRunner) captureApplicationProfile() error {
	profile, err := r.deps().LoadApplicationPostchecks(r.server)
	if err != nil {
		return err
	}
	r.applicationProfile = profile
	return r.persistApplicationChecks()
}
func (r *withActorRunner) persistApplicationChecks() error {
	if r.jobID == "" {
		return nil
	}
	jm := r.currentJobManager()
	if jm == nil {
		return fmt.Errorf("job manager unavailable")
	}
	return jm.MergeActiveMetadata(r.jobID, map[string]any{"application_postcheck_profile": r.applicationProfile, "application_postcheck_results": r.applicationResults})
}
func (r *withActorRunner) applicationProfileChanged() (bool, error) {
	latest, err := r.deps().LoadApplicationPostchecks(r.server)
	if err != nil {
		return false, err
	}
	if reflect.DeepEqual(r.applicationProfile, latest) {
		return false, nil
	}
	r.applicationProfile = latest
	r.applicationResults = nil
	return true, r.persistApplicationChecks()
}

func applicationCheckStoppedDetail(last string, err error) string {
	reason := "Recovery timeout reached"
	if err != nil {
		reason = "Application check cancelled"
	}
	if last == "" {
		return reason
	}
	return last + "; " + reason
}
