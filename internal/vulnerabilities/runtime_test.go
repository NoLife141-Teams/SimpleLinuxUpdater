package vulnerabilities

import (
	"testing"
	"time"
)

func TestRuntimeComponentEvidenceNeverMeansExploitabilityOrExclusion(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	host := RuntimeContext{KernelRelease: "6.1-synthetic", Virtualization: "none", KernelOwner: "linux-image-6.1-synthetic:amd64", ObservedAt: now.Format(time.RFC3339), Source: "injected-session", Images: []KernelImage{
		{Package: "linux-image-6.1-synthetic:amd64", Version: "1", SourcePackage: "linux", SourceVersion: "1"},
		{Package: "linux-image-6.0-synthetic:amd64", Version: "1", SourcePackage: "linux", SourceVersion: "1"},
	}}
	for _, tc := range []struct{ binary, source, virtualization, owner, component, execution string }{
		{"linux-libc-dev:amd64", "linux", "none", host.KernelOwner, "development_headers", "unknown"},
		{"linux-image-6.1-synthetic:amd64", "linux", "none", host.KernelOwner, "kernel_image", "active"},
		{"linux-image-6.1-synthetic:amd64", "linux", "vm", host.KernelOwner, "kernel_image", "active"},
		{"linux-image-6.0-synthetic:amd64", "linux", "vm", host.KernelOwner, "kernel_image", "inactive"},
		{"linux-image-6.1-synthetic:amd64", "linux", "container", "", "kernel_image", "external"},
		{"linux-image-6.1-synthetic:amd64", "linux", "vm", "", "kernel_image", "unknown"},
		{"linux-image-6.1-synthetic:amd64", "linux", "unknown", host.KernelOwner, "kernel_image", "unknown"},
		{"example-dev", "example", "none", host.KernelOwner, "source_match", "unknown"},
		{"example-cli", "example", "none", host.KernelOwner, "source_match", "unknown"},
	} {
		t.Run(tc.binary+tc.virtualization, func(t *testing.T) {
			h := host
			h.Virtualization, h.KernelOwner = tc.virtualization, tc.owner
			r := ClassifyRuntime(tc.binary, "1", tc.source, "1", h)
			if r.Component != tc.component || r.Execution != tc.execution || r.Exposure != "unknown" || r.Exploitability != "unknown" || r.Reason == "" || r.Action == "" {
				t.Fatalf("%+v", r)
			}
			if tc.execution == "external" && r.Owner != "container_host" {
				t.Fatal(r)
			}
		})
	}
	changed := ClassifyRuntime(host.Images[0].Package, "2", "linux", "2", host)
	if changed.Execution != "unknown" {
		t.Fatal("image identity changed during collection", changed)
	}
	active := ClassifyRuntime(host.Images[0].Package, "1", "linux", "1", host)
	stale, freshness := ProjectRuntime(active, now.Add(2*time.Hour), time.Hour)
	if stale.Execution != "unknown" || freshness != "stale" || active.Execution != "active" || host.KernelCoverage(now.Add(2*time.Hour), time.Hour) != "unknown" {
		t.Fatal(stale, freshness)
	}
	if _, freshness := ProjectRuntime(RuntimeApplicability{}, now, time.Hour); freshness != "unknown" {
		t.Fatal(freshness)
	}
	host.Virtualization, host.KernelOwner = "container", ""
	if host.KernelCoverage(now, time.Hour) != "external" {
		t.Fatal(host)
	}
}
