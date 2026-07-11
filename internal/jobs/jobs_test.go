package jobs

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestJobManagerCreateJobDefaultsAndTrims(t *testing.T) {
	db := openJobTestDB(t)
	var notifications []string
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	idSeq := 0
	manager := NewManager(NewSQLiteRepository(db), ManagerOptions{
		Notify: func(reason string) {
			notifications = append(notifications, reason)
		},
		Now: func() time.Time { return now },
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("job-create-%d", idSeq)
		},
	})

	record, err := manager.CreateJob(CreateParams{
		Kind:     " update ",
		Actor:    " ",
		ClientIP: strings.Repeat("1", 150),
		Status:   " ",
	})
	if err != nil {
		t.Fatalf("CreateJob() error = %v", err)
	}
	if record.ID != "job-create-1" || record.Kind != KindUpdate || record.Actor != "unknown" || record.Status != StatusQueued {
		t.Fatalf("record defaults/trimming = %+v", record)
	}
	if len([]rune(record.ClientIP)) != 128 {
		t.Fatalf("ClientIP len = %d, want 128", len([]rune(record.ClientIP)))
	}
	if record.RetryPolicyJSON != "{}" || record.MetaJSON != "{}" {
		t.Fatalf("json defaults = retry %q meta %q, want {}", record.RetryPolicyJSON, record.MetaJSON)
	}
	if record.CreatedAt != "2026-05-17T12:00:00.000000000Z" || record.UpdatedAt != record.CreatedAt {
		t.Fatalf("timestamps = created %q updated %q", record.CreatedAt, record.UpdatedAt)
	}
	if !reflect.DeepEqual(notifications, []string{"job.create"}) {
		t.Fatalf("notifications = %v, want job.create", notifications)
	}

}

func TestJobStateTransitionOwnsRevisionAndLifecycleTimestamps(t *testing.T) {
	db := openJobTestDB(t)
	times := []time.Time{
		time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 17, 12, 1, 0, 0, time.UTC),
		time.Date(2026, 5, 17, 12, 2, 0, 0, time.UTC),
	}
	next := 0
	manager := NewManager(NewSQLiteRepository(db), ManagerOptions{
		Now: func() time.Time {
			value := times[next]
			next++
			return value
		},
		NewID: func() string { return "transition-job" },
	})

	job, err := manager.CreateJob(CreateParams{Kind: KindUpdate, Actor: "admin"})
	if err != nil {
		t.Fatalf("CreateJob() error = %v", err)
	}
	if err := manager.Transition(job.ID, Intent{Kind: IntentStart, Phase: stringPointer(PhaseDial)}); err != nil {
		t.Fatalf("Transition(Start) error = %v", err)
	}
	running, err := manager.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob(running) error = %v", err)
	}
	if running.Status != StatusRunning || running.StartedAt != FormatTimestamp(times[1]) || running.Revision != 1 {
		t.Fatalf("running transition = %+v", running)
	}

	if err := manager.Transition(job.ID, Intent{Kind: IntentSucceed}); err != nil {
		t.Fatalf("Transition(Succeed) error = %v", err)
	}
	finished, err := manager.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob(finished) error = %v", err)
	}
	if finished.Status != StatusSucceeded || finished.Phase != PhaseComplete || finished.FinishedAt != FormatTimestamp(times[2]) || finished.Revision != 2 {
		t.Fatalf("finished transition = %+v", finished)
	}
}

func TestJobStateTransitionRejectsStaleRevision(t *testing.T) {
	db := openJobTestDB(t)
	repo := NewSQLiteRepository(db)
	manager := NewManager(repo, ManagerOptions{NewID: func() string { return "stale-job" }})
	job, err := manager.CreateJob(CreateParams{Kind: KindUpdate, Actor: "admin"})
	if err != nil {
		t.Fatalf("CreateJob() error = %v", err)
	}
	stale := job
	stale.Status = StatusRunning
	stale.Revision = 1
	updated, err := repo.ApplyTransition(stale, job.Revision+1, false)
	if err != nil {
		t.Fatalf("ApplyTransition(stale) error = %v", err)
	}
	if updated {
		t.Fatal("ApplyTransition(stale) updated = true, want false")
	}
}

func TestEnsureSchemaAddsRevisionToExistingJobsTable(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy-jobs.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE jobs (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create legacy jobs table: %v", err)
	}
	if err := ensureRevisionColumn(db); err != nil {
		t.Fatalf("ensureRevisionColumn() error = %v", err)
	}
	var revision int64
	if err := db.QueryRow("SELECT revision FROM jobs LIMIT 1").Scan(&revision); err != sql.ErrNoRows {
		t.Fatalf("revision query error = %v, want sql.ErrNoRows", err)
	}
}

func stringPointer(value string) *string { return &value }

func TestJobManagerUpsertSyncsRuntimeAndNotifies(t *testing.T) {
	db := openJobTestDB(t)
	var synced []Record
	var notifications []string
	manager := NewManager(NewSQLiteRepository(db), ManagerOptions{
		SyncRuntime: func(record Record) {
			synced = append(synced, record)
		},
		Notify: func(reason string) {
			notifications = append(notifications, reason)
		},
		Now:   func() time.Time { return time.Date(2026, 5, 17, 13, 0, 0, 0, time.UTC) },
		NewID: func() string { return "job-upsert" },
	})

	if err := manager.ImportJobRecord(Record{
		Kind:       KindAutoremove,
		ServerName: "srv-upsert",
		Status:     StatusSucceeded,
		Phase:      PhaseComplete,
	}); err != nil {
		t.Fatalf("ImportJobRecord() error = %v", err)
	}
	record, err := manager.GetJob("job-upsert")
	if err != nil {
		t.Fatalf("GetJob() error = %v", err)
	}
	if record.Actor != "unknown" || record.RetryPolicyJSON != "{}" || record.MetaJSON != "{}" || record.CreatedAt == "" || record.UpdatedAt == "" {
		t.Fatalf("upserted defaults incomplete: %+v", record)
	}
	if len(synced) != 1 || synced[0].ID != "job-upsert" {
		t.Fatalf("synced records = %+v, want job-upsert once", synced)
	}
	if !reflect.DeepEqual(notifications, []string{"job.upsert"}) {
		t.Fatalf("notifications = %v, want job.upsert", notifications)
	}
}

func TestJobManagerTransitionActiveCompareAndSet(t *testing.T) {
	db := openJobTestDB(t)
	var notifications []string
	var synced []Record
	manager := NewManager(NewSQLiteRepository(db), ManagerOptions{
		Notify: func(reason string) {
			notifications = append(notifications, reason)
		},
		SyncRuntime: func(record Record) {
			synced = append(synced, record)
		},
		Now: func() time.Time { return time.Date(2026, 5, 17, 14, 0, 0, 0, time.UTC) },
	})
	active, err := manager.CreateJob(CreateParams{Kind: KindUpdate, ServerName: "srv", Actor: "admin", Status: StatusQueued})
	if err != nil {
		t.Fatalf("CreateJob(active) error = %v", err)
	}
	done, err := manager.CreateJob(CreateParams{Kind: KindUpdate, ServerName: "srv", Actor: "admin", Status: StatusSucceeded})
	if err != nil {
		t.Fatalf("CreateJob(done) error = %v", err)
	}
	notifications = nil
	synced = nil

	status := StatusRunning
	updated, err := manager.TransitionActive(active.ID, Intent{Status: &status})
	if err != nil {
		t.Fatalf("TransitionActive(active) error = %v", err)
	}
	if !updated {
		t.Fatalf("TransitionActive(active) updated = false, want true")
	}
	updated, err = manager.TransitionActive(done.ID, Intent{Status: &status})
	if err != nil {
		t.Fatalf("TransitionActive(done) error = %v", err)
	}
	if updated {
		t.Fatalf("TransitionActive(done) updated = true, want false")
	}
	if !reflect.DeepEqual(notifications, []string{"job.update"}) {
		t.Fatalf("notifications = %v, want one job.update", notifications)
	}
	if len(synced) != 1 || synced[0].ID != active.ID || synced[0].Status != StatusRunning {
		t.Fatalf("synced records = %+v, want active running once", synced)
	}
}

func TestJobManagerLookupLatestActiveJob(t *testing.T) {
	db := openJobTestDB(t)
	manager := NewManager(NewSQLiteRepository(db), ManagerOptions{
		Now: func() time.Time { return time.Date(2026, 5, 17, 15, 0, 0, 0, time.UTC) },
	})
	older := Record{ID: "older", Kind: KindUpdate, ServerName: "srv", Actor: "admin", Status: StatusQueued, CreatedAt: "2026-05-17T14:00:00.000000000Z", UpdatedAt: "2026-05-17T14:00:00.000000000Z", RetryPolicyJSON: "{}", MetaJSON: "{}"}
	newer := Record{ID: "newer", Kind: KindUpdate, ServerName: "srv", Actor: "admin", Status: StatusWaitingApproval, CreatedAt: "2026-05-17T15:00:00.000000000Z", UpdatedAt: "2026-05-17T15:00:00.000000000Z", RetryPolicyJSON: "{}", MetaJSON: "{}"}
	finished := Record{ID: "finished", Kind: KindUpdate, ServerName: "srv", Actor: "admin", Status: StatusSucceeded, CreatedAt: "2026-05-17T16:00:00.000000000Z", UpdatedAt: "2026-05-17T16:00:00.000000000Z", RetryPolicyJSON: "{}", MetaJSON: "{}"}
	for _, record := range []Record{older, newer, finished} {
		if err := manager.ImportJobRecord(record); err != nil {
			t.Fatalf("ImportJobRecord(%s) error = %v", record.ID, err)
		}
	}

	got, err := manager.FindLatestActiveJobByServerAndKind("srv", KindUpdate)
	if err != nil {
		t.Fatalf("FindLatestActiveJobByServerAndKind() error = %v", err)
	}
	if got == nil || got.ID != "newer" {
		t.Fatalf("latest active = %+v, want newer", got)
	}
}

func TestJobManagerMarkUnfinishedJobsInterrupted(t *testing.T) {
	db := openJobTestDB(t)
	var interruptedServers [][]string
	manager := NewManager(NewSQLiteRepository(db), ManagerOptions{
		SyncInterruptedServer: func(serverNames []string) {
			interruptedServers = append(interruptedServers, append([]string(nil), serverNames...))
		},
		Now: func() time.Time { return time.Date(2026, 5, 17, 16, 0, 0, 0, time.UTC) },
	})
	for _, status := range []string{StatusQueued, StatusRunning, StatusWaitingApproval, StatusSucceeded} {
		if _, err := manager.CreateJob(CreateParams{Kind: KindUpdate, ServerName: "srv-" + status, Actor: "admin", Status: status}); err != nil {
			t.Fatalf("CreateJob(%s) error = %v", status, err)
		}
	}

	if err := manager.MarkUnfinishedJobsInterrupted(); err != nil {
		t.Fatalf("MarkUnfinishedJobsInterrupted() error = %v", err)
	}
	var interruptedCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM jobs WHERE status = ?", StatusInterrupted).Scan(&interruptedCount); err != nil {
		t.Fatalf("count interrupted jobs: %v", err)
	}
	if interruptedCount != 3 {
		t.Fatalf("interruptedCount = %d, want 3", interruptedCount)
	}
	if len(interruptedServers) != 1 || len(interruptedServers[0]) != 3 {
		t.Fatalf("interruptedServers = %+v, want three affected servers once", interruptedServers)
	}
}

func TestJobManagerDoesNotDispatchCallbacksAfterRepositoryFailure(t *testing.T) {
	repo := &failingRepository{err: errors.New("write failed")}
	var notifications []string
	var synced []Record
	manager := NewManager(repo, ManagerOptions{
		Notify: func(reason string) {
			notifications = append(notifications, reason)
		},
		SyncRuntime: func(record Record) {
			synced = append(synced, record)
		},
		NewID: func() string { return "job-fail" },
	})

	if _, err := manager.CreateJob(CreateParams{Kind: KindUpdate, Actor: "admin"}); err == nil {
		t.Fatalf("CreateJob() error = nil, want repository failure")
	}
	if err := manager.ImportJobRecord(Record{ID: "job-fail", Actor: "admin"}); err == nil {
		t.Fatalf("ImportJobRecord() error = nil, want repository failure")
	}
	status := StatusRunning
	if _, err := manager.TransitionActive("job-fail", Intent{Status: &status}); err == nil {
		t.Fatalf("TransitionActive() error = nil, want repository failure")
	}
	if len(notifications) != 0 || len(synced) != 0 {
		t.Fatalf("callbacks after failures: notifications=%v synced=%+v", notifications, synced)
	}
}

func openJobTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	if err := EnsureSchema(db); err != nil {
		t.Fatalf("EnsureSchema() error = %v", err)
	}
	return db
}

type failingRepository struct {
	err error
}

func (r *failingRepository) Create(Record) error {
	return r.err
}

func (r *failingRepository) Upsert(Record) error {
	return r.err
}

func (r *failingRepository) ApplyTransition(Record, int64, bool) (bool, error) {
	return false, r.err
}

func (r *failingRepository) Get(string) (Record, error) {
	return Record{}, r.err
}

func (r *failingRepository) FindLatestActiveByServerAndKind(string, string) (*Record, error) {
	return nil, r.err
}

func (r *failingRepository) ListUnfinishedServerNames() ([]string, error) {
	return nil, r.err
}

func (r *failingRepository) MarkUnfinishedInterrupted(string) error {
	return r.err
}
