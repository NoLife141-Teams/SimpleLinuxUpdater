package updates

import (
	"errors"
	"fmt"
	"math"
	mathrand "math/rand"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"debian-updater/internal/servers"

	"golang.org/x/crypto/ssh"
)

var (
	cveRegex                = regexp.MustCompile(`CVE-[0-9]{4}-[0-9]+`)
	securitySuiteTokenRegex = regexp.MustCompile(`(?:^|[\s/:])[a-z0-9][a-z0-9+.-]*-security(?:$|[\s/\],:\)])`)
)

func UpdateCompletionOutcome(finalStatus string) string {
	switch finalStatus {
	case "done":
		return "success"
	case "idle":
		return "ignored"
	default:
		return "failure"
	}
}

func DoneOnlyOutcome(finalStatus string) string {
	if finalStatus == "done" {
		return "success"
	}
	return "failure"
}

func IsRetryableMessage(msg string) bool {
	normalized := strings.ToLower(strings.TrimSpace(msg))
	if normalized == "" {
		return false
	}
	nonRetryableHints := []string{
		"unable to authenticate",
		"permission denied",
		"no auth",
		"authentication",
		"host key",
		"knownhosts",
		"missing password or ssh key",
		"fingerprint mismatch",
		"invalid credentials",
		"invalid key",
		"invalid private key",
	}
	for _, hint := range nonRetryableHints {
		if strings.Contains(normalized, hint) {
			return false
		}
	}
	retryableHints := []string{
		"i/o timeout",
		"timeout",
		"timed out",
		"connection reset",
		"connection refused",
		"broken pipe",
		"eof",
		"temporarily unavailable",
		"resource temporarily unavailable",
		"could not get lock",
		"dpkg frontend lock",
		"network is unreachable",
		"no route to host",
		"connection closed",
	}
	for _, hint := range retryableHints {
		if strings.Contains(normalized, hint) {
			return true
		}
	}
	return false
}

func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	var tagged interface{ Retryable() bool }
	if errors.As(err, &tagged) && tagged.Retryable() {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return IsRetryableMessage(err.Error())
}

func MarkRetryableFromOutput(err error, output string) error {
	if err == nil {
		return nil
	}
	if IsRetryableMessage(output) {
		return RetryableTaggedError{Err: err}
	}
	return err
}

func ComputeRetryDelay(policy RetryPolicy, failedAttempt int, jitterRand float64) time.Duration {
	if failedAttempt < 1 {
		failedAttempt = 1
	}
	delay := float64(policy.BaseDelay) * math.Pow(2, float64(failedAttempt-1))
	maxDelay := float64(policy.MaxDelay)
	if delay > maxDelay {
		delay = maxDelay
	}
	if policy.JitterPct > 0 {
		jitterFactor := (jitterRand*2 - 1) * (float64(policy.JitterPct) / 100.0)
		delay = delay * (1 + jitterFactor)
	}
	if delay > maxDelay {
		delay = maxDelay
	}
	if delay < float64(time.Millisecond) {
		delay = float64(time.Millisecond)
	}
	return time.Duration(delay)
}

func RunWithRetryWithSleep(policy RetryPolicy, opName string, fn func() error, onRetry func(attempt int, wait time.Duration, err error), sleepFn func(time.Duration), logf func(string, ...any)) error {
	if policy.MaxAttempts < 1 {
		policy.MaxAttempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if !IsRetryableError(lastErr) {
			return lastErr
		}
		if attempt == policy.MaxAttempts {
			break
		}
		wait := ComputeRetryDelay(policy, attempt, mathrand.Float64())
		if onRetry != nil {
			onRetry(attempt, wait, lastErr)
		}
		if sleepFn != nil {
			sleepFn(wait)
		}
	}
	if lastErr != nil && IsRetryableError(lastErr) && logf != nil {
		logf("Retry exhausted for %s after %d attempts: %v", opName, policy.MaxAttempts, lastErr)
	}
	return lastErr
}

func RunWithRetry(policy RetryPolicy, opName string, fn func() error, onRetry func(attempt int, wait time.Duration, err error), logf func(string, ...any)) error {
	return RunWithRetryWithSleep(policy, opName, fn, onRetry, time.Sleep, logf)
}

func ParseUpgradableEntries(stdout string) ([]servers.PendingUpdate, []string, error) {
	lines := strings.Split(stdout, "\n")
	pendingUpdates := make([]servers.PendingUpdate, 0)
	upgradable := make([]string, 0)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "Inst ") {
			continue
		}
		entry := strings.TrimSpace(strings.TrimPrefix(trimmed, "Inst "))
		if entry == "" {
			continue
		}
		upgradable = append(upgradable, entry)
		pendingUpdates = append(pendingUpdates, ParsePendingUpdateEntry(entry))
	}
	return pendingUpdates, upgradable, nil
}

func ParsePendingUpdateEntry(entry string) servers.PendingUpdate {
	parsed := servers.PendingUpdate{
		Raw:      entry,
		CVEs:     []string{},
		CVEState: "",
	}
	fields := strings.Fields(entry)
	if len(fields) == 0 {
		return parsed
	}
	parsed.Package = fields[0]
	if len(fields) > 1 && strings.HasPrefix(fields[1], "[") && strings.HasSuffix(fields[1], "]") {
		parsed.CurrentVersion = strings.Trim(fields[1], "[]")
	}
	openParen := strings.Index(entry, "(")
	closeParen := strings.LastIndex(entry, ")")
	if openParen >= 0 && closeParen > openParen+1 {
		inside := strings.TrimSpace(entry[openParen+1 : closeParen])
		insideParts := strings.Fields(inside)
		if len(insideParts) > 0 {
			parsed.CandidateVersion = insideParts[0]
		}
		if len(insideParts) > 1 {
			parsed.Source = strings.Join(insideParts[1:], " ")
		}
	}
	parsed.Security = IsSecurityUpdate(parsed.Raw, parsed.Source)
	return parsed
}

func IsSecurityUpdate(raw, source string) bool {
	combined := strings.ToLower(strings.TrimSpace(raw + " " + source))
	if combined == "" {
		return false
	}
	securityMarkers := []string{
		"security.debian.org",
		"debian-security",
		"/security",
		"esm-apps",
		"esm-infra",
		"ubuntu-security",
	}
	for _, marker := range securityMarkers {
		if strings.Contains(combined, marker) {
			return true
		}
	}
	sourceOnly := strings.ToLower(strings.TrimSpace(source))
	if sourceOnly == "" {
		sourceOnly = combined
	}
	return securitySuiteTokenRegex.MatchString(sourceOnly)
}

func SortPendingUpdates(updates []servers.PendingUpdate) {
	sort.Slice(updates, func(i, j int) bool {
		if updates[i].Security != updates[j].Security {
			return updates[i].Security && !updates[j].Security
		}
		if len(updates[i].CVEs) != len(updates[j].CVEs) {
			return len(updates[i].CVEs) > len(updates[j].CVEs)
		}
		return updates[i].Package < updates[j].Package
	})
}

func NormalizeApprovalScope(scope string) string {
	normalized := strings.ToLower(strings.TrimSpace(scope))
	if normalized == "security" {
		return "security"
	}
	return "all"
}

func SecurityPackagesFromPendingUpdates(updates []servers.PendingUpdate) []string {
	return packageNamesFromPendingUpdates(updates, true)
}

func PackageNamesFromPendingUpdates(updates []servers.PendingUpdate) []string {
	return packageNamesFromPendingUpdates(updates, false)
}

func packageNamesFromPendingUpdates(updates []servers.PendingUpdate, securityOnly bool) []string {
	if len(updates) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(updates))
	packages := make([]string, 0, len(updates))
	for _, update := range updates {
		if securityOnly && !update.Security {
			continue
		}
		pkg := strings.TrimSpace(update.Package)
		if pkg == "" {
			continue
		}
		if _, exists := seen[pkg]; exists {
			continue
		}
		seen[pkg] = struct{}{}
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	return packages
}

func ShellEscapeSingleQuotes(value string) string {
	return strings.ReplaceAll(value, "'", "'\"'\"'")
}

func BuildSelectedUpgradeCmd(packages []string) string {
	if len(packages) == 0 {
		return ""
	}
	escaped := make([]string, 0, len(packages))
	for _, pkg := range packages {
		trimmed := strings.TrimSpace(pkg)
		if trimmed == "" {
			continue
		}
		escaped = append(escaped, fmt.Sprintf("'%s'", ShellEscapeSingleQuotes(trimmed)))
	}
	if len(escaped) == 0 {
		return ""
	}
	return AptUpgradeSelectedPrefixCmd + " " + strings.Join(escaped, " ")
}

func PreparePendingUpdatesForCVE(updates []servers.PendingUpdate) []servers.PendingUpdate {
	prepared := servers.ClonePendingUpdates(updates)
	SortPendingUpdates(prepared)
	for i := range prepared {
		if prepared[i].CVEs == nil {
			prepared[i].CVEs = []string{}
		}
		if i < CVELookupMaxPackages && strings.TrimSpace(prepared[i].Package) != "" {
			prepared[i].CVEState = "pending"
		} else {
			prepared[i].CVEState = "skipped"
		}
	}
	return prepared
}

func PendingCVEPackages(updates []servers.PendingUpdate) []string {
	pkgs := make([]string, 0)
	for _, update := range updates {
		if update.CVEState != "pending" {
			continue
		}
		pkg := strings.TrimSpace(update.Package)
		if pkg == "" {
			continue
		}
		pkgs = append(pkgs, pkg)
	}
	return pkgs
}

func ExtractCVEsFromText(text string, max int) []string {
	matches := cveRegex.FindAllString(strings.ToUpper(text), -1)
	if len(matches) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		if _, exists := seen[match]; exists {
			continue
		}
		seen[match] = struct{}{}
		out = append(out, match)
	}
	sort.Strings(out)
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

func BuildPackageCVEQueryCmd(pkg string) string {
	escapedPkg := fmt.Sprintf("'%s'", ShellEscapeSingleQuotes(strings.TrimSpace(pkg)))
	innerCmd := fmt.Sprintf(
		"apt-get changelog %s 2>/dev/null | grep -Eo 'CVE-[0-9]{4}-[0-9]+' | sort -u | head -n %d",
		escapedPkg,
		CVELookupMaxPerPackage,
	)
	return fmt.Sprintf("sh -c '%s'", ShellEscapeSingleQuotes(innerCmd))
}

func SSHExitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var exitStatusErr interface{ ExitStatus() int }
	if errors.As(err, &exitStatusErr) {
		return exitStatusErr.ExitStatus(), true
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitStatus(), true
	}
	return 0, false
}
