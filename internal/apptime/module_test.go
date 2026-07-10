package apptime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestModuleInitializesAndConfiguresAcceptedInterpretation(t *testing.T) {
	store := NewMemoryStore("")
	detector := DetectorFunc(func() (*time.Location, string, error) {
		loc, _ := time.LoadLocation("America/Toronto")
		return loc, "America/Toronto", nil
	})
	module := New(Deps{Store: store, Detector: detector})
	if err := module.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	initial := module.Current()
	if initial.Configured != ChoiceSystem || initial.DisplayName != "America/Toronto" || initial.EditableName != "" {
		t.Fatalf("Current() = %+v", initial)
	}

	updated, err := module.Configure(context.Background(), "Europe/London")
	if err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	if updated.Configured != ChoiceNamed || updated.ResolvedName != "Europe/London" {
		t.Fatalf("Configure() = %+v", updated)
	}
	if got, _ := store.Load(context.Background()); got != "Europe/London" {
		t.Fatalf("stored choice = %q", got)
	}

	store.SaveError = errors.New("disk full")
	if _, err := module.Configure(context.Background(), "UTC"); err == nil {
		t.Fatal("Configure() error = nil, want persistence failure")
	}
	if got := module.Current(); got.ResolvedName != "Europe/London" {
		t.Fatalf("Current() after failed save = %+v", got)
	}
}

func TestInterpretationResolvesDSTGapAndOverlap(t *testing.T) {
	loc, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	interpretation := Interpretation{Location: loc, DisplayName: loc.String()}

	gap := interpretation.ResolveLocal(time.Date(2026, 3, 8, 12, 0, 0, 0, loc), 2, 30)
	if gap.Kind != OccurrenceNonexistent || !gap.Instant.IsZero() {
		t.Fatalf("spring gap = %+v, want nonexistent", gap)
	}

	overlap := interpretation.ResolveLocal(time.Date(2026, 11, 1, 12, 0, 0, 0, loc), 1, 30)
	if overlap.Kind != OccurrenceAmbiguous || overlap.Instant.Format(time.RFC3339) != "2026-11-01T05:30:00Z" || overlap.Offset != "-04:00" {
		t.Fatalf("fall overlap = %+v, want earlier 2026-11-01T05:30:00Z at -04:00", overlap)
	}
}
