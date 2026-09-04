package main

import "testing"

func TestSSHServerAddressCanonicalizesIPLiteral(t *testing.T) {
	tests := []struct {
		name   string
		server Server
		want   string
	}{
		{name: "bracketed expanded IPv6", server: Server{Host: "[2001:0db8:0:0:0:0:0:1]", Port: 22}, want: "[2001:0db8:0:0:0:0:0:1]:22"},
		{name: "IPv6 zone", server: Server{Host: "fe80:0:0:0::1%eth0", Port: 2201}, want: "[fe80:0:0:0::1%eth0]:2201"},
		{name: "mapped IPv4", server: Server{Host: "::ffff:192.0.2.10", Port: 22}, want: "[::ffff:192.0.2.10]:22"},
		{name: "DNS presentation", server: Server{Host: "NODE.EXAMPLE", Port: 22}, want: "NODE.EXAMPLE:22"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sshServerAddress(tt.server); got != tt.want {
				t.Fatalf("sshServerAddress(%+v) = %q, want %q", tt.server, got, tt.want)
			}
		})
	}
}
