package health

import (
	"context"
	"errors"
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
