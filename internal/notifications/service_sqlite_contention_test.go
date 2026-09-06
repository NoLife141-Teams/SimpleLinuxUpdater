package notifications

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"
)

func TestNotificationDeliveryLifecycleWaitsForExternalSQLiteContention(t *testing.T) {
	svc, _, db := newTestServiceWithQueueAndDB(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}, defaultQueueSize)

	var seq int
	var name, dbPath string
	if err := db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &dbPath); err != nil {
		t.Fatalf("resolve SQLite database path: %v", err)
	}
	if dbPath == "" {
		t.Fatal("SQLite database path is empty")
	}

	lockDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open independent SQLite lock database: %v", err)
	}
	lockDB.SetMaxOpenConns(1)
	lockDB.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = lockDB.Close() })
	if _, err := lockDB.Exec("PRAGMA busy_timeout=5000"); err != nil {
		t.Fatalf("set lock database busy timeout: %v", err)
	}

	lockConn, err := lockDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("reserve independent SQLite lock connection: %v", err)
	}
	locked := false
	defer func() {
		if locked {
			_, _ = lockConn.ExecContext(context.Background(), "ROLLBACK")
		}
		_ = lockConn.Close()
	}()
	if _, err := lockConn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("acquire external SQLite lock: %v", err)
	}
	locked = true

	admission := make(chan Admission, 1)
	go func() {
		admission <- svc.Accept(DeliveryIntent{
			Action:     EventUpdateComplete,
			TargetName: "srv-current",
			Status:     "success",
			MetaJSON:   `{"upgrade_completed":true,"approved_package_count":3}`,
		})
	}()

	select {
	case got := <-admission:
		t.Fatalf("Accept() completed while a separate SQLite connection held an exclusive lock: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}

	if _, err := lockConn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatalf("release external SQLite lock: %v", err)
	}
	locked = false

	select {
	case got := <-admission:
		if got.State != AdmissionAdmitted {
			t.Fatalf("Accept() = %+v, want admitted after external SQLite contention cleared", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Accept() did not resume after external SQLite contention cleared")
	}
}
