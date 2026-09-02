package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	listenAddrEnv     = "DEBIAN_UPDATER_LISTEN_ADDR"
	defaultListenAddr = "127.0.0.1:8080"
)

func resolveListenAddr(getenv func(string) string) (string, error) {
	raw := strings.TrimSpace(getenv(listenAddrEnv))
	if raw == "" {
		return defaultListenAddr, nil
	}

	host, portText, err := net.SplitHostPort(raw)
	if err != nil {
		return "", fmt.Errorf("%s=%q must use host:port syntax (IPv6 addresses require brackets): %w", listenAddrEnv, raw, err)
	}
	if strings.ContainsAny(host, " \t\r\n/\\[]") {
		return "", fmt.Errorf("%s=%q contains an invalid listen host", listenAddrEnv, raw)
	}
	if strings.Contains(host, ":") {
		ipHost := host
		if zoneIndex := strings.LastIndexByte(ipHost, '%'); zoneIndex >= 0 {
			if zoneIndex == len(ipHost)-1 {
				return "", fmt.Errorf("%s=%q contains an invalid IPv6 zone", listenAddrEnv, raw)
			}
			ipHost = ipHost[:zoneIndex]
		}
		if net.ParseIP(ipHost) == nil {
			return "", fmt.Errorf("%s=%q contains an invalid IPv6 listen host", listenAddrEnv, raw)
		}
	} else if strings.HasPrefix(raw, "[") {
		return "", fmt.Errorf("%s=%q contains an invalid bracketed listen host", listenAddrEnv, raw)
	} else if looksLikeIPv4Literal(host) && net.ParseIP(host) == nil {
		return "", fmt.Errorf("%s=%q contains an invalid IPv4 listen host", listenAddrEnv, raw)
	}
	if portText == "" || strings.IndexFunc(portText, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return "", fmt.Errorf("%s=%q must use a numeric TCP port", listenAddrEnv, raw)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("%s=%q must use a TCP port in the range 1..65535", listenAddrEnv, raw)
	}
	return raw, nil
}

func looksLikeIPv4Literal(host string) bool {
	return strings.Contains(host, ".") && strings.IndexFunc(host, func(r rune) bool {
		return (r < '0' || r > '9') && r != '.'
	}) == -1
}
