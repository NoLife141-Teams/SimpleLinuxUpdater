package apptime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
)

type gatedConfigureStore struct {
	mu       sync.Mutex
	value    string
	savedA   chan struct{}
	releaseA chan struct{}
}

func (s *gatedConfigureStore) Load(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value, nil
}
func (s *gatedConfigureStore) Save(_ context.Context, value string) error {
	s.mu.Lock()
	s.value = value
	s.mu.Unlock()
	if value == "Europe/London" {
		close(s.savedA)
		<-s.releaseA
	}
	return nil
}
func TestConcurrentConfigureKeepsPersistedAndPublishedTimezonesAligned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		store := &gatedConfigureStore{value: "UTC", savedA: make(chan struct{}), releaseA: make(chan struct{})}
		module := New(Deps{Store: store})
		if err := module.Initialize(ctx); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 2)
		go func() { _, err := module.Configure(ctx, "Europe/London"); done <- err }()
		<-store.savedA
		go func() { _, err := module.Configure(ctx, "America/Toronto"); done <- err }()
		synctest.Wait()
		close(store.releaseA)
		for i := 0; i < 2; i++ {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		persisted, _ := store.Load(ctx)
		if live := module.Current().ResolvedName; live != persisted {
			t.Fatalf("persisted=%s, published=%s", persisted, live)
		}
	})
}

func TestQueuedConfigureCancellationDoesNotPersist(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &gatedConfigureStore{value: "UTC", savedA: make(chan struct{}), releaseA: make(chan struct{})}
		module := New(Deps{Store: store})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := module.Configure(context.Background(), "Europe/London"); done <- err }()
		<-store.savedA
		cancelled := make(chan error, 1)
		go func() { _, err := module.Configure(ctx, "America/Toronto"); cancelled <- err }()
		synctest.Wait()
		cancel()
		if err := <-cancelled; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled configure: %v", err)
		}
		close(store.releaseA)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		persisted, _ := store.Load(context.Background())
		if persisted != "Europe/London" || module.Current().ResolvedName != persisted {
			t.Fatalf("cancelled request affected timezone: %s", persisted)
		}
	})
}
