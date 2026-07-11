package updates

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	rebootCheckErrorPattern     = regexp.MustCompile(`\b(error|failed|failure|unable|cannot|can't)\b`)
	rebootRequiredPhrasePattern = regexp.MustCompile(`\b(reboot required|requires reboot|restart required|system restart required|needs reboot|need reboot)\b`)
)

// HealthObservation is the transport-neutral result of interpreting health checks.
type HealthObservation struct {
	DiskStatus     string
	DiskFreeKB     int64
	DiskTotalKB    int64
	DiskDetails    string
	AptStatus      string
	AptDetails     string
	RebootRequired *bool
}

// HealthResultInterpreter supplies parsing policies while keeping result precedence shared.
type HealthResultInterpreter struct {
	Status         func(PrecheckResult) string
	DiskFree       func(string) (int64, bool)
	DiskFreeTotal  func(string) (int64, int64, bool)
	RebootRequired func(PrecheckResult) (bool, bool)
}

func (d HealthResultInterpreter) withDefaults() HealthResultInterpreter {
	if d.Status == nil {
		d.Status = func(result PrecheckResult) string {
			if result.Passed {
				return "ok"
			}
			return "critical"
		}
	}
	if d.DiskFree == nil {
		d.DiskFree = diskFreeKBFromHealthOutput
	}
	if d.DiskFreeTotal == nil {
		d.DiskFreeTotal = diskFreeTotalKBFromHealthOutput
	}
	if d.RebootRequired == nil {
		d.RebootRequired = rebootRequiredFromHealthResult
	}
	return d
}

// ApplyHealthResults applies results in order; later results supersede earlier dimensions.
func ApplyHealthResults(health *HealthObservation, results []PrecheckResult, interpreter HealthResultInterpreter) bool {
	if health == nil {
		return false
	}
	interpreter = interpreter.withDefaults()
	applied := false
	for _, result := range results {
		switch result.Name {
		case "disk_space":
			applied = true
			health.DiskStatus = interpreter.Status(result)
			if freeKB, totalKB, ok := interpreter.DiskFreeTotal(result.Output); ok {
				health.DiskFreeKB, health.DiskTotalKB = freeKB, totalKB
			} else if freeKB, ok := interpreter.DiskFree(result.Output); ok {
				health.DiskFreeKB = freeKB
			}
			health.DiskDetails = result.Details
		case "apt_health", PostcheckNameAptHealth:
			applied = true
			health.AptStatus = interpreter.Status(result)
			health.AptDetails = result.Details
		case PostcheckNameRebootNeeded:
			if strings.TrimSpace(result.Error) != "" {
				continue
			}
			required, known := interpreter.RebootRequired(result)
			if known {
				health.RebootRequired = &required
				applied = true
			}
		}
	}
	return applied
}

func (r SQLiteServerFactsRepository) CaptureCompletion(completion MaintenanceCompletion) error {
	meta := map[string]any{}
	if strings.TrimSpace(completion.RawJSON) != "" {
		if err := json.Unmarshal([]byte(completion.RawJSON), &meta); err != nil {
			meta = map[string]any{}
		}
	}
	packageCount := healthMetaInt(meta, "pending_package_count", "approved_package_count")
	securityCount := healthMetaInt(meta, "security_package_count")
	if securityCount == 0 && strings.HasPrefix(healthMetaString(meta, "approval_scope"), "security") {
		securityCount = healthMetaInt(meta, "approved_package_count")
	}
	if discovery := healthMetaMap(meta, "discovery"); discovery != nil {
		if packageCount == 0 {
			packageCount = healthMetaInt(discovery, "pending_package_count")
		}
		if securityCount == 0 {
			securityCount = healthMetaInt(discovery, "security_package_count")
		}
	}
	record := HealthSnapshotRecord{
		ServerName: completion.ServerName, CapturedAt: completion.CompletedAt, Source: "audit",
		PackageCount: packageCount, SecurityCount: securityCount,
		DiskStatus: "unknown", AptStatus: "unknown", RawJSON: completion.RawJSON,
	}
	switch completion.Kind {
	case MaintenanceKindUpdate:
		record.LastUpdateStatus = strings.TrimSpace(completion.Status)
	case MaintenanceKindScheduledRun:
		record.LastScanStatus = strings.TrimSpace(completion.Status)
	default:
		return fmt.Errorf("unknown maintenance completion kind %q", completion.Kind)
	}
	results := healthResultsFromMeta(meta, "precheck_results")
	results = append(results, healthResultsFromMeta(meta, "postcheck_results")...)
	health := HealthObservation{DiskStatus: "unknown", AptStatus: "unknown"}
	ApplyHealthResults(&health, results, HealthResultInterpreter{})
	record.DiskStatus, record.DiskFreeKB, record.DiskTotalKB = health.DiskStatus, health.DiskFreeKB, health.DiskTotalKB
	record.AptStatus, record.RebootRequired = health.AptStatus, health.RebootRequired
	return r.saveHealthSnapshot(record)
}

func healthMetaInt(meta map[string]any, keys ...string) int {
	for _, key := range keys {
		switch value := meta[key].(type) {
		case float64:
			if value >= 0 {
				return int(value)
			}
		case int:
			if value >= 0 {
				return value
			}
		case string:
			if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && parsed >= 0 {
				return parsed
			}
		}
	}
	return 0
}

func healthMetaString(meta map[string]any, key string) string {
	value, ok := meta[key]
	if !ok || value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(fmt.Sprintf("%v", value))
}

func healthMetaMap(meta map[string]any, key string) map[string]any {
	value, ok := meta[key]
	if !ok || value == nil {
		return nil
	}
	if result, ok := value.(map[string]any); ok {
		return result
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var result map[string]any
	if json.Unmarshal(data, &result) != nil {
		return nil
	}
	return result
}

func healthResultsFromMeta(meta map[string]any, key string) []PrecheckResult {
	value, ok := meta[key]
	if !ok || value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var results []PrecheckResult
	if json.Unmarshal(data, &results) != nil {
		return nil
	}
	return results
}

func diskFreeKBFromHealthOutput(output string) (int64, bool) {
	if freeKB, _, ok := diskFreeTotalKBFromHealthOutput(output); ok {
		return freeKB, true
	}
	var minimum int64
	found := false
	for _, field := range strings.Fields(output) {
		value, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err == nil && (!found || value < minimum) {
			minimum, found = value, true
		}
	}
	return minimum, found
}

func diskFreeTotalKBFromHealthOutput(output string) (int64, int64, bool) {
	var minimum, total int64
	found := false
	for _, line := range strings.Split(output, "\n") {
		values := []int64{}
		for _, field := range strings.Fields(line) {
			value, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
			if err == nil && value >= 0 {
				values = append(values, value)
			}
		}
		if len(values) >= 2 && (!found || values[1] < minimum) {
			total, minimum, found = values[0], values[1], true
		}
	}
	return minimum, total, found
}

func rebootRequiredFromHealthResult(result PrecheckResult) (bool, bool) {
	if strings.TrimSpace(result.Error) != "" {
		return false, false
	}
	if result.Passed {
		return false, true
	}
	text := strings.ToLower(result.Details + " " + result.Output)
	if rebootCheckErrorPattern.MatchString(text) {
		return false, false
	}
	if rebootRequiredPhrasePattern.MatchString(text) {
		return true, true
	}
	return false, true
}
