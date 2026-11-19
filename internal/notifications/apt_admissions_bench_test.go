package notifications

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkAPTAdmissionLedger(b *testing.B) {
	for _, size := range []int{100, 1000, 10000, 50000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			b.Run("Reconcile", func(b *testing.B) {
				h := aptAdmissionBenchmarkFixture(b, size)
				ctx := b.Context()
				b.ReportAllocs()
				for b.Loop() {
					if err := h.service.reconcileConditions(ctx); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("PrepareAndStageRestore", func(b *testing.B) {
				h := aptAdmissionBenchmarkFixture(b, size)
				ctx := b.Context()
				archive := openOutboxTestDB(b, "archive-template.db")
				if err := EnsureSchema(archive); err != nil {
					b.Fatal(err)
				}
				if err := insertPendingAPTBenchmarkRow(b, h, archive); err != nil {
					b.Fatal(err)
				}
				path := filepath.Join(b.TempDir(), "staged.db")
				b.ReportAllocs()
				for b.Loop() {
					b.StopTimer()
					if _, err := archive.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
						b.Fatal(err)
					}
					staged, err := sql.Open("sqlite", path)
					if err != nil {
						b.Fatal(err)
					}
					// Each run starts with an unprepared service and a fresh
					// copy of the archive, like an in-place restore.
					service := &Service{deps: h.service.deps, wake: make(chan struct{}, 1)}
					b.StartTimer()
					if err := service.PreparePersistenceReplacement(ctx); err != nil {
						b.Fatal(err)
					}
					if err := service.StageAPTDeliveryEvidence(ctx, staged); err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					if err := staged.Close(); err != nil {
						b.Fatal(err)
					}
					if err := os.Remove(path); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
				}
			})
		})
	}
}

func aptAdmissionBenchmarkFixture(b *testing.B, size int) *conditionHarness {
	b.Helper()
	h := newConditionHarness(b)
	h.db.SetMaxOpenConns(1)
	if _, err := h.db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL"); err != nil {
		b.Fatal(err)
	}
	h.facts = []ConditionFact{{
		ID: APTReconciliationConditionID("active-target", "active-job"), EventType: EventAPTReconciliationRequired,
		TargetType: "server", TargetName: "active-host", Active: true,
	}}
	settings, err := h.service.currentSettings()
	if err != nil {
		b.Fatal(err)
	}
	fingerprint := destinationConfigFingerprint(settings, DestinationWebhook)
	state, err := loadConditionState(b.Context(), h.db)
	if err != nil {
		b.Fatal(err)
	}
	state.Admissions = make(map[string]bool, size)
	for i := range size {
		fact := ConditionFact{ID: APTReconciliationConditionID("historical-target", fmt.Sprint(i))}
		state.Admissions[aptAdmissionID(fact, fingerprint)] = true
	}
	if err := saveConditionState(b.Context(), h.db, &state); err != nil {
		b.Fatal(err)
	}
	// Simulate opening a previously installed database with this history.
	if err := EnsureSchema(h.db); err != nil {
		b.Fatal(err)
	}
	if err := h.service.reconcileConditions(b.Context()); err != nil {
		b.Fatal(err)
	}
	return h
}

func insertPendingAPTBenchmarkRow(b *testing.B, h *conditionHarness, archive *sql.DB) error {
	b.Helper()
	settings, err := h.service.currentSettings()
	if err != nil {
		return err
	}
	fingerprint := destinationConfigFingerprint(settings, DestinationWebhook)
	fact := ConditionFact{ID: APTReconciliationConditionID("historical-target", "0"), EventType: EventAPTReconciliationRequired,
		TargetType: "server", TargetName: "historical-host", Active: true}
	intent := conditionIntent(Condition{Fact: fact, Episode: 1})
	intent.ID = aptAdmissionID(fact, fingerprint)
	rows, err := buildOutboxRows(intent, fact.EventType, []string{DestinationWebhook}, h.now)
	if err != nil {
		return err
	}
	rows[0].DestinationFingerprint = fingerprint
	return insertOutboxRows(b.Context(), archive, rows, h.now.Format(time.RFC3339Nano))
}
