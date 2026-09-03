package main

import (
	"errors"
	"testing"

	updatespkg "debian-updater/internal/updates"
)

func TestClassifyCommandTimeoutUsesTypedEffectNotCommandSyntax(t *testing.T) {
	timeoutErr := errors.New("connection lost after timeout")
	tests := []struct {
		name               string
		effect             updatespkg.HostCommandEffect
		wantTagged         bool
		wantReconciliation bool
	}{
		{
			name:   "read-only effect",
			effect: updatespkg.HostCommandEffectReadOnly,
		},
		{
			name:       "metadata mutation",
			effect:     updatespkg.HostCommandEffectMetadataMutation,
			wantTagged: true,
		},
		{
			name:               "package-state mutation",
			effect:             updatespkg.HostCommandEffectPackageStateMutation,
			wantTagged:         true,
			wantReconciliation: true,
		},
		{
			name:   "system-state mutation",
			effect: updatespkg.HostCommandEffectSystemStateMutation,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyCommandTimeout(tt.effect, timeoutErr)
			var tagged updatespkg.NonRetryableTaggedError
			if errors.As(got, &tagged) != tt.wantTagged {
				t.Fatalf("classifyCommandTimeout(%q) tagged = %t, want %t", tt.effect, errors.As(got, &tagged), tt.wantTagged)
			}
			if tt.wantTagged && tagged.RequiresReconciliation() != tt.wantReconciliation {
				t.Fatalf("classifyCommandTimeout(%q) reconciliation = %t, want %t", tt.effect, tagged.RequiresReconciliation(), tt.wantReconciliation)
			}
		})
	}
}
