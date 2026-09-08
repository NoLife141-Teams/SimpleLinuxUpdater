package notifications

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestCloseWhilePersistenceIsPausedForFailedRestore(t *testing.T) {
	svc, _, _ := newTestServiceWithQueueAndDB(t, func(http.ResponseWriter, *http.Request) { t.Error("delivery during paused recovery") }, 1)
	if err := svc.PreparePersistenceReplacement(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("shutdown waited for persistence to resume: %v", err)
	}
	svc.persistenceMu.Lock()
	defer svc.persistenceMu.Unlock()
	if !svc.persistencePaused {
		t.Fatal("shutdown reopened persistence during incomplete recovery")
	}
}

func TestPreparationFailureResumesOriginalNotificationPersistence(t *testing.T) {
	for _, failure := range []string{"pause_cancelled", "outbox_load_failed"} {
		t.Run(failure, func(t *testing.T) {
			svc, _, db := newTestServiceWithQueueAndDB(t, func(http.ResponseWriter, *http.Request) {}, 1)
			ctx := context.Background()
			if failure == "pause_cancelled" {
				if !svc.beginPersistence(ctx) {
					t.Fatal("failed to admit initial persistence work")
				}
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				defer svc.endPersistence()
			} else {
				if _, err := db.Exec("ALTER TABLE notification_outbox RENAME TO unavailable_outbox"); err != nil {
					t.Fatal(err)
				}
			}
			if err := svc.PreparePersistenceReplacement(ctx); err == nil {
				t.Fatal("expected preparation failure")
			}
			svc.persistenceMu.Lock()
			paused, prepared := svc.persistencePaused, svc.replacementRows != nil
			svc.persistenceMu.Unlock()
			if paused || prepared {
				t.Fatalf("failed preparation left persistence paused=%t prepared=%t", paused, prepared)
			}
			probeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if !svc.beginPersistence(probeCtx) {
				t.Fatal("failed preparation blocked subsequent persistence work")
			}
			svc.endPersistence()
		})
	}
}
