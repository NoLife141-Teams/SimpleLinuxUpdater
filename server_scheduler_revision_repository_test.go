package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	policypkg "debian-updater/internal/policies"
	serverpkg "debian-updater/internal/servers"
)

func TestSchedulerRevisionServerRepositoryTracksOnlyMatchingState(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "scheduler-server-revision.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
	if err := serverpkg.EnsureSchema(db); err != nil {
		t.Fatalf("servers.EnsureSchema() error = %v", err)
	}
	if err := policypkg.EnsureSchema(db); err != nil {
		t.Fatalf("policies.EnsureSchema() error = %v", err)
	}
	policyRepo := policypkg.NewSQLiteRepository(policypkg.SQLiteRepositoryDeps{DB: func() *sql.DB { return db }})
	initialRevision, err := policyRepo.LoadSchedulerStateRevision()
	if err != nil {
		t.Fatalf("LoadSchedulerStateRevision() error = %v", err)
	}

	repository := newSchedulerRevisionServerRepository(func() *sql.DB { return db }, nil, nil)
	servers := []serverpkg.Server{{
		Name: "srv-a", Host: "10.0.0.10", Port: 22, User: "root", Key: "key-v1", Tags: []string{"prod", "web"},
	}}
	if err := repository.Save(servers, nil); err != nil {
		t.Fatalf("initial Save() error = %v", err)
	}
	afterCreate, err := policyRepo.LoadSchedulerStateRevision()
	if err != nil {
		t.Fatalf("revision after create: %v", err)
	}
	if afterCreate <= initialRevision {
		t.Fatalf("revision after matching-state create = %d, want > %d", afterCreate, initialRevision)
	}

	credentialOnly := append([]serverpkg.Server(nil), servers...)
	credentialOnly[0].Host = "10.0.0.11"
	credentialOnly[0].Key = "key-v2"
	if err := repository.Save(credentialOnly, nil); err != nil {
		t.Fatalf("credential/host-only Save() error = %v", err)
	}
	afterNonMatchingSave, err := policyRepo.LoadSchedulerStateRevision()
	if err != nil {
		t.Fatalf("revision after non-matching save: %v", err)
	}
	if afterNonMatchingSave != afterCreate {
		t.Fatalf("revision after host/key-only save = %d, want unchanged %d", afterNonMatchingSave, afterCreate)
	}

	if err := repository.UpdateServerKey("srv-a", "key-v3"); err != nil {
		t.Fatalf("UpdateServerKey() error = %v", err)
	}
	afterKeyRotation, err := policyRepo.LoadSchedulerStateRevision()
	if err != nil {
		t.Fatalf("revision after key rotation: %v", err)
	}
	if afterKeyRotation != afterCreate {
		t.Fatalf("revision after key rotation = %d, want unchanged %d", afterKeyRotation, afterCreate)
	}

	credentialOnly[0].Disabled = true
	if err := repository.Save(credentialOnly, nil); err != nil {
		t.Fatal(err)
	}
	afterDisable, err := policyRepo.LoadSchedulerStateRevision()
	if err != nil || afterDisable <= afterKeyRotation {
		t.Fatalf("disable revision = %d, %v", afterDisable, err)
	}
	if err := repository.Save(credentialOnly, nil); err != nil {
		t.Fatal(err)
	}
	unchanged, err := policyRepo.LoadSchedulerStateRevision()
	if err != nil || unchanged != afterDisable {
		t.Fatalf("unchanged availability revision = %d, %v", unchanged, err)
	}
	if _, err := db.Exec("UPDATE servers SET disabled = 0 WHERE name = 'srv-a'"); err != nil {
		t.Fatal(err)
	}
	afterEnable, err := policyRepo.LoadSchedulerStateRevision()
	if err != nil || afterEnable <= afterDisable {
		t.Fatalf("direct availability update revision = %d, %v", afterEnable, err)
	}

	tagChanged := append([]serverpkg.Server(nil), credentialOnly...)
	tagChanged[0].Tags = []string{"prod", "db"}
	if err := repository.Save(tagChanged, nil); err != nil {
		t.Fatalf("tag-changing Save() error = %v", err)
	}
	afterTagChange, err := policyRepo.LoadSchedulerStateRevision()
	if err != nil {
		t.Fatalf("revision after tag change: %v", err)
	}
	if afterTagChange <= afterKeyRotation {
		t.Fatalf("revision after tag change = %d, want > %d", afterTagChange, afterKeyRotation)
	}
}
