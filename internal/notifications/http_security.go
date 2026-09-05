package notifications

import (
	"net/http"
	"time"
)

// secureNotificationHTTPClient prevents notification delivery from following
// redirects. The configured destination is the only network endpoint that may
// receive a notification payload; redirect targets are never trusted implicitly.
//
// Custom HTTPClient implementations are explicit trusted dependencies used by
// tests or embedding code. Standard *http.Client dependencies are cloned before
// hardening so the caller's client is not mutated.
func secureNotificationHTTPClient(client HTTPClient) HTTPClient {
	if client == nil {
		return &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: rejectNotificationRedirect,
		}
	}
	httpClient, ok := client.(*http.Client)
	if !ok || httpClient == nil {
		return client
	}
	clone := *httpClient
	clone.CheckRedirect = rejectNotificationRedirect
	return &clone
}

func rejectNotificationRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}
