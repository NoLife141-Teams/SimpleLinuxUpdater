package vulnerabilities

import (
	"strings"
	"time"
)

// RuntimeContext is bounded host evidence, collected without privilege escalation.
// Kernel ownership is never inferred from a Server's name or from uname alone.
type RuntimeContext struct {
	KernelRelease  string        `json:"kernel_release,omitempty"`
	Virtualization string        `json:"virtualization"` // container, vm, none, unknown
	KernelOwner    string        `json:"kernel_owner,omitempty"`
	Images         []KernelImage `json:"images"`
	Source         string        `json:"source"`
	ObservedAt     string        `json:"observed_at"`
	Reason         string        `json:"reason,omitempty"`
}

type KernelImage struct {
	Package       string `json:"package"`
	Version       string `json:"version"`
	SourcePackage string `json:"source_package"`
	SourceVersion string `json:"source_version"`
}

// RuntimeApplicability preserves component knowledge independently of source
// triage, CVSS, package proof and resolution. Execution does not prove exposure.
type RuntimeApplicability struct {
	KernelRelease  string `json:"kernel_release,omitempty"`
	Virtualization string `json:"virtualization,omitempty"`
	KernelOwner    string `json:"kernel_owner,omitempty"`
	Component      string `json:"component"`
	Execution      string `json:"execution"`
	Exposure       string `json:"exposure"`
	Exploitability string `json:"exploitability"`
	Owner          string `json:"owner"`
	Action         string `json:"action"`
	Reason         string `json:"reason"`
	Source         string `json:"source,omitempty"`
	ObservedAt     string `json:"observed_at,omitempty"`
}

// ClassifyRuntime deliberately never excludes a finding. linux-libc-dev is an
// exact, documented component distinction, not a *-dev or advisory-age rule.
func ClassifyRuntime(binary, version, source, sourceVersion string, host RuntimeContext) RuntimeApplicability {
	r := RuntimeApplicability{Component: "source_match", Execution: "unknown", Exposure: "unknown", Exploitability: "unknown", Owner: "server", Action: "Review binary applicability before Server maintenance", Reason: "A source-package match does not prove vulnerable code is supplied or used by this binary", Source: host.Source, ObservedAt: host.ObservedAt}
	r.KernelRelease, r.Virtualization, r.KernelOwner = host.KernelRelease, host.Virtualization, host.KernelOwner
	name := strings.SplitN(binary, ":", 2)[0]
	if source == "linux" && name == "linux-libc-dev" {
		r.Component = "development_headers"
		r.Reason = "linux-libc-dev supplies userspace development headers, not the running kernel image; binary applicability requires review"
		r.Action = "Review the advisory and header usage; do not infer running-kernel exposure"
		return r
	}
	for _, image := range host.Images {
		if image.Package != binary || image.Version != version || image.SourcePackage != source || image.SourceVersion != sourceVersion {
			continue
		}
		r.Component = "kernel_image"
		if host.Virtualization == "container" {
			r.Owner = "container_host"
			r.Execution = "external"
			r.Reason = "The container uses an external host kernel; installed guest images do not establish the host kernel's vulnerability status"
			r.Action = "Ask the container-host operator to verify and maintain its kernel; a container update or reboot cannot establish remediation"
		} else if host.KernelOwner != "" && host.KernelRelease != "" && (host.Virtualization == "none" || host.Virtualization == "vm") {
			r.Execution = "inactive"
			r.Reason = "Installed kernel image differs from the observed running image; inactive does not mean risk-free"
			r.Action = "Review inactive image usage and boot policy; no automatic removal"
			if host.KernelOwner == binary {
				r.Execution = "active"
				r.Reason = "The installed package owns the observed running kernel image; source match still does not prove exploitability"
				r.Action = "Review Server kernel maintenance and reboot requirements"
			}
		} else {
			r.Owner = "unknown"
			r.Reason = "Kernel image installed, but running image ownership or virtualization evidence is unavailable"
			r.Action = "Collect missing read-only kernel context; no additional privileges requested"
		}
		return r
	}
	if source == "linux" && host.Virtualization == "container" {
		r.Owner = "container_host"
		r.Action = "Review this binary separately; ask the container-host operator to verify its kernel"
		r.Reason += "; the running kernel is managed outside this container"
	}
	return r
}

func ProjectRuntime(r RuntimeApplicability, now time.Time, maxAge time.Duration) (RuntimeApplicability, string) {
	at, err := time.Parse(time.RFC3339, r.ObservedAt)
	if err != nil || r.Source == "" {
		unknown := ClassifyRuntime("", "", "", "", RuntimeContext{})
		unknown.Owner, unknown.Action = "unknown", "Collect missing read-only component context"
		return unknown, "unknown"
	}
	if at.After(now) || now.Sub(at) > maxAge {
		r.Execution, r.Owner = "unknown", "unknown"
		r.Reason = "Runtime context is stale; previous observation: " + r.Reason
		r.Action = "Collect fresh read-only kernel context; no additional privileges requested"
		return r, "stale"
	}
	return r, "fresh"
}

// A retained package finding may outlive the runtime snapshot attached to it.
// Prefer newer host evidence without replacing the original durable observation.
func ProjectFindingRuntime(f Finding, host RuntimeContext, now time.Time, maxAge time.Duration) (RuntimeApplicability, string) {
	hostAt, hostErr := time.Parse(time.RFC3339, host.ObservedAt)
	findingAt, findingErr := time.Parse(time.RFC3339, f.Runtime.ObservedAt)
	if host.Source != "" && hostErr == nil && (findingErr != nil || hostAt.After(findingAt)) {
		r := ClassifyRuntime(f.Package, f.InstalledVersion, f.SourcePackage, f.Assessment.SourceVersion, host)
		if f.Runtime.Component == "kernel_image" && r.Component != "kernel_image" {
			r.Component, r.Owner = "kernel_image", "unknown"
			r.Reason = "Newer host context does not verify this finding's exact installed kernel image identity; the source match remains for review"
			r.Action = "Collect fresh read-only kernel context; no additional privileges requested"
		}
		return ProjectRuntime(r, now, maxAge)
	}
	return ProjectRuntime(f.Runtime, now, maxAge)
}

// KernelCoverage is a statement about component context, never a clean CVE result.
func (h RuntimeContext) KernelCoverage(now time.Time, maxAge time.Duration) string {
	at, err := time.Parse(time.RFC3339, h.ObservedAt)
	if err != nil || at.After(now) || now.Sub(at) > maxAge || h.Source == "" {
		return "unknown"
	}
	if h.Virtualization == "container" {
		return "external"
	}
	if h.KernelRelease != "" && h.KernelOwner != "" && (h.Virtualization == "vm" || h.Virtualization == "none") {
		return "evaluated"
	}
	return "unknown"
}
