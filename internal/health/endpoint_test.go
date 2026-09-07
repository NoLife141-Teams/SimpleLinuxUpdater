package health

import (
	"context"
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestEndpointInvalidationPreservesHistoryAndRejectsLateFacts(t *testing.T) {
	db, repo := openServerFactsTestRepository(t, "endpoint.db")
	now := time.Now().UTC()
	repo.Now = func() time.Time { return now }
	old := CollectedFacts{ServerName: "host", CollectedAt: now.Add(-time.Minute).Format(time.RFC3339Nano), OSPrettyName: "old", DiskStatus: "ok", AptStatus: "ok"}
	if err := repo.AcceptCollectedFacts(old); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.InvalidateEndpointTx(tx, "host", "192.0.2.1:22", "192.0.2.2:22"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	latest, err := repo.LatestObservations("host")
	if err != nil || len(latest) != 0 {
		t.Fatalf("old observations remain current: %+v / %v", latest, err)
	}
	if err := repo.AcceptCollectedFacts(old); err == nil {
		t.Fatal("late facts restored old endpoint health")
	}
	history, err := repo.History(now.Add(-time.Hour).Format(time.RFC3339Nano), now.Add(time.Hour).Format(time.RFC3339Nano), "host")
	if err != nil || len(history) != 1 || history[0].Endpoint != "192.0.2.1:22" {
		t.Fatalf("history=%+v / %v", history, err)
	}
	newFacts := old
	newFacts.CollectedAt = now.Add(time.Second).Format(time.RFC3339Nano)
	newFacts.OSPrettyName = "new"
	if err := repo.AcceptCollectedFacts(newFacts); err != nil {
		t.Fatal(err)
	}
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RenameServerTx(tx, "host", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	latest, err = repo.LatestObservations("renamed")
	if err != nil || latest["renamed"].OSPrettyName != "new" {
		t.Fatalf("rename lost current health: %+v / %v", latest, err)
	}
}

func TestEndpointChangeClearsAutomaticRefreshBackoff(t *testing.T) {
	server := servers.Server{Name: "host", Host: "192.0.2.1", Port: 22}
	attempts := 0
	worker := NewRefreshWorker(RefreshWorkerDeps{
		SnapshotServers: func() []servers.Server { return []servers.Server{server} },
		LatestFacts:     func() (map[string]CollectedFacts, error) { return nil, nil },
		Refresh: func(context.Context, servers.Server) RefreshAttempt {
			attempts++
			return RefreshAttempt{State: RefreshAttemptFailed}
		},
	}, RefreshWorkerOptions{})
	worker.RunOnce(context.Background())
	worker.RunOnce(context.Background())
	if attempts != 1 {
		t.Fatalf("backoff not applied: %d", attempts)
	}
	server.Host = "192.0.2.2"
	worker.RunOnce(context.Background())
	if attempts != 2 {
		t.Fatalf("replacement endpoint inherited backoff: %d", attempts)
	}
}
