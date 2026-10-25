package jobs

import (
	"encoding/json"
	"errors"
	"testing"
)

type racingMetadataRepo struct {
	Repository
	race bool
}

func (r *racingMetadataRepo) ApplyTransition(record Record, revision int64, activeOnly bool) (bool, error) {
	if !r.race {
		r.race = true
		current, err := r.Repository.Get(record.ID)
		if err != nil {
			return false, err
		}
		current.MetaJSON = `{"scheduler":"new","approval_generation":2}`
		current.Revision++
		if err = r.Repository.Upsert(current); err != nil {
			return false, err
		}
		return false, nil
	}
	return r.Repository.ApplyTransition(record, revision, activeOnly)
}
func TestMergeActiveMetadataPreservesConcurrentFacts(t *testing.T) {
	db := openJobTestDB(t)
	repo := &racingMetadataRepo{Repository: NewSQLiteRepository(db)}
	m := NewManager(repo, ManagerOptions{})
	job, err := m.CreateJob(CreateParams{Kind: KindUpdate, Status: StatusRunning, MetaJSON: `{"scheduler":"old"}`})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.MergeActiveMetadata(job.ID, map[string]any{"application_postcheck_profile": map[string]any{"id": "web", "revision": 3}}); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]json.RawMessage
	if err = json.Unmarshal([]byte(got.MetaJSON), &meta); err != nil {
		t.Fatal(err)
	}
	if string(meta["scheduler"]) != `"new"` || string(meta["approval_generation"]) != "2" || meta["application_postcheck_profile"] == nil {
		t.Fatal(got.MetaJSON)
	}
	if err = m.Transition(job.ID, Intent{Kind: IntentSucceed}); err != nil {
		t.Fatal(err)
	}
	if err = m.MergeActiveMetadata(job.ID, map[string]any{"late": true}); !errors.Is(err, ErrTransitionConflict) {
		t.Fatal(err)
	}
	var unavailable *Manager
	if err = unavailable.MergeActiveMetadata(job.ID, nil); err == nil {
		t.Fatal("nil manager")
	}
}
