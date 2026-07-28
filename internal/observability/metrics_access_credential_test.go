package observability

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMetricsAccessCredentialStatusAndVerification(t *testing.T) {
	ctx := context.Background()
	store := &credentialStoreStub{hash: "accepted-hash"}
	credential := NewMetricsAccessCredential(MetricsAccessCredentialDeps{
		Store: store,
		ComparePasswordAndHash: func(presented, hash string) (bool, error) {
			return presented == "accepted-token" && hash == "accepted-hash", nil
		},
	})

	status, err := credential.Status(ctx)
	if err != nil || status != MetricsAccessEnabled {
		t.Fatalf("Status() = %q, %v; want enabled, nil", status, err)
	}
	result, err := credential.Verify(ctx, "accepted-token")
	if err != nil || result != MetricsAccessAccepted {
		t.Fatalf("Verify(valid) = %q, %v; want accepted, nil", result, err)
	}
	result, err = credential.Verify(ctx, "wrong-token")
	if err != nil || result != MetricsAccessRejected {
		t.Fatalf("Verify(invalid) = %q, %v; want rejected, nil", result, err)
	}
	if store.loads != 1 {
		t.Fatalf("store loads = %d, want one accepted-state load", store.loads)
	}
	if store.updates != 1 {
		t.Fatalf("usage updates = %d, want only the accepted verification", store.updates)
	}
}

func TestMetricsAccessCredentialDistinguishesDisabledAndUnavailable(t *testing.T) {
	ctx := context.Background()
	disabled := NewMetricsAccessCredential(MetricsAccessCredentialDeps{Store: &credentialStoreStub{}})
	status, err := disabled.Status(ctx)
	if err != nil || status != MetricsAccessDisabled {
		t.Fatalf("disabled Status() = %q, %v; want disabled, nil", status, err)
	}
	result, err := disabled.Verify(ctx, "anything")
	if err != nil || result != MetricsAccessDisabledVerification {
		t.Fatalf("disabled Verify() = %q, %v; want disabled, nil", result, err)
	}

	loadErr := errors.New("persistence unavailable")
	unavailable := NewMetricsAccessCredential(MetricsAccessCredentialDeps{Store: &credentialStoreStub{loadErr: loadErr}})
	status, err = unavailable.Status(ctx)
	if !errors.Is(err, loadErr) || status != MetricsAccessUnavailable {
		t.Fatalf("unavailable Status() = %q, %v; want unavailable, load error", status, err)
	}
	result, err = unavailable.Verify(ctx, "anything")
	if !errors.Is(err, loadErr) || result != MetricsAccessUnavailableVerification {
		t.Fatalf("unavailable Verify() = %q, %v; want unavailable, load error", result, err)
	}
}

func TestMetricsAccessCredentialDoesNotAcceptUsageWhenLifecyclePersistenceFails(t *testing.T) {
	ctx := context.Background()
	store := &credentialStoreStub{
		hash:      "accepted-hash",
		updateErr: errors.New("usage persistence failed"),
	}
	credential := NewMetricsAccessCredential(MetricsAccessCredentialDeps{
		Store: store,
		ComparePasswordAndHash: func(presented, hash string) (bool, error) {
			return presented == "accepted-token" && hash == "accepted-hash", nil
		},
	})
	result, err := credential.VerifyWithOrigin(ctx, "accepted-token", "203.0.113.44")
	if !errors.Is(err, store.updateErr) || result != MetricsAccessUnavailableVerification {
		t.Fatalf("VerifyWithOrigin() = %q, %v; want unavailable persistence failure", result, err)
	}
	if store.lifecycle != (MetricsCredentialLifecycle{}) {
		t.Fatalf("failed usage persistence published lifecycle = %+v", store.lifecycle)
	}
}

func TestMetricsAccessCredentialPublishesRotationOnlyAfterPersistence(t *testing.T) {
	ctx := context.Background()
	store := &credentialStoreStub{hash: "old-hash"}
	credential := NewMetricsAccessCredential(MetricsAccessCredentialDeps{
		Store: store,
		RandomRead: func(buf []byte) (int, error) {
			for i := range buf {
				buf[i] = 7
			}
			return len(buf), nil
		},
		HashPassword: func(clear string) (string, error) {
			if clear == "" {
				t.Fatal("HashPassword received empty clear credential")
			}
			return "new-hash", nil
		},
		ComparePasswordAndHash: func(presented, hash string) (bool, error) {
			return (presented == "old-token" && hash == "old-hash") || (presented == "new-token" && hash == "new-hash"), nil
		},
	})
	if status, err := credential.Status(ctx); err != nil || status != MetricsAccessEnabled {
		t.Fatalf("prime Status() = %q, %v", status, err)
	}

	store.replaceErr = errors.New("write failed")
	if _, err := credential.Rotate(ctx); err == nil {
		t.Fatal("Rotate() error = nil, want persistence failure")
	}
	if result, err := credential.Verify(ctx, "old-token"); err != nil || result != MetricsAccessAccepted {
		t.Fatalf("old credential after failed rotation = %q, %v; want accepted", result, err)
	}

	store.replaceErr = nil
	clear, err := credential.Rotate(ctx)
	if err != nil || clear == "" {
		t.Fatalf("Rotate() = %q, %v; want one clear credential", clear, err)
	}
	if store.hash != "new-hash" || store.replaces != 2 {
		t.Fatalf("persisted rotation = hash %q, writes %d; want new-hash, 2", store.hash, store.replaces)
	}
}

func TestMetricsAccessCredentialPublishesDisableOnlyAfterPersistence(t *testing.T) {
	ctx := context.Background()
	store := &credentialStoreStub{hash: "accepted-hash"}
	credential := NewMetricsAccessCredential(MetricsAccessCredentialDeps{
		Store: store,
		ComparePasswordAndHash: func(presented, hash string) (bool, error) {
			return presented == "accepted-token" && hash == "accepted-hash", nil
		},
	})
	if status, err := credential.Status(ctx); err != nil || status != MetricsAccessEnabled {
		t.Fatalf("prime Status() = %q, %v", status, err)
	}

	store.deleteErr = errors.New("delete failed")
	if err := credential.Disable(ctx); err == nil {
		t.Fatal("Disable() error = nil, want persistence failure")
	}
	if result, err := credential.Verify(ctx, "accepted-token"); err != nil || result != MetricsAccessAccepted {
		t.Fatalf("credential after failed disable = %q, %v; want accepted", result, err)
	}

	store.deleteErr = nil
	if err := credential.Disable(ctx); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	if status, err := credential.Status(ctx); err != nil || status != MetricsAccessDisabled {
		t.Fatalf("Status() after disable = %q, %v; want disabled", status, err)
	}
}

func TestMetricsAccessCredentialInvalidationLoadsReplacementPersistence(t *testing.T) {
	ctx := context.Background()
	store := &credentialStoreStub{hash: "before-hash"}
	credential := NewMetricsAccessCredential(MetricsAccessCredentialDeps{
		Store: store,
		ComparePasswordAndHash: func(presented, hash string) (bool, error) {
			return (presented == "before" && hash == "before-hash") || (presented == "after" && hash == "after-hash"), nil
		},
	})
	if result, err := credential.Verify(ctx, "before"); err != nil || result != MetricsAccessAccepted {
		t.Fatalf("Verify(before) = %q, %v", result, err)
	}

	store.hash = "after-hash"
	credential.Invalidate()
	credential.Invalidate()
	if store.loads != 1 {
		t.Fatalf("Invalidate() performed persistence I/O: loads = %d, want 1", store.loads)
	}
	if result, err := credential.Verify(ctx, "before"); err != nil || result != MetricsAccessRejected {
		t.Fatalf("Verify(stale) after invalidation = %q, %v; want rejected", result, err)
	}
	if result, err := credential.Verify(ctx, "after"); err != nil || result != MetricsAccessAccepted {
		t.Fatalf("Verify(restored) = %q, %v; want accepted", result, err)
	}
	if store.loads != 2 {
		t.Fatalf("loads after invalidation = %d, want 2", store.loads)
	}
}

func TestMetricsAccessCredentialLifecycleFactsAndSafeUsageOrigin(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	store := &credentialStoreStub{}
	credential := NewMetricsAccessCredential(MetricsAccessCredentialDeps{
		Store: store,
		Now:   func() time.Time { return now },
		RandomRead: func(buf []byte) (int, error) {
			for i := range buf {
				buf[i] = 9
			}
			return len(buf), nil
		},
		HashPassword: func(string) (string, error) { return "generated-hash", nil },
		ComparePasswordAndHash: func(presented, hash string) (bool, error) {
			return presented == "accepted-token" && hash == "generated-hash", nil
		},
	})

	if _, err := credential.Rotate(ctx); err != nil {
		t.Fatalf("Rotate(create) error = %v", err)
	}
	details, err := credential.Details(ctx)
	if err != nil {
		t.Fatalf("Details(create) error = %v", err)
	}
	if !details.Enabled || details.LifecycleState != MetricsAccessLifecycleNeverUsed ||
		!details.NeverUsed || details.Stale || details.CreatedAt != "2026-06-01T12:00:00Z" ||
		details.RotatedAt != "" || details.LastUsedAt != "" || details.StaleAfterDays != 30 {
		t.Fatalf("Details(create) = %+v", details)
	}

	now = now.Add(2 * time.Hour)
	result, err := credential.VerifyWithOrigin(ctx, "accepted-token", "203.0.113.44")
	if err != nil || result != MetricsAccessAccepted {
		t.Fatalf("VerifyWithOrigin() = %q, %v", result, err)
	}
	details, err = credential.Details(ctx)
	if err != nil {
		t.Fatalf("Details(used) error = %v", err)
	}
	if details.LifecycleState != MetricsAccessLifecycleCurrent || details.NeverUsed || details.Stale ||
		details.LastUsedAt != "2026-06-01T14:00:00Z" || details.LastUsedOriginMasked != "203.0.113.x" {
		t.Fatalf("Details(used) = %+v", details)
	}
	if serialized := store.serialized(); strings.Contains(serialized, "accepted-token") || strings.Contains(serialized, "203.0.113.44") {
		t.Fatalf("persisted lifecycle contains bearer or full origin: %s", serialized)
	}

	now = now.Add(30 * time.Second)
	if result, err = credential.VerifyWithOrigin(ctx, "accepted-token", "203.0.113.44"); err != nil || result != MetricsAccessAccepted {
		t.Fatalf("VerifyWithOrigin(throttled) = %q, %v", result, err)
	}
	if store.updates != 1 {
		t.Fatalf("usage writes within throttle window = %d, want 1", store.updates)
	}
	now = now.Add(31 * time.Second)
	if result, err = credential.VerifyWithOrigin(ctx, "accepted-token", "203.0.113.44"); err != nil || result != MetricsAccessAccepted {
		t.Fatalf("VerifyWithOrigin(after throttle) = %q, %v", result, err)
	}
	if store.updates != 2 {
		t.Fatalf("usage writes after throttle window = %d, want 2", store.updates)
	}

	now = now.Add(DefaultMetricsCredentialStaleAfter)
	details, err = credential.Details(ctx)
	if err != nil {
		t.Fatalf("Details(stale) error = %v", err)
	}
	if details.LifecycleState != MetricsAccessLifecycleStale || !details.Stale || details.NeverUsed {
		t.Fatalf("Details(stale) = %+v", details)
	}

	now = now.Add(time.Hour)
	if _, err := credential.Rotate(ctx); err != nil {
		t.Fatalf("Rotate(existing) error = %v", err)
	}
	details, err = credential.Details(ctx)
	if err != nil {
		t.Fatalf("Details(rotated) error = %v", err)
	}
	if details.CreatedAt != "2026-06-01T12:00:00Z" || details.RotatedAt != "2026-07-01T15:01:01Z" ||
		details.LifecycleState != MetricsAccessLifecycleNeverUsed || details.LastUsedAt != "" ||
		details.LastUsedOriginMasked != "" {
		t.Fatalf("Details(rotated) = %+v", details)
	}
}

func TestSQLiteMetricsCredentialStoreLoadsLegacyHashAndPersistsLifecycle(t *testing.T) {
	db, _ := newTestDB(t, "metrics-legacy.db")
	if _, err := db.Exec("INSERT INTO settings(key, value) VALUES(?, ?)", DefaultMetricsTokenSettingKey, "legacy-hash"); err != nil {
		t.Fatalf("insert legacy hash: %v", err)
	}
	store := SQLiteMetricsCredentialStore{DB: func() *sql.DB { return db }}
	record, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load(legacy) error = %v", err)
	}
	if record.Hash != "legacy-hash" || record.Lifecycle != (MetricsCredentialLifecycle{}) {
		t.Fatalf("Load(legacy) = %+v", record)
	}
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	credential := NewMetricsAccessCredential(MetricsAccessCredentialDeps{
		Store: store,
		Now:   func() time.Time { return now },
		ComparePasswordAndHash: func(presented, hash string) (bool, error) {
			return presented == "legacy-token" && hash == "legacy-hash", nil
		},
	})
	details, err := credential.Details(context.Background())
	if err != nil {
		t.Fatalf("Details(legacy) error = %v", err)
	}
	if !details.Enabled || details.LifecycleState != MetricsAccessLifecycleUnknown ||
		details.NeverUsed || details.Stale || details.CreatedAt != "" {
		t.Fatalf("Details(legacy) = %+v, want enabled usage-unknown status", details)
	}
	if result, verifyErr := credential.VerifyWithOrigin(context.Background(), "legacy-token", "198.51.100.21"); verifyErr != nil || result != MetricsAccessAccepted {
		t.Fatalf("VerifyWithOrigin(legacy) = %q, %v", result, verifyErr)
	}
	reloaded := NewMetricsAccessCredential(MetricsAccessCredentialDeps{Store: store, Now: func() time.Time { return now }})
	details, err = reloaded.Details(context.Background())
	if err != nil {
		t.Fatalf("Details(migrated legacy) error = %v", err)
	}
	if details.LifecycleState != MetricsAccessLifecycleCurrent || details.CreatedAt != "" ||
		details.LastUsedAt != "2026-07-01T12:00:00Z" || details.LastUsedOriginMasked != "198.51.100.x" {
		t.Fatalf("Details(migrated legacy) = %+v", details)
	}
}

func TestMaskMetricsCredentialOrigin(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		want   string
	}{
		{name: "IPv4", origin: "203.0.113.44", want: "203.0.113.x"},
		{name: "IPv6", origin: "2001:db8:abcd:12:1111:2222:3333:4444", want: "2001:db8:abcd:12::/64"},
		{name: "already masked IPv4", origin: "198.51.100.x", want: "198.51.100.x"},
		{name: "unknown", origin: "", want: "unknown"},
		{name: "hostname rejected", origin: "scraper.internal", want: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := maskMetricsCredentialOrigin(test.origin); got != test.want {
				t.Fatalf("maskMetricsCredentialOrigin(%q) = %q, want %q", test.origin, got, test.want)
			}
		})
	}
}

type credentialStoreStub struct {
	hash       string
	lifecycle  MetricsCredentialLifecycle
	loadErr    error
	replaceErr error
	updateErr  error
	deleteErr  error
	loads      int
	replaces   int
	updates    int
	deletes    int
}

func (s *credentialStoreStub) Load(context.Context) (MetricsCredentialRecord, error) {
	s.loads++
	return MetricsCredentialRecord{Hash: s.hash, Lifecycle: s.lifecycle}, s.loadErr
}

func (s *credentialStoreStub) Replace(_ context.Context, record MetricsCredentialRecord) error {
	s.replaces++
	if s.replaceErr == nil {
		s.hash = record.Hash
		s.lifecycle = record.Lifecycle
	}
	return s.replaceErr
}

func (s *credentialStoreStub) UpdateLifecycle(_ context.Context, lifecycle MetricsCredentialLifecycle) error {
	s.updates++
	if s.updateErr == nil {
		s.lifecycle = lifecycle
	}
	return s.updateErr
}

func (s *credentialStoreStub) Delete(context.Context) error {
	s.deletes++
	if s.deleteErr == nil {
		s.hash = ""
		s.lifecycle = MetricsCredentialLifecycle{}
	}
	return s.deleteErr
}

func (s *credentialStoreStub) serialized() string {
	return s.hash + "|" + s.lifecycle.CreatedAt + "|" + s.lifecycle.RotatedAt + "|" +
		s.lifecycle.LastUsedAt + "|" + s.lifecycle.LastUsedOriginMasked
}
