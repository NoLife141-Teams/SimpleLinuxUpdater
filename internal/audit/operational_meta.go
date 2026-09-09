package audit

import "encoding/json"

// Keep machine-consumed facts outside the human-readable truncation preview.
// Values come from the already-redacted map and share the same total byte cap.
func retainOperationalMeta(dst, src map[string]any) {
	keys := []string{
		"pending_package_count", "approved_package_count", "security_package_count", "approval_scope",
		"execution_duration_ms", "duration_ms", "total_elapsed_ms", "status", "last_error_class",
		"prechecks_passed", "precheck_failed", "postchecks_enabled", "postchecks_passed", "postcheck_failed",
		"upgrade_completed", "interrupted", "retry_exhausted",
	}
	for _, key := range keys {
		value, ok := src[key]
		if !ok {
			continue
		}
		switch v := value.(type) {
		case string:
			if len(v) > 128 {
				continue
			}
		case bool, json.Number, nil:
		default:
			continue
		}
		dst[key] = value
	}
	if discovery, ok := src["discovery"].(map[string]any); ok {
		counts := map[string]any{}
		for _, key := range []string{"pending_package_count", "security_package_count"} {
			if n, ok := discovery[key].(json.Number); ok {
				counts[key] = n
			}
		}
		if len(counts) > 0 {
			dst["discovery"] = counts
		}
	}

	// Reserve compact status records for every supported dimension before adding
	// optional text, so a long precheck output cannot crowd out a failed postcheck.
	type retainedCheck struct{ compact, original map[string]any }
	checks := make([]retainedCheck, 0)
	for _, key := range []string{"postcheck_results", "precheck_results"} {
		values, ok := src[key].([]any)
		if !ok {
			continue
		}
		results := make([]any, 0)
		byName := map[string]map[string]any{}
		for _, value := range values {
			result, ok := value.(map[string]any)
			if !ok {
				continue
			}
			name, _ := result["name"].(string)
			switch name {
			case "disk_space", "apt_health", "post_apt_health", "reboot_required":
			default:
				continue
			}
			byName[name] = result
		}
		for _, name := range []string{"disk_space", "apt_health", "post_apt_health", "reboot_required"} {
			result, ok := byName[name]
			if !ok {
				continue
			}
			passed, _ := result["passed"].(bool)
			compact := map[string]any{"name": name, "passed": passed}
			if text, ok := result["error"].(string); ok && text != "" {
				compact["error"] = "..."
			}
			results = append(results, compact)
			checks = append(checks, retainedCheck{compact, result})
		}
		dst[key] = results
	}
	for _, check := range checks {
		for _, field := range []string{"error", "output", "details"} {
			text, ok := check.original[field].(string)
			if !ok {
				continue
			}
			runes := []rune(text)
			if len(runes) > 128 {
				text = string(runes[:128]) + "..."
			}
			previous, hadPrevious := check.compact[field]
			check.compact[field] = text
			encoded, err := json.Marshal(dst)
			if err != nil || len(encoded) > MetaMaxLen-256 {
				if hadPrevious {
					check.compact[field] = previous
				} else {
					delete(check.compact, field)
				}
			}
		}
	}
}
