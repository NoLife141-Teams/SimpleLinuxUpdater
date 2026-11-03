package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/vulnerabilities"
)

func TestInstalledScanGeneratesBoundStablePackageProofs(t *testing.T) {
	for _, mode := range []string{"partial", "severity_outage", "changed_a", "changed_b", "changed_source", "changed_distro", "pagination_failure", "truncated_end"} {
		t.Run(mode, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if mode == "changed_a" {
						fmt.Fprint(w, `{"id":"DEBIAN-CVE-2026-2000","affected":[{"package":{"ecosystem":"Debian:12","name":"source-a"},"ecosystem_specific":{"urgency":"high"}}]}`)
						return
					}
					http.Error(w, "optional details unavailable", http.StatusServiceUnavailable)
					return
				}
				var req osvBatchRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				res := osvBatchResponse{Results: make([]osvQueryResult, len(req.Queries))}
				for i, q := range req.Queries {
					if mode == "pagination_failure" {
						if q.PageToken != "" {
							http.Error(w, "page unavailable", http.StatusServiceUnavailable)
							return
						}
						res.Results[i].NextPageToken = "next"
					}
					if mode == "severity_outage" || mode == "changed_a" {
						res.Results[i].Vulnerabilities = []osvVulnerabilityReference{{ID: "DEBIAN-CVE-2026-2000"}}
					}
				}
				json.NewEncoder(w).Encode(res)
			}))
			defer api.Close()
			session := &HostMaintenanceSessionFuncs{RunCommandFunc: func(ctx context.Context, req HostCommandRequest) (HostCommandResult, error) {
				if req.Effect != HostCommandEffectReadOnly {
					t.Fatal("mutating scan")
				}
				switch req.Operation {
				case "vulnerability_scan.runtime_context":
					return HostCommandResult{}, nil
				case "vulnerability_scan.installed_inventory", "vulnerability_scan.installed_inventory_verify":
					a, b, source, distro := "2", "1", "2", "debian\t12"
					if req.Operation == "vulnerability_scan.installed_inventory_verify" {
						switch mode {
						case "changed_a":
							a = "3"
							source = "3"
						case "changed_b":
							b = "2"
						case "changed_source":
							source = "repacked"
						case "changed_distro":
							distro = "ubuntu\t24.04"
						case "truncated_end":
							return HostCommandResult{Stdout: "aa:amd64\t2"}, nil
						}
					}
					return HostCommandResult{Stdout: fmt.Sprintf("__SLU_OS__\t%s\naa:amd64\t%s\tsource-a\t%s\nbb:amd64\t%s\tsource-b\t%s\n__SLU_INVENTORY_END__\n", distro, a, source, b, b)}, nil
				case "cve_enrichment.package_inventory":
					return HostCommandResult{Stdout: "__SLU_OS__\tdebian\t12\n__SLU_PACKAGE__\taa:amd64\taa:amd64\tsource-a\t2\t2\tDebian\tverified\ta\t2\tsource-a (2)\tDebian\tverified\n__SLU_PACKAGE__\tbb:amd64\tbb:amd64\tsource-b\t1\t1\tAcme\tverified\tb\t1\tsource-b (1)\tAcme\tverified\n"}, nil
				default:
					t.Fatal(req.Operation)
					return HostCommandResult{}, nil
				}
			}}
			scanner := NewOSVVulnerabilityScanner(OSVVulnerabilityScannerDeps{Endpoint: api.URL + "/querybatch", HTTPClient: api.Client()})
			scan, err := scanner.ScanInstalled(vulnerabilities.WithScanIdentity(t.Context(), "host", "admitted"), session)
			old := vulnerabilities.Accept(vulnerabilities.Observation{}, vulnerabilities.Scan{State: "partial", Scope: "installed_packages", FinishedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), Findings: []vulnerabilities.Finding{{Package: "aa:amd64", ID: "CVE-2026-1000", InstalledVersion: "1"}, {Package: "bb:amd64", ID: "CVE-2026-1001", InstalledVersion: "1"}}})
			next := vulnerabilities.Accept(old, scan)
			wantResolved := mode == "partial" || mode == "severity_outage" || mode == "changed_b"
			for _, f := range next.Findings {
				if f.ID == "CVE-2026-1000" && (f.ResolvedAt != "") != wantResolved {
					t.Fatalf("mode %s scan=%+v finding=%+v err=%v", mode, scan, f, err)
				}
				if f.Package == "bb:amd64" && f.ResolvedAt != "" {
					t.Fatal("unverified package resolved", f)
				}
			}
			if mode == "truncated_end" {
				if err == nil {
					t.Fatal("truncated inventory accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if wantResolved {
				p := scan.PackageProof(scan.Packages[0])
				if p == nil || p.ScanID != "admitted" || p.ServerName != "host" || p.SourceVersion != "2" {
					t.Fatal(scan)
				}
				if mode == "severity_outage" && (len(scan.EnrichmentErrors) != 1 || len(scan.Errors) != 0) {
					t.Fatal(scan)
				}
			}
			if mode == "changed_b" && scan.InventoryProof.Stable {
				t.Fatal("whole inventory incorrectly stable")
			}
			if mode == "pagination_failure" && scan.Packages[0].Proof != nil {
				t.Fatal("incomplete source pagination attested")
			}
			if mode == "changed_a" && scan.Findings[0].Assessment.Qualification() != vulnerabilities.QualificationReview {
				t.Fatal("changed package identity still confirmed", scan.Findings[0])
			}
		})
	}
}

func TestInstalledInventoryProofCommandAndParser(t *testing.T) {
	cmd := BuildInstalledPackageInventoryCmd()
	for _, want := range []string{"${source:Package}", "${source:Version}", "__SLU_OS__", "dpkg-query"} {
		if !strings.Contains(cmd, want) {
			t.Fatal(cmd)
		}
	}
	for _, bad := range []string{"__SLU_OS__\tdebian\t12\n__SLU_OS__\tdebian\t13\npkg\t1\n__SLU_INVENTORY_END__\n", "pkg\t1\tbad source\t1\n__SLU_INVENTORY_END__\n"} {
		if _, err := parseInstalledPackages(bad); err == nil {
			t.Fatal("accepted invalid inventory", bad)
		}
	}
}
