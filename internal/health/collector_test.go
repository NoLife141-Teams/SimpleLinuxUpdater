package health

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCollectorCapturesPartialHostHealthObservation(t *testing.T) {
	now := time.Date(2026, 7, 11, 20, 0, 0, 0, time.UTC)
	collector := Collector{
		Now: func() time.Time { return now },
		Probe: func(_ context.Context, kind ProbeKind) ProbeResult {
			switch kind {
			case ProbeOS:
				return ProbeResult{Output: "Ubuntu 24.04 LTS\n"}
			case ProbeUptime:
				return ProbeResult{Output: "90061.25 1.0"}
			case ProbeDisk:
				return ProbeResult{Status: "ok", Details: "16 GiB free", FreeKB: 16 * 1024 * 1024, TotalKB: 64 * 1024 * 1024}
			case ProbeAPT:
				return ProbeResult{Status: "warning", Details: "dpkg requires attention", Err: errors.New("apt check failed")}
			case ProbeReboot:
				required := true
				return ProbeResult{RebootRequired: &required}
			default:
				return ProbeResult{}
			}
		},
	}

	got := collector.Capture(context.Background(), "edge-01")
	if got.ServerName != "edge-01" || got.CollectedAt != "2026-07-11T20:00:00Z" {
		t.Fatalf("identity = %+v", got)
	}
	if got.OSPrettyName != "Ubuntu 24.04 LTS" || got.UptimeSeconds != 90061 {
		t.Fatalf("host facts = %+v", got)
	}
	if got.DiskStatus != "ok" || got.DiskFreeKB != 16*1024*1024 || got.DiskTotalKB != 64*1024*1024 {
		t.Fatalf("disk facts = %+v", got)
	}
	if got.AptStatus != "warning" || got.RebootRequired == nil || !*got.RebootRequired {
		t.Fatalf("health facts = %+v", got)
	}
	if got.RawJSON == "" || got.RawJSON == "{}" {
		t.Fatalf("raw observation not retained: %q", got.RawJSON)
	}
}

func TestCollectorUsesSafeFallbacksForFailedProbes(t *testing.T) {
	collector := Collector{Probe: func(context.Context, ProbeKind) ProbeResult {
		return ProbeResult{Err: errors.New("timeout")}
	}}
	got := collector.Capture(context.Background(), "offline-01")
	if got.OSPrettyName != "Unknown" || got.DiskStatus != "unknown" || got.AptStatus != "unknown" {
		t.Fatalf("fallback observation = %+v", got)
	}
}

func TestCollectorHandlesInvalidOutputAndTimeoutWithoutLosingObservationIdentity(t *testing.T) {
	now := time.Date(2026, 7, 11, 21, 30, 0, 0, time.UTC)
	collector := Collector{
		Now: func() time.Time { return now },
		Probe: func(_ context.Context, kind ProbeKind) ProbeResult {
			switch kind {
			case ProbeOS:
				return ProbeResult{Output: strings.Repeat("x", 220)}
			case ProbeUptime:
				return ProbeResult{Output: "not-a-number"}
			case ProbeDisk:
				return ProbeResult{Status: "unexpected", Err: context.DeadlineExceeded}
			case ProbeAPT:
				return ProbeResult{Err: errors.New("command failed")}
			default:
				return ProbeResult{}
			}
		},
	}

	got := collector.Capture(context.Background(), "slow-host")
	if got.ServerName != "slow-host" || got.CollectedAt != "2026-07-11T21:30:00Z" {
		t.Fatalf("observation identity changed: %+v", got)
	}
	if len(got.OSPrettyName) != 160 || got.UptimeSeconds != 0 {
		t.Fatalf("invalid output handling = %+v", got)
	}
	if got.DiskStatus != "unknown" || got.AptStatus != "unknown" {
		t.Fatalf("failed probe status = %+v", got)
	}
	if !strings.Contains(got.RawJSON, "context deadline exceeded") || !strings.Contains(got.RawJSON, "command failed") {
		t.Fatalf("raw failures not retained: %s", got.RawJSON)
	}
}
