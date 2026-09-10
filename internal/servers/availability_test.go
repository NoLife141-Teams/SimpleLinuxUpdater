package servers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestServerAvailabilityPreservesInventoryAndRollsBackFailures(t *testing.T) {
	original := Server{Name: "srv", Host: "host", User: "root", Port: 22, Pass: "secret", Key: "private-key", Tags: []string{"prod"}}
	repo := &fakeRepo{}
	svc, state, _, statuses := newTestService(repo, []Server{original})
	(*statuses)[original.Name].Logs = "previous maintenance"
	commands := NewCommandService(svc)
	result := commands.SetServerDisabled(original.Name, true)
	if !result.Succeeded() || result.Audit.Action != "server.disable" || result.Audit.Meta["disabled"] != true {
		t.Fatalf("disable result = %+v", result)
	}
	want := original
	want.Disabled = true
	if !reflect.DeepEqual(*result.Server, want) || !reflect.DeepEqual(repo.saved, []Server{want}) {
		t.Fatalf("disabled server = %+v, saved = %+v", result.Server, repo.saved)
	}
	if got := state.ListStatuses()[0]; !got.Disabled || got.Logs != "previous maintenance" {
		t.Fatalf("status = %+v", got)
	}
	encoded, err := json.Marshal(result.Server)
	if err != nil || strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "private-key") || !strings.Contains(string(encoded), `"disabled":true`) {
		t.Fatalf("unsafe or incomplete response: %s, %v", encoded, err)
	}
	updated, err := svc.Update(original.Name, Server{Name: "renamed", Host: "host", User: "root"})
	if err != nil || !updated.Disabled || updated.Pass != original.Pass || updated.Key != original.Key {
		t.Fatalf("ordinary edit = %+v, %v", updated, err)
	}
	repo.saveErr = errors.New("disk unavailable")
	if result := commands.SetServerDisabled(updated.Name, false); result.Succeeded() {
		t.Fatal("failed persistence accepted")
	}
	if got, _ := state.FindByName(updated.Name); !got.Disabled || !state.CurrentStatusSnapshot(updated.Name).Disabled {
		t.Fatal("failed save changed runtime availability")
	}
	repo.saveErr = nil
	result = commands.SetServerDisabled(updated.Name, false)
	if !result.Succeeded() || result.Server.Disabled || result.Audit.Action != "server.enable" {
		t.Fatalf("enable result = %+v", result)
	}
}

func TestServerAvailabilityAdmissionAndActiveOperations(t *testing.T) {
	for _, activeStatus := range []string{"updating", "pending_approval", "facts_refresh", "rebooting", "done"} {
		t.Run(activeStatus, func(t *testing.T) {
			svc, _, _, statuses := newTestService(&fakeRepo{}, []Server{{Name: "srv"}})
			(*statuses)["srv"].Status = activeStatus
			(*statuses)["srv"].ActionRunning = activeStatus == "done"
			if _, err := svc.SetDisabled("srv", true); !errors.Is(err, ErrActionInProgress) {
				t.Fatalf("disable during %s = %v", activeStatus, err)
			}
		})
	}
	for _, transient := range []bool{false, true} {
		svc, state, _, _ := newTestService(&fakeRepo{}, []Server{{Name: "srv", Disabled: true}})
		before := state.CurrentStatusSnapshot("srv")
		admit := func() error {
			if transient {
				_, _, err := state.BeginTransientAction("srv", "facts_refresh")
				return err
			}
			_, err := state.BeginPackageMutation("srv", "updating")
			return err
		}
		if err := admit(); !errors.Is(err, ErrDisabled) || !reflect.DeepEqual(before, state.CurrentStatusSnapshot("srv")) {
			t.Fatalf("disabled admission = %v, status = %+v", err, state.CurrentStatusSnapshot("srv"))
		}
		if _, err := svc.SetDisabled("srv", false); err != nil {
			t.Fatal(err)
		}
		if err := admit(); err != nil {
			t.Fatalf("admission after enable = %v", err)
		}
	}
}

func TestServerAvailabilityMigrationAndPersistence(t *testing.T) {
	db := openSchemaTestDB(t, "legacy-availability.db")
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE servers (name TEXT PRIMARY KEY, host TEXT NOT NULL, user TEXT NOT NULL, pass_enc TEXT NOT NULL);
		INSERT INTO servers VALUES ('legacy', 'host', 'root', 'password')`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	repo := SQLiteRepository{DB: func() *sql.DB { return db }}
	loaded, err := repo.Load()
	if err != nil || len(loaded) != 1 || loaded[0].Disabled {
		t.Fatalf("legacy availability = %+v, %v", loaded, err)
	}
	loaded[0].Disabled = true
	if err := repo.Save(loaded, nil); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	reloaded, err := repo.Load()
	if err != nil || !reflect.DeepEqual(loaded, reloaded) {
		t.Fatalf("reloaded = %+v, %v; want %+v", reloaded, err, loaded)
	}
}

func TestServerAvailabilitySerializesWithActionAdmission(t *testing.T) {
	for i := 0; i < 50; i++ {
		svc, state, _, _ := newTestService(&fakeRepo{}, []Server{{Name: "srv"}})
		start := make(chan struct{})
		disabled := make(chan error, 1)
		admitted := make(chan error, 1)
		go func() { <-start; _, err := svc.SetDisabled("srv", true); disabled <- err }()
		go func() { <-start; _, err := state.BeginAction("srv", "updating"); admitted <- err }()
		close(start)
		disableErr, admitErr := <-disabled, <-admitted
		if !((disableErr == nil && errors.Is(admitErr, ErrDisabled)) || (admitErr == nil && errors.Is(disableErr, ErrActionInProgress))) {
			t.Fatalf("disable = %v, admission = %v; exactly one must succeed", disableErr, admitErr)
		}
	}
}
