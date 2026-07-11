package observability

import (
	"context"
	"errors"
	"testing"
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

type credentialStoreStub struct {
	hash       string
	loadErr    error
	replaceErr error
	deleteErr  error
	loads      int
	replaces   int
	deletes    int
}

func (s *credentialStoreStub) Load(context.Context) (string, error) {
	s.loads++
	return s.hash, s.loadErr
}

func (s *credentialStoreStub) Replace(_ context.Context, hash string) error {
	s.replaces++
	if s.replaceErr == nil {
		s.hash = hash
	}
	return s.replaceErr
}

func (s *credentialStoreStub) Delete(context.Context) error {
	s.deletes++
	if s.deleteErr == nil {
		s.hash = ""
	}
	return s.deleteErr
}
