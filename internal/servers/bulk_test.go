package servers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func bulkFixture(t *testing.T) (*BulkCommands, *fakeRepo, *State) {
	t.Helper()
	repo := &fakeRepo{}
	svc, state, _, _ := newTestService(repo, []Server{{Name: "a", Host: "a.invalid", Port: 22, User: "root", Pass: "never-export-password", Key: "never-export-key", Tags: []string{"old"}, Disabled: true}, {Name: "b", Host: "b.invalid", Port: 22, User: "updater", Tags: []string{"old"}}})
	bulk := NewBulkCommands(NewCommandService(svc), BulkDeps{Revision: func() (int64, error) { return 7, nil }, CheckRevision: func(*sql.Tx, int64) error { return nil }, Project: func(list []Server) (map[string]json.RawMessage, error) { return map[string]json.RawMessage{}, nil }})
	return bulk, repo, state
}
func TestBulkInventoryPreservesConfigurationAndAdmitsConcurrentConfirmationOnce(t *testing.T) {
	bulk, repo, state := bulkFixture(t)
	preview, err := bulk.Preview(BulkDraft{Names: []string{"a", "b"}, AddTags: []string{" production ", "production"}, RemoveTags: []string{"old"}})
	if err != nil || !preview.CanConfirm {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	before := state.CloneServers()
	results := make(chan BulkResult, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { results <- bulk.Commit(preview.BatchID) })
	}
	wg.Wait()
	close(results)
	successes := 0
	for result := range results {
		if result.Outcome == CommandOutcomeSuccess {
			successes++
		}
	}
	if successes != 1 || repo.saveCalls != 1 {
		t.Fatalf("successes=%d saves=%d", successes, repo.saveCalls)
	}
	after := state.CloneServers()
	for i := range after {
		expected := before[i]
		expected.Tags = []string{"production"}
		if !reflect.DeepEqual(after[i], expected) {
			t.Fatalf("changed non-tag configuration: %+v", after[i])
		}
	}
	raw, _ := json.Marshal(preview)
	if containsSecret(string(raw)) {
		t.Fatal("preview exposed credential")
	}
}
func containsSecret(s string) bool {
	for _, secret := range []string{"never-export-password", "never-export-key"} {
		for i := 0; i+len(secret) <= len(s); i++ {
			if s[i:i+len(secret)] == secret {
				return true
			}
		}
	}
	return false
}
func TestBulkInventoryRejectsWholeSelectionAndConsumesConflictingPreview(t *testing.T) {
	for _, scenario := range []string{"busy", "pending approval", "missing", "concurrent edit", "policy change", "save failure", "transaction conflict"} {
		t.Run(scenario, func(t *testing.T) {
			bulk, repo, state := bulkFixture(t)
			disabled := true
			preview, err := bulk.Preview(BulkDraft{Names: []string{"a", "b"}, Disabled: &disabled})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "busy":
				state.StatusMap()["b"].ActionRunning = true
			case "pending approval":
				state.StatusMap()["b"].Status = "pending_approval"
				state.StatusMap()["b"].ActionRunning = false
			case "missing":
				state.SetServers(state.Servers()[:1])
			case "concurrent edit":
				state.Servers()[1].User = "another-user"
			case "policy change":
				bulk.deps.Revision = func() (int64, error) { return 8, nil }
			case "save failure":
				repo.saveErr = errors.New("commit failure")
			case "transaction conflict":
				bulk.deps.CheckRevision = func(*sql.Tx, int64) error { return ErrBulkConflict }
			}
			before := state.CloneServers()
			statuses := state.CloneStatusMap()
			result := bulk.Commit(preview.BatchID)
			if result.Outcome == CommandOutcomeSuccess {
				t.Fatal("accepted stale/busy/failed batch")
			}
			if !reflect.DeepEqual(before, state.Servers()) || !reflect.DeepEqual(statuses, state.CloneStatusMap()) {
				t.Fatal("partial runtime publication")
			}
			if len(repo.saved) != 0 {
				t.Fatal("partial persistence")
			}
			if bulk.Commit(preview.BatchID).Outcome != CommandOutcomeConflict {
				t.Fatal("replayed consumed preview")
			}
		})
	}
}
func TestBulkInventoryStrictScopeAndLimits(t *testing.T) {
	bulk, _, _ := bulkFixture(t)
	for _, draft := range []BulkDraft{{}, {Names: []string{"a", "a"}, AddTags: []string{"x"}}, {Names: make([]string, 1001), AddTags: []string{"x"}}, {Names: []string{"a"}, AddTags: []string{"x"}, RemoveTags: []string{"x"}}} {
		if _, err := bulk.Preview(draft); err == nil {
			t.Fatalf("accepted invalid draft %+v", draft)
		}
	}
}

func TestBulkInventoryRejectsPreviewAcrossRestoredPersistence(t *testing.T) {
	bulk, repo, _ := bulkFixture(t)
	epoch := "original"
	bulk.deps.Epoch = func() string { return epoch }
	p, err := bulk.Preview(BulkDraft{Names: []string{"a"}, AddTags: []string{"new"}})
	if err != nil {
		t.Fatal(err)
	}
	epoch = "restored"
	if result := bulk.Commit(p.BatchID); result.Outcome != CommandOutcomeConflict || repo.saveCalls != 0 {
		t.Fatal("accepted preview from previous persistence instance")
	}
}
