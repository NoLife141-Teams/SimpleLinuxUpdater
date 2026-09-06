package main

import (
	"context"
	"testing"
	"time"
)

func TestPreparePersistenceReplacementClearsPendingPolicySchedulerTicks(t *testing.T) {
	service := NewPolicyService(PolicyServiceDeps{})
	missed := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	service.RememberMissedTick(missed)
	if got := service.PendingMissedTicks(); len(got) != 1 {
		t.Fatalf("pending missed ticks before replacement = %v, want one", got)
	}

	composition := &runtimeComposition{
		deps: AppDeps{PolicyService: service},
	}
	if err := composition.PreparePersistenceReplacement(context.Background()); err != nil {
		t.Fatalf("PreparePersistenceReplacement() error = %v", err)
	}
	if got := service.PendingMissedTicks(); len(got) != 0 {
		t.Fatalf("pending missed ticks after replacement handoff = %v, want none", got)
	}
}
