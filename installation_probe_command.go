package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

func localProbeURL(getenv func(string) string) (string, error) {
	address, err := resolveListenAddr(getenv)
	if err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/readyz"}).String(), nil
}

// runInstallationProbeCommand runs before acquiring the installation lock or
// opening SQLite, so Docker can execute it as the unprivileged application user.
func runInstallationProbeCommand(args []string, getenv func(string) string) (bool, int) {
	if len(args) == 0 || args[0] != "healthcheck" {
		return false, 0
	}
	if len(args) != 1 {
		return true, 1
	}
	target, err := localProbeURL(getenv)
	if err != nil {
		return true, 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return true, 1
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return true, 1
	}
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	if err != nil || response.StatusCode != http.StatusOK {
		return true, 1
	}
	return true, 0
}
