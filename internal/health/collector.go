package health

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

type ProbeKind string

const (
	ProbeOS                    ProbeKind = "os"
	ProbeRunningKernel         ProbeKind = "running_kernel"
	ProbeLatestInstalledKernel ProbeKind = "latest_installed_kernel"
	ProbeUptime                ProbeKind = "uptime"
	ProbeDisk                  ProbeKind = "disk"
	ProbeAPT                   ProbeKind = "apt"
	ProbeReboot                ProbeKind = "reboot"
)

type ProbeResult struct {
	Output         string
	Stderr         string
	Status         string
	Details        string
	FreeKB         int64
	TotalKB        int64
	RebootRequired *bool
	Err            error
}

type Collector struct {
	Now   func() time.Time
	Probe func(context.Context, ProbeKind) ProbeResult
}

func (c Collector) Capture(ctx context.Context, serverName string) CollectedFacts {
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}
	facts := CollectedFacts{
		ServerName:   serverName,
		CollectedAt:  now.Format(time.RFC3339),
		OSPrettyName: "Unknown",
		DiskStatus:   "unknown",
		AptStatus:    "unknown",
		RawJSON:      "{}",
	}
	probe := func(kind ProbeKind) ProbeResult {
		if c.Probe == nil {
			return ProbeResult{}
		}
		return c.Probe(ctx, kind)
	}
	osResult := probe(ProbeOS)
	if osResult.Err == nil {
		if value := truncateObservation(strings.TrimSpace(osResult.Output), 160); value != "" {
			facts.OSPrettyName = value
		}
	}
	runningKernelResult := probe(ProbeRunningKernel)
	if runningKernelResult.Err == nil {
		facts.RunningKernelVersion = truncateObservation(strings.TrimSpace(runningKernelResult.Output), 160)
	}
	latestInstalledKernelResult := probe(ProbeLatestInstalledKernel)
	if latestInstalledKernelResult.Err == nil {
		facts.LatestInstalledKernelVersion = truncateObservation(strings.TrimSpace(latestInstalledKernelResult.Output), 160)
	}
	uptimeResult := probe(ProbeUptime)
	if uptimeResult.Err == nil {
		fields := strings.Fields(uptimeResult.Output)
		if len(fields) > 0 {
			if value, err := strconv.ParseFloat(fields[0], 64); err == nil && value >= 0 {
				facts.UptimeSeconds = int64(value)
			}
		}
	}
	diskResult := probe(ProbeDisk)
	facts.DiskStatus = normalizedProbeStatus(diskResult.Status)
	facts.DiskDetails = diskResult.Details
	facts.DiskFreeKB = diskResult.FreeKB
	facts.DiskTotalKB = diskResult.TotalKB
	aptResult := probe(ProbeAPT)
	facts.AptStatus = normalizedProbeStatus(aptResult.Status)
	facts.AptDetails = aptResult.Details
	rebootResult := probe(ProbeReboot)
	facts.RebootRequired = rebootResult.RebootRequired
	raw, err := json.Marshal(map[string]any{
		"os":                      probeSummary(osResult),
		"running_kernel":          probeSummary(runningKernelResult),
		"latest_installed_kernel": probeSummary(latestInstalledKernelResult),
		"uptime":                  probeSummary(uptimeResult),
		"disk":                    probeSummary(diskResult),
		"apt":                     probeSummary(aptResult),
		"reboot":                  probeSummary(rebootResult),
	})
	if err == nil {
		facts.RawJSON = string(raw)
	}
	return facts
}

func normalizedProbeStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "ok", "warning", "error":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "unknown"
	}
}

func truncateObservation(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func probeSummary(result ProbeResult) map[string]any {
	errText := ""
	if result.Err != nil {
		errText = result.Err.Error()
	}
	return map[string]any{
		"status":  result.Status,
		"details": result.Details,
		"stderr":  truncateObservation(result.Stderr, 160),
		"error":   errText,
	}
}
