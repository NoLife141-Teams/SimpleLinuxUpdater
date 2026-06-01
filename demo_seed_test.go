package main

import "testing"

func TestDemoSeedResetEnabled(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "empty disabled", raw: "", want: false},
		{name: "arbitrary disabled", raw: "variant-b", want: false},
		{name: "one enabled", raw: "1", want: true},
		{name: "true enabled", raw: "true", want: true},
		{name: "yes enabled", raw: "yes", want: true},
		{name: "reset enabled", raw: "reset", want: true},
		{name: "variant c enabled", raw: "variant-c", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := demoSeedResetEnabled(tt.raw); got != tt.want {
				t.Fatalf("demoSeedResetEnabled(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
