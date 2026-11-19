package main

import (
	"net/url"
	"testing"
)

func TestApprovalApplicationOrigin(t *testing.T) {
	for _, input := range []string{
		"data:text/html,updates", "//updates.example", "https://", "https://user:pass@updates.example",
		"https://updates.example/path", "https://updates.example:99999", "https://updates.example/?", "https://updates.example/#fragment",
		"https://[updates.example]/", "http://::1/", "https://%25.example/",
		"https://999.999.0.1/", "https://updates\u00a0example.test/",
		"https://updates.example.123/", "https://4294967296/", "https://updates.example.0x1234/",
		"http://010.0.0.1/", "https://xn--.example/",
	} {
		if _, err := parseApplicationOrigin(input); err == nil {
			t.Errorf("accepted invalid application origin %q", input)
		}
	}
	if origin, err := parseApplicationOrigin(""); origin != nil || err != nil {
		t.Fatalf("unset origin should disable links: %v %v", origin, err)
	}
	for _, input := range []string{
		"https://updates.example/", "https://updates.localhost/", "http://localhost:8080/",
		"http://updates_host.internal:8080/", "https://xn--caf-dma.example/", "http://127.0.0.1:8080/", "http://[::1]:8080/",
	} {
		if origin, err := parseApplicationOrigin(input); err != nil || origin.String() != input {
			t.Errorf("valid application origin %q: %v %v", input, origin, err)
		}
	}
	if origin, err := parseApplicationOrigin("https://café.example/"); err != nil || origin.Hostname() != "café.example" {
		t.Errorf("valid internationalized application origin: %v %v", origin, err)
	}
}

func TestStatusReturnOnlyAllowsExactInternalStatusLink(t *testing.T) {
	valid := (approvalLink{ServerIdentity: "stable", JobID: "job-1", Generation: 3}).url(url.URL{})
	const batchLink = "/?maintenance_batch=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, target := range []string{valid, batchLink} {
		if result := statusReturnDestination(target); result != target {
			t.Fatalf("valid return %q changed to %q", target, result)
		}
	}
	for _, target := range []string{
		"https://somewhere.example" + valid, "//somewhere.example" + valid, "/%2f" + valid,
		"/\\somewhere.example" + valid, "/manage", "/api/approve/srv", "/?server_identity=stable",
		valid + "&job_id=other", valid + "&next=https://somewhere.example", valid + "%0A", valid + "#fragment",
		"https://somewhere.example" + batchLink, batchLink + "&maintenance_batch=other", batchLink + "&other=1", "/?maintenance_batch=short",
	} {
		if result := statusReturnDestination(target); result != "/" {
			t.Errorf("unsafe return %q accepted as %q", target, result)
		}
	}
}
