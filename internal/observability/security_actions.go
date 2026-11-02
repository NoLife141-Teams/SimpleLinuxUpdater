package observability

import (
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"debian-updater/internal/securityintel"
	"debian-updater/internal/vulnerabilities"
)

type SecurityActionQuery struct {
	Server, Search             string
	Page, PageSize, RecentDays int
}

func ParseSecurityActionQuery(v url.Values) (SecurityActionQuery, error) {
	q := SecurityActionQuery{Server: strings.TrimSpace(v.Get("server")), Search: strings.TrimSpace(v.Get("q")), Page: 1, PageSize: 25, RecentDays: 30}
	if len(q.Server) > 200 || len(q.Search) > 200 {
		return q, strconv.ErrSyntax
	}
	for _, p := range []struct {
		name     string
		target   *int
		min, max int
	}{{"page", &q.Page, 1, 1000000}, {"page_size", &q.PageSize, 1, 100}, {"recent_days", &q.RecentDays, 1, 90}} {
		if raw := v.Get(p.name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < p.min || n > p.max {
				return q, strconv.ErrSyntax
			}
			*p.target = n
		}
	}
	return q, nil
}

type SecurityAction struct {
	CVECount              int      `json:"cve_count"`
	Server                string   `json:"server"`
	Package               string   `json:"package"`
	Installed             string   `json:"installed"`
	Candidate             string   `json:"candidate,omitempty"`
	CVEs                  []string `json:"cves"`
	Severity              string   `json:"severity"`
	PublishedAt           string   `json:"published_at,omitempty"`
	KnownExploited        bool     `json:"known_exploited"`
	ExploitationFreshness string   `json:"exploitation_freshness,omitempty"`
	ExploitationSource    string   `json:"exploitation_source,omitempty"`
	RequiredAction        string   `json:"required_action,omitempty"`
	FixState              string   `json:"fix_state"`
	Evidence              string   `json:"evidence"`
	Source                string   `json:"source"`
	ObservedAt            string   `json:"observed_at"`
	Scopes                []string `json:"scopes"`
}
type LifecycleStatus struct {
	Server             string `json:"server"`
	Component          string `json:"component"`
	Kind               string `json:"kind"`
	Version            string `json:"version"`
	Product            string `json:"product"`
	Cycle              string `json:"cycle,omitempty"`
	State              string `json:"state"`
	EOLAt              string `json:"eol_at,omitempty"`
	ExtendedUntil      string `json:"extended_until,omitempty"`
	Source             string `json:"source,omitempty"`
	ObservedAt         string `json:"observed_at,omitempty"`
	Freshness          string `json:"freshness"`
	InventoryFreshness string `json:"inventory_freshness"`
	SupportContext     string `json:"support_context"`
}
type SecurityActions struct {
	GeneratedAt    string                        `json:"generated_at"`
	Servers        []string                      `json:"servers"`
	Fixes          []SecurityAction              `json:"fixes"`
	Alerts         []SecurityAction              `json:"alerts"`
	Lifecycle      []LifecycleStatus             `json:"lifecycle"`
	Feeds          map[string]securityintel.Feed `json:"feeds"`
	FixTotal       int                           `json:"fix_total"`
	AlertTotal     int                           `json:"alert_total"`
	LifecycleTotal int                           `json:"lifecycle_total"`
	ReviewFindings int                           `json:"review_findings"`
	ReviewPriority int                           `json:"review_priority"`
	Page           int                           `json:"page"`
	Pages          int                           `json:"pages"`
	RecentDays     int                           `json:"recent_days"`
}

func BuildSecurityActions(summary DashboardSummaryResponse, intel securityintel.Snapshot, q SecurityActionQuery, now time.Time) SecurityActions {
	if q.PageSize < 1 || q.PageSize > 100 {
		q.PageSize = 25
	}
	if q.Page < 1 {
		q.Page = 1
	}
	if q.RecentDays < 1 || q.RecentDays > 90 {
		q.RecentDays = 30
	}
	out := SecurityActions{GeneratedAt: now.UTC().Format(time.RFC3339), Servers: []string{}, Fixes: []SecurityAction{}, Alerts: []SecurityAction{}, Lifecycle: []LifecycleStatus{}, Feeds: intel.Feeds, RecentDays: q.RecentDays}
	fixes := map[string]*SecurityAction{}
	for _, host := range summary.Servers {
		out.Servers = append(out.Servers, host.Name)
		if q.Server != "" && q.Server != host.Name {
			continue
		}
		info := host.Risk.Vulnerability
		for _, f := range info.Findings {
			if f.ResolvedAt != "" || f.ID == "" || !actionSearch(q.Search, host.Name, f.Package, f.ID) {
				continue
			}
			if f.Qualification == vulnerabilities.QualificationQualified || f.Qualification == vulnerabilities.QualificationWithdrawn {
				continue
			}
			known, exploited := intel.Exploited[f.ID]
			row := SecurityAction{Server: host.Name, Package: f.Package, Installed: f.InstalledVersion, Candidate: f.CandidateVersion, CVEs: []string{f.ID}, Severity: f.Severity, PublishedAt: f.PublishedAt, KnownExploited: exploited, Source: f.AdvisoryURL, ObservedAt: f.LastSeen, Scopes: []string{}}
			if f.SeveritySource == "" || f.SeverityFreshness != "fresh" {
				row.Severity = "unknown"
			}
			if exploited {
				row.ExploitationFreshness = intel.Feeds["kev"].State
				row.ExploitationSource = "https://www.cisa.gov/known-exploited-vulnerabilities-catalog"
				row.RequiredAction = known.Action
			}
			fresh := f.Freshness == "fresh" && f.Qualification == vulnerabilities.QualificationConfirmed && f.AssessmentFreshness == "fresh"
			a := f.Availability
			if fresh && a != nil && f.AvailabilityFreshness == "fresh" && f.PublishedFixFreshness == "fresh" && a.CandidateState == "addresses" && a.Published.State == "published" && a.RepositoryState == "observed" && a.APT.LastRefreshState == "succeeded" && recentEvidence(a.APT.LastRefreshAt, now) && len(a.Restrictions) == 0 {
				for _, s := range a.Scopes {
					if s.State == "simulated" {
						row.Scopes = append(row.Scopes, s.Scope)
					}
				}
			}
			if len(row.Scopes) > 0 {
				row.FixState = "installable"
				row.Evidence = "fresh"
				key := host.Name + "\x00" + f.Package + "\x00" + f.InstalledVersion + "\x00" + f.CandidateVersion
				if old := fixes[key]; old != nil {
					old.CVEs = append(old.CVEs, f.ID)
					old.KnownExploited = old.KnownExploited || exploited
					if vulnerabilities.SeverityRank(row.Severity) > vulnerabilities.SeverityRank(old.Severity) {
						old.Severity = row.Severity
					}
				} else {
					fixes[key] = &row
				}
				continue
			}
			published, err := time.Parse(time.RFC3339, f.PublishedAt)
			recent := err == nil && !published.After(now) && now.Sub(published) <= time.Duration(q.RecentDays)*24*time.Hour
			watchedCritical := f.Severity == "critical" && f.SeveritySource != "" && validPriorityWatch(f.PrioritySince, now)
			priority := exploited || (f.Severity == "critical" && f.SeveritySource != "" && f.SeverityFreshness == "fresh" && (recent || watchedCritical))
			if fresh && priority && a != nil && f.PublishedFixFreshness == "fresh" && a.Published.State == "not_established" && len(a.Published.Ranges) > 0 {
				row.FixState = "no_published_fix_in_source"
				row.Evidence = "fresh"
				out.Alerts = append(out.Alerts, row)
			} else if priority && fresh && a != nil && f.PublishedFixFreshness == "fresh" && a.Published.State == "published" {
				// A published but blocked fix must never be described as unpatched.
				out.ReviewPriority++
				out.ReviewFindings++
			} else {
				out.ReviewFindings++
				// Expired severity/source evidence blocks a current unpatched
				// claim, but must not silently erase a previously tracked
				// critical incident from the priorities requiring review.
				if priority || watchedCritical {
					out.ReviewPriority++
				}
			}
		}
		osProduct, osVersion := osLifecycleIdentity(info.Distro, host.Health.OSPrettyName)
		osFreshness := info.Freshness
		factsProduct, factsVersion := osLifecycleIdentity("", host.Health.OSPrettyName)
		factsAt, factsErr := time.Parse(time.RFC3339, host.Health.CollectedAt)
		scanAt, scanErr := time.Parse(time.RFC3339, info.AcceptedAt)
		if factsProduct != "unknown" && factsErr == nil && !factsAt.After(now) && (scanErr != nil || !factsAt.Before(scanAt)) {
			osProduct, osVersion = factsProduct, factsVersion
			osFreshness = "stale"
			if recentEvidence(host.Health.CollectedAt, now) {
				osFreshness = "fresh"
			}
		}
		if actionSearch(q.Search, host.Name, osProduct, osVersion) {
			out.Lifecycle = append(out.Lifecycle, lifecycleFor(host.Name, osProduct, osProduct, osVersion, "os", osFreshness, intel, now))
		}
		seen := map[string]bool{}
		for _, p := range info.Inventory {
			product := appLifecycleProduct(p.Name)
			if product == "" || !actionSearch(q.Search, host.Name, p.Name, p.Version) {
				continue
			}
			key := product + "\x00" + p.Version
			if seen[key] {
				continue
			}
			seen[key] = true
			out.Lifecycle = append(out.Lifecycle, lifecycleFor(host.Name, p.Name, product, p.Version, "application", info.Freshness, intel, now))
		}
	}
	for _, f := range fixes {
		sort.Strings(f.CVEs)
		f.CVECount = len(f.CVEs)
		if len(f.CVEs) > 10 {
			f.CVEs = f.CVEs[:10]
		}
		out.Fixes = append(out.Fixes, *f)
	}
	lessAction := func(a, b SecurityAction) bool {
		if a.KnownExploited != b.KnownExploited {
			return a.KnownExploited
		}
		ar, br := vulnerabilities.SeverityRank(a.Severity), vulnerabilities.SeverityRank(b.Severity)
		if ar != br {
			return ar > br
		}
		if a.Server != b.Server {
			return a.Server < b.Server
		}
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		return a.CVEs[0] < b.CVEs[0]
	}
	sort.Slice(out.Fixes, func(i, j int) bool { return lessAction(out.Fixes[i], out.Fixes[j]) })
	sort.Slice(out.Alerts, func(i, j int) bool { return lessAction(out.Alerts[i], out.Alerts[j]) })
	sort.Slice(out.Lifecycle, func(i, j int) bool {
		a, b := out.Lifecycle[i], out.Lifecycle[j]
		rank := func(v LifecycleStatus) int {
			switch v.State {
			case "end_of_life", "upstream_end_of_life":
				return 0
			case "extended_support_unverified":
				return 1
			case "ending_soon":
				return 2
			case "unknown":
				return 3
			}
			return 4
		}
		if rank(a) != rank(b) {
			return rank(a) < rank(b)
		}
		if a.Server != b.Server {
			return a.Server < b.Server
		}
		if a.Component != b.Component {
			return a.Component < b.Component
		}
		return a.Version < b.Version
	})
	out.FixTotal, out.AlertTotal, out.LifecycleTotal = len(out.Fixes), len(out.Alerts), len(out.Lifecycle)
	maxRows := max(out.FixTotal, out.AlertTotal, out.LifecycleTotal)
	out.Pages = max(1, (maxRows+q.PageSize-1)/q.PageSize)
	out.Page = min(q.Page, out.Pages)
	out.Fixes = actionPage(out.Fixes, out.Page, q.PageSize)
	out.Alerts = actionPage(out.Alerts, out.Page, q.PageSize)
	out.Lifecycle = actionPage(out.Lifecycle, out.Page, q.PageSize)
	sort.Strings(out.Servers)
	return out
}
func actionPage[T any](values []T, page, size int) []T {
	start := min(len(values), (page-1)*size)
	return values[start:min(len(values), start+size)]
}
func validPriorityWatch(v string, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, v)
	return err == nil && !at.After(now)
}
func actionSearch(q string, values ...string) bool {
	return q == "" || strings.Contains(strings.ToLower(strings.Join(values, " ")), strings.ToLower(q))
}

var osIdentityRE = regexp.MustCompile(`(?i)\b(ubuntu|debian)\b[^0-9]*([0-9]+(?:\.[0-9]+)?)`)

func osLifecycleIdentity(distro, pretty string) (string, string) {
	for _, value := range []string{distro, pretty} {
		if m := osIdentityRE.FindStringSubmatch(value); len(m) == 3 {
			return strings.ToLower(m[1]), m[2]
		}
	}
	return "unknown", ""
}

var appNameRE = regexp.MustCompile(`^(python3(?:\.[0-9]+)?|php[0-9]+\.[0-9]+(?:-.+)?|postgresql-[0-9]+)$`)

func appLifecycleProduct(name string) string {
	name = strings.Split(name, ":")[0]
	switch name {
	case "nodejs":
		return "nodejs"
	case "redis-server":
		return "redis"
	case "nginx", "nginx-core", "nginx-full", "nginx-light":
		return "nginx"
	case "openssl", "openssh-server":
		if name == "openssl" {
			return "openssl"
		}
		return "openssh"
	case "docker-ce", "docker.io":
		return "docker-engine"
	case "containerd", "containerd.io":
		return "containerd"
	}
	if appNameRE.MatchString(name) {
		if strings.HasPrefix(name, "python") {
			return "python"
		}
		if strings.HasPrefix(name, "php") {
			return "php"
		}
		return "postgresql"
	}
	return ""
}

var versionStartRE = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)*`)

func lifecycleFor(server, component, product, version, kind, inventoryFreshness string, intel securityintel.Snapshot, now time.Time) LifecycleStatus {
	row := LifecycleStatus{Server: server, Component: component, Product: product, Version: version, Kind: kind, State: "unknown", Freshness: intel.Feeds[product].State, InventoryFreshness: inventoryFreshness, SupportContext: "distribution_support"}
	if row.Freshness == "" {
		row.Freshness = "unavailable"
	}
	feed := intel.Feeds[product]
	row.ObservedAt = feed.ObservedAt
	if product != "unknown" {
		row.Source = "https://endoflife.date/" + product
	}
	if kind == "application" {
		row.SupportContext = "upstream_only_distribution_support_must_be_checked"
	}
	// A Debian epoch is packaging metadata, not part of the upstream cycle.
	v := version
	if parts := strings.SplitN(v, ":", 2); len(parts) == 2 {
		v = parts[1]
	}
	v = versionStartRE.FindString(v)
	if v == "" {
		return row
	}
	var chosen *securityintel.Release
	for _, release := range intel.Products[product].Releases {
		if v != release.Name && !strings.HasPrefix(v, release.Name+".") {
			continue
		}
		at, err := time.Parse("2006-01-02", release.ReleaseDate)
		if err != nil || at.After(now) {
			continue
		}
		if chosen == nil || len(release.Name) > len(chosen.Name) {
			copy := release
			chosen = &copy
		}
	}
	if chosen == nil {
		return row
	}
	r := chosen
	row.Cycle, row.EOLAt, row.ExtendedUntil = r.Name, r.EOLFrom, r.EOESFrom
	end, err := time.Parse("2006-01-02", r.EOLFrom)
	if err != nil {
		if r.IsEOL != nil && *r.IsEOL {
			row.State = "end_of_life"
			if kind == "application" {
				row.State = "upstream_end_of_life"
			}
		}
		return row
	}
	if !now.Before(end) {
		row.State = "end_of_life"
		if kind == "application" {
			row.State = "upstream_end_of_life"
		} else if ext, e := time.Parse("2006-01-02", r.EOESFrom); e == nil && now.Before(ext) {
			row.State = "extended_support_unverified"
		}
		return row
	}
	if row.Freshness != "fresh" {
		return row
	}
	row.State = "supported"
	if end.Sub(now) <= 90*24*time.Hour {
		row.State = "ending_soon"
	}
	return row
}

func recentEvidence(value string, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, value)
	return err == nil && !at.After(now) && now.Sub(at) <= 24*time.Hour
}
