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
