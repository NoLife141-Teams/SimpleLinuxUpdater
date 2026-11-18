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
	} {
		if _, err := parseApplicationOrigin(input); err == nil {
			t.Errorf("accepted invalid application origin %q", input)
		}
	}
	if origin, err := parseApplicationOrigin(""); origin != nil || err != nil {
		t.Fatalf("unset origin should disable links: %v %v", origin, err)
	}
	for _, input := range []string{"https://updates.example/", "http://127.0.0.1:8080/", "http://[::1]:8080/"} {
		if origin, err := parseApplicationOrigin(input); err != nil || origin.String() != input {
			t.Errorf("valid application origin %q: %v %v", input, origin, err)
		}
	}
}

func TestApprovalReturnOnlyAllowsExactInternalStatusLink(t *testing.T) {
	valid := (approvalLink{ServerIdentity: "stable", JobID: "job-1", Generation: 3}).url(url.URL{})
	if result := approvalReturnDestination(valid); result != valid {
		t.Fatalf("valid return changed to %q", result)
	}
	for _, target := range []string{
		"https://somewhere.example" + valid, "//somewhere.example" + valid, "/%2f" + valid,
		"/\\somewhere.example" + valid, "/manage", "/api/approve/srv", "/?server_identity=stable",
		valid + "&job_id=other", valid + "&next=https://somewhere.example", valid + "%0A", valid + "#fragment",
	} {
		if result := approvalReturnDestination(target); result != "/" {
			t.Errorf("unsafe return %q accepted as %q", target, result)
		}
	}
}
