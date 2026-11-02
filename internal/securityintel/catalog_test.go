package securityintel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCatalogRetainsLastGoodPublicDataAcrossFailuresAndRestart(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	fail := false
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != "GET" || r.Body != nil || r.URL.RawQuery != "" {
			t.Fatal("feed request contains non-public inventory")
		}
		body := `{"result":{"name":"ubuntu","releases":[{"name":"25.04","releaseDate":"2025-04-17","eolFrom":"2026-01-17","isEol":true}]}}`
		code := 200
		if strings.Contains(r.URL.Path, "known_exploited") {
			body = `{"vulnerabilities":[{"cveID":"CVE-2026-1000","dateAdded":"2026-11-01","requiredAction":"Read vendor guidance"}]}`
		} else if !strings.Contains(r.URL.Path, "/ubuntu/") {
			code = 404
		}
		if fail {
			code = 503
			body = "unavailable"
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	c := New(Config{CacheDir: dir, Now: func() time.Time { return now }, Client: client})
	if len(c.Snapshot().Products) != 0 {
		t.Fatal("unavailable feed must stay unknown")
	}
	c.Refresh(context.Background())
	first := c.Snapshot()
	if first.Products["ubuntu"].Releases[0].EOLFrom != "2026-01-17" || first.Feeds["ubuntu"].State != "fresh" || len(first.Exploited) != 1 {
		t.Fatalf("bad feed interpretation: %+v", first)
	}
	fail = true
	now = now.Add(48 * time.Hour)
	c.Refresh(context.Background())
	last := c.Snapshot()
	if last.Feeds["ubuntu"].State != "stale" || last.Feeds["ubuntu"].LastError == "" || len(last.Products["ubuntu"].Releases) != 1 {
		t.Fatal("refresh failure discarded known lifecycle or pretended freshness")
	}
	restarted := New(Config{CacheDir: dir, Now: func() time.Time { return now }, Client: client}).Snapshot()
	if len(restarted.Exploited) != 1 || restarted.Feeds["ubuntu"].State != "stale" {
		t.Fatal("public cache did not survive restart")
	}
	if requests != 2*len(feedURLs()) {
		t.Fatal("unexpected per-host feed requests", requests)
	}
}
func TestCatalogRejectsFutureForgedAndInvalidCachedMetadata(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct{ name, source, at, data string }{{"wrong_source", "https://example.invalid", "2026-11-01T00:00:00Z", `{"result":{"name":"ubuntu","releases":[{"name":"25.04"}]}}`}, {"future", feedURLs()["ubuntu"], "2099-01-01T00:00:00Z", `{"result":{"name":"ubuntu","releases":[{"name":"25.04"}]}}`}, {"wrong_product", feedURLs()["ubuntu"], "2026-11-01T00:00:00Z", `{"result":{"name":"debian","releases":[{"name":"12"}]}}`}, {"bad_json", feedURLs()["ubuntu"], "2026-11-01T00:00:00Z", `{`}} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			raw, _ := json.Marshal(cachedFeed{Source: test.source, ObservedAt: test.at, Data: json.RawMessage(test.data)})
			if len(raw) == 0 {
				raw = []byte(`{"source":"` + test.source + `","observed_at":"` + test.at + `","data":{}}`)
			}
			if err := os.WriteFile(filepath.Join(dir, "ubuntu.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			s := New(Config{SeedDir: dir, Now: func() time.Time { return now }}).Snapshot()
			if len(s.Products) != 0 || s.Feeds["ubuntu"].State != "unavailable" {
				t.Fatal("invalid source promoted to support evidence")
			}
		})
	}
}
