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
	securitySuiteTokenRegex = regexp.MustCompile(`(?:^|[\s/,:])[a-z0-9][a-z0-9+.-]*-security(?:$|[\s/\],:\)])`)
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

func ParseFailedSystemdUnits(output string) []string {
	lines := strings.Split(output, "\n")
	units := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		unit := strings.TrimSpace(fields[0])
		if unit == "" {
			continue
		}
		if _, exists := seen[unit]; exists {
			continue
		}
		seen[unit] = struct{}{}
		units = append(units, unit)
	}
	return units
}

func SummarizeUnitNames(units []string, maxShown int) string {
	if len(units) == 0 {
		return ""
	}
	if maxShown <= 0 || maxShown >= len(units) {
		return strings.Join(units, ", ")
	}
	remaining := len(units) - maxShown
	return fmt.Sprintf("%s (+%d more)", strings.Join(units[:maxShown], ", "), remaining)
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
	if len(upgradable) > 0 {
		return pendingUpdates, upgradable, nil
	}
	for _, pkg := range parseAptUpgradeSummaryPackages(lines) {
		upgradable = append(upgradable, pkg)
		pendingUpdates = append(pendingUpdates, ParsePendingUpdateEntry(pkg))
	}
	return pendingUpdates, upgradable, nil
}

func NeedsAptListMetadata(pendingUpdates []servers.PendingUpdate) bool {
	if len(pendingUpdates) == 0 {
		return false
	}
	for _, update := range pendingUpdates {
		if strings.TrimSpace(update.Source) != "" || strings.TrimSpace(update.CandidateVersion) != "" || strings.TrimSpace(update.CurrentVersion) != "" {
			return false
		}
	}
	return true
}

func ParseAptListMetadataEntries(stdout string, packageNames []string) ([]servers.PendingUpdate, []string) {
	requested := packageSelectorsFromEntries(packageNames)
	allowedExact := make(map[string]struct{}, len(requested))
	allowedBase := make(map[string]struct{}, len(requested))
	for _, selector := range requested {
		if selector.arch == "" {
			allowedBase[selector.base] = struct{}{}
			continue
		}
		allowedExact[selector.selector] = struct{}{}
	}
	parsedBySelector := make(map[string]servers.PendingUpdate)
	rawBySelector := make(map[string]string)
	fallbackOrder := make([]packageSelector, 0)
	for _, line := range strings.Split(stdout, "\n") {
		update, ok := ParseAptListMetadataEntry(line)
		if !ok {
			continue
		}
		selector := aptListMetadataSelector(update)
		if selector.base == "" {
			continue
		}
		key := selector.selector
		if len(requested) > 0 {
			if _, exists := allowedExact[selector.selector]; exists {
				key = selector.selector
				update.Package = selector.selector
			} else if _, exists := allowedBase[selector.base]; exists {
				key = selector.base
			} else {
				continue
			}
		}
		if _, exists := parsedBySelector[key]; exists {
			continue
		}
		fallbackOrder = append(fallbackOrder, packageSelector{selector: key, base: selector.base, arch: selector.arch})
		parsedBySelector[key] = update
		rawBySelector[key] = update.Raw
	}
	order := requested
	if len(order) == 0 {
		order = fallbackOrder
	}
	pendingUpdates := make([]servers.PendingUpdate, 0, len(order))
	upgradable := make([]string, 0, len(order))
	for _, selector := range order {
		key := selector.selector
		update, exists := parsedBySelector[key]
		if !exists {
			continue
		}
		pendingUpdates = append(pendingUpdates, update)
		upgradable = append(upgradable, rawBySelector[key])
	}
	return pendingUpdates, upgradable
}

func MergePendingUpdatesWithMetadata(summaryPending []servers.PendingUpdate, metadataPending []servers.PendingUpdate) ([]servers.PendingUpdate, []string) {
	if len(summaryPending) == 0 {
		return nil, nil
	}
	metadataBySelector := make(map[string]servers.PendingUpdate, len(metadataPending))
	metadataByBase := make(map[string]servers.PendingUpdate, len(metadataPending))
	for _, update := range metadataPending {
		selector := packageSelectorFromPackage(update.Package)
		if selector.base == "" {
			continue
		}
		metadataBySelector[selector.selector] = update
		if _, exists := metadataByBase[selector.base]; !exists {
			metadataByBase[selector.base] = update
		}
	}
	mergedPending := make([]servers.PendingUpdate, 0, len(summaryPending))
	mergedUpgradable := make([]string, 0, len(summaryPending))
	for _, update := range summaryPending {
		selector := packageSelectorFromPackage(update.Package)
		metadata, ok := metadataBySelector[selector.selector]
		if !ok && selector.arch == "" {
			metadata, ok = metadataByBase[selector.base]
		}
		if ok {
			metadata.Package = update.Package
			mergedPending = append(mergedPending, metadata)
			mergedUpgradable = append(mergedUpgradable, metadata.Raw)
			continue
		}
		if strings.TrimSpace(update.Raw) == "" {
			update.Raw = selector.selector
		}
		mergedPending = append(mergedPending, update)
		mergedUpgradable = append(mergedUpgradable, update.Raw)
	}
	return mergedPending, mergedUpgradable
}

func MergeAvailableUpdatesWithStandard(standardPending []servers.PendingUpdate, standardUpgradable []string, availablePending []servers.PendingUpdate, availableUpgradable []string, fullUpgradeStdout string, fullUpgradePlanAvailable bool) ([]servers.PendingUpdate, []string, servers.UpgradePlan) {
	if len(availablePending) == 0 {
		pending := servers.ClonePendingUpdates(standardPending)
		upgradable := append([]string(nil), standardUpgradable...)
		plan := BuildUpgradePlan(pending, fullUpgradeStdout, fullUpgradePlanAvailable)
		return pending, upgradable, plan
	}

	type availableRecord struct {
		update   servers.PendingUpdate
		raw      string
		selector packageSelector
	}
	records := make([]availableRecord, 0, len(availablePending))
	bySelector := make(map[string]int, len(availablePending))
	byBase := make(map[string]int, len(availablePending))
	for i, update := range availablePending {
		selector := aptListMetadataSelector(update)
		if selector.selector == "" {
			continue
		}
		raw := update.Raw
		if i < len(availableUpgradable) && strings.TrimSpace(availableUpgradable[i]) != "" {
			raw = availableUpgradable[i]
		}
		idx := len(records)
		records = append(records, availableRecord{update: update, raw: raw, selector: selector})
		if _, exists := bySelector[selector.selector]; !exists {
			bySelector[selector.selector] = idx
		}
		if _, exists := byBase[selector.base]; !exists {
			byBase[selector.base] = idx
		}
	}

	seen := make(map[string]struct{}, len(records)+len(standardPending))
	pending := make([]servers.PendingUpdate, 0, len(availablePending)+len(standardPending))
	upgradable := make([]string, 0, len(availableUpgradable)+len(standardUpgradable))

	appendUpdate := func(update servers.PendingUpdate, raw string, selector packageSelector, requiresFull bool) {
		update.KeptBack = requiresFull
		update.RequiresFull = requiresFull
		if requiresFull && selector.arch != "" && selector.arch != "all" {
			update.InstallPackage = selector.selector
		}
		pending = append(pending, update)
		if strings.TrimSpace(raw) != "" {
			upgradable = append(upgradable, raw)
		} else if strings.TrimSpace(update.Raw) != "" {
			upgradable = append(upgradable, update.Raw)
		} else {
			upgradable = append(upgradable, update.Package)
		}
		if selector.selector != "" {
			seen[selector.selector] = struct{}{}
		}
		if selector.arch == "" && selector.base != "" {
			seen[selector.base] = struct{}{}
		}
	}

	for i, update := range standardPending {
		selector := packageSelectorFromPackage(update.Package)
		if selector.selector == "" {
			continue
		}
		raw := ""
		if i < len(standardUpgradable) {
			raw = standardUpgradable[i]
		}
		if idx, exists := bySelector[selector.selector]; exists {
			record := records[idx]
			enriched := record.update
			if selector.arch != "" {
				enriched.Package = selector.selector
			} else {
				enriched.Package = selector.base
			}
			appendUpdate(enriched, record.raw, record.selector, false)
			if selector.arch == "" && record.selector.selector != "" {
				seen[record.selector.selector] = struct{}{}
			}
			continue
		}
		if selector.arch == "" {
			if idx, exists := byBase[selector.base]; exists {
				record := records[idx]
				enriched := record.update
				enriched.Package = selector.base
				appendUpdate(enriched, record.raw, packageSelector{selector: selector.base, base: selector.base}, false)
				if record.selector.selector != "" {
					seen[record.selector.selector] = struct{}{}
				}
				continue
			}
		}
		appendUpdate(update, raw, selector, false)
	}

	for _, record := range records {
		selector := record.selector
		if _, exists := seen[selector.selector]; exists {
			continue
		}
		if _, exists := seen[selector.base]; exists && selector.arch != "" {
			continue
		}
		update := record.update
		if selector.arch != "" {
			update.Package = selector.base
		}
		appendUpdate(update, record.raw, selector, true)
	}

	plan := BuildUpgradePlan(pending, fullUpgradeStdout, fullUpgradePlanAvailable)
	return pending, upgradable, plan
}

func BuildUpgradePlan(pending []servers.PendingUpdate, fullUpgradeStdout string, fullUpgradePlanAvailable bool) servers.UpgradePlan {
	plan := servers.UpgradePlan{
		FullUpgradePlanAvailable:   fullUpgradePlanAvailable,
		FullUpgradeNewPackages:     parseAptSummaryPackages(fullUpgradeStdout, "the following new packages will be installed:"),
		FullUpgradeRemovedPackages: parseAptSummaryPackages(fullUpgradeStdout, "the following packages will be removed:"),
	}
	plan.KeptBackSecurityPackageCount = len(KeptBackSecurityPackagesFromPendingUpdates(pending))
	fullUpgradePackages := parseAptUpgradeSummaryPackages(strings.Split(fullUpgradeStdout, "\n"))
	plan.FullUpgradePackageCount = len(fullUpgradePackages)
	if plan.FullUpgradePackageCount == 0 {
		plan.FullUpgradePackageCount = len(pending)
	}
	for _, update := range pending {
		if update.RequiresFull || update.KeptBack {
			plan.KeptBackPackageCount++
		} else {
			plan.StandardPackageCount++
			if update.Security {
				plan.StandardSecurityCount++
			}
		}
		if update.Security {
			plan.TotalSecurityCount++
		}
	}
	return plan
}

func ApplyKeptBackSecuritySimulation(plan *servers.UpgradePlan, stdout string) {
	if plan == nil {
		return
	}
	plan.KeptBackSecurityPlanAvailable = true
	plan.KeptBackSecurityNewPackages = parseAptSummaryPackages(stdout, "the following new packages will be installed:")
	plan.KeptBackSecurityRemovedPackages = parseAptSummaryPackages(stdout, "the following packages will be removed:")
}

func ParseAptListMetadataEntry(line string) (servers.PendingUpdate, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.EqualFold(trimmed, "Listing...") {
		return servers.PendingUpdate{}, false
	}
	fields := strings.Fields(trimmed)
	if len(fields) < 3 {
		return servers.PendingUpdate{}, false
	}
	nameAndSource := fields[0]
	slash := strings.Index(nameAndSource, "/")
	if slash <= 0 {
		return servers.PendingUpdate{}, false
	}
	pkg := strings.TrimSpace(nameAndSource[:slash])
	pkg = normalizePackageName(pkg)
	source := strings.TrimSpace(nameAndSource[slash+1:])
	if pkg == "" {
		return servers.PendingUpdate{}, false
	}
	currentVersion := ""
	if open := strings.Index(trimmed, "[upgradable from:"); open >= 0 {
		start := open + len("[upgradable from:")
		if close := strings.Index(trimmed[start:], "]"); close >= 0 {
			currentVersion = strings.TrimSpace(trimmed[start : start+close])
		}
	}
	update := servers.PendingUpdate{
		Package:          pkg,
		CurrentVersion:   currentVersion,
		CandidateVersion: fields[1],
		Source:           source,
		Raw:              trimmed,
		CVEs:             []string{},
	}
	update.Security = IsSecurityUpdate(update.Raw, update.Source)
	return update, true
}

type packageSelector struct {
	selector string
	base     string
	arch     string
}

func packageSelectorsFromEntries(entries []string) []packageSelector {
	seen := make(map[string]struct{}, len(entries))
	selectors := make([]packageSelector, 0, len(entries))
	for _, entry := range entries {
		selector := packageSelectorFromPackage(entry)
		if selector.base == "" {
			continue
		}
		if _, exists := seen[selector.selector]; exists {
			continue
		}
		seen[selector.selector] = struct{}{}
		selectors = append(selectors, selector)
	}
	return selectors
}

func aptListMetadataSelector(update servers.PendingUpdate) packageSelector {
	selector := packageSelectorFromPackage(update.Package)
	if selector.base == "" || selector.arch != "" {
		return selector
	}
	arch := aptListMetadataArch(update.Raw)
	if arch == "" {
		return selector
	}
	selector.arch = arch
	selector.selector = selector.base + ":" + arch
	return selector
}

func aptListMetadataArch(raw string) string {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) < 3 {
		return ""
	}
	arch := strings.TrimSpace(fields[2])
	if arch == "" || strings.ContainsAny(arch, "/[]") {
		return ""
	}
	return arch
}

func packageSelectorFromPackage(entry string) packageSelector {
	fields := strings.Fields(strings.TrimSpace(entry))
	if len(fields) == 0 {
		return packageSelector{}
	}
	pkg := fields[0]
	if slash := strings.Index(pkg, "/"); slash > 0 {
		pkg = pkg[:slash]
	}
	pkg = strings.TrimSpace(pkg)
	if pkg == "" {
		return packageSelector{}
	}
	selector := packageSelector{selector: pkg, base: pkg}
	if colon := strings.Index(pkg, ":"); colon > 0 {
		selector.base = strings.TrimSpace(pkg[:colon])
		selector.arch = strings.TrimSpace(pkg[colon+1:])
		if selector.base == "" || selector.arch == "" {
			return packageSelector{}
		}
		selector.selector = selector.base + ":" + selector.arch
	}
	return selector
}

func normalizePackageName(pkg string) string {
	trimmed := strings.TrimSpace(pkg)
	if trimmed == "" {
		return ""
	}
	if colon := strings.Index(trimmed, ":"); colon > 0 {
		return strings.TrimSpace(trimmed[:colon])
	}
	return trimmed
}

func parseAptUpgradeSummaryPackages(lines []string) []string {
	return parseAptSummaryPackages(strings.Join(lines, "\n"), "the following packages will be upgraded:")
}

func parseAptSummaryPackages(stdout, header string) []string {
	lines := strings.Split(stdout, "\n")
	packages := make([]string, 0)
	inUpgradeBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if lower == header {
			inUpgradeBlock = true
			continue
		}
		if !inUpgradeBlock {
			continue
		}
		if trimmed == "" {
			if len(packages) > 0 {
				break
			}
			continue
		}
		if len(packages) > 0 && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			break
		}
		if strings.HasPrefix(lower, "the following ") || strings.Contains(lower, " upgraded,") || strings.HasPrefix(lower, "need to get ") || strings.HasPrefix(lower, "after this operation") {
			break
		}
		packages = append(packages, strings.Fields(trimmed)...)
	}
	return packages
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
		if updates[i].RequiresFull != updates[j].RequiresFull {
			return !updates[i].RequiresFull && updates[j].RequiresFull
		}
		if len(updates[i].CVEs) != len(updates[j].CVEs) {
			return len(updates[i].CVEs) > len(updates[j].CVEs)
		}
		return updates[i].Package < updates[j].Package
	})
}

func NormalizeApprovalScope(scope string) string {
	normalized := strings.ToLower(strings.TrimSpace(scope))
	if normalized == "security" || normalized == "security_kept_back" || normalized == "full_upgrade" {
		return normalized
	}
	return "all"
}

func SecurityPackagesFromPendingUpdates(updates []servers.PendingUpdate) []string {
	return packageNamesFromPendingUpdates(updates, true)
}

func KeptBackSecurityPackagesFromPendingUpdates(updates []servers.PendingUpdate) []string {
	if len(updates) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(updates))
	packages := make([]string, 0, len(updates))
	for _, update := range updates {
		if !update.Security || !(update.RequiresFull || update.KeptBack) {
			continue
		}
		pkg := strings.TrimSpace(update.Package)
		if installPackage := strings.TrimSpace(update.InstallPackage); installPackage != "" {
			pkg = installPackage
		}
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

func PackageNamesFromPendingUpdates(updates []servers.PendingUpdate) []string {
	return packageNamesFromPendingUpdates(updates, false)
}

func FullUpgradePackagesFromPendingUpdates(updates []servers.PendingUpdate) []string {
	return packageNamesFromPendingUpdates(updates, false, true)
}

func packageNamesFromPendingUpdates(updates []servers.PendingUpdate, securityOnly bool, includeRequiresFullOpt ...bool) []string {
	if len(updates) == 0 {
		return nil
	}
	includeRequiresFull := false
	if len(includeRequiresFullOpt) > 0 {
		includeRequiresFull = includeRequiresFullOpt[0]
	}
	seen := make(map[string]struct{}, len(updates))
	packages := make([]string, 0, len(updates))
	for _, update := range updates {
		if securityOnly && !update.Security {
			continue
		}
		if !includeRequiresFull && (update.RequiresFull || update.KeptBack) {
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

func RootOrSudoCommand(command string) string {
	return fmt.Sprintf("if [ \"$(id -u)\" -eq 0 ]; then %s; else sudo -n %s; fi", command, command)
}

func BuildSelectedUpgradeCmd(packages []string) string {
	return buildSelectedInstallCmd(packages, true)
}

func BuildSelectedInstallCmd(packages []string) string {
	return buildSelectedInstallCmd(packages, false)
}

func BuildSelectedInstallSimulationCmd(packages []string) string {
	escaped := shellEscapedPackageArgs(packages)
	if len(escaped) == 0 {
		return ""
	}
	return "LC_ALL=C apt-get -s install -- " + strings.Join(escaped, " ")
}

func buildSelectedInstallCmd(packages []string, onlyUpgrade bool) string {
	args := buildSelectedInstallArgs(packages, onlyUpgrade)
	if args == "" {
		return ""
	}
	return RootOrSudoCommand(args)
}

func buildSelectedInstallArgs(packages []string, onlyUpgrade bool) string {
	escaped := shellEscapedPackageArgs(packages)
	if len(escaped) == 0 {
		return ""
	}
	args := "apt-get -y install"
	if onlyUpgrade {
		args += " --only-upgrade"
	}
	return args + " -- " + strings.Join(escaped, " ")
}

func shellEscapedPackageArgs(packages []string) []string {
	if len(packages) == 0 {
		return nil
	}
	escaped := make([]string, 0, len(packages))
	for _, pkg := range packages {
		trimmed := strings.TrimSpace(pkg)
		if trimmed == "" {
			continue
		}
		escaped = append(escaped, fmt.Sprintf("'%s'", ShellEscapeSingleQuotes(trimmed)))
	}
	return escaped
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
