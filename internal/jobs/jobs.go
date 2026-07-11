package jobs

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	KindUpdate         = "update"
	KindAutoremove     = "autoremove"
	KindSudoersEnable  = "sudoers_enable"
	KindSudoersDisable = "sudoers_disable"
	KindCVEEnrichment  = "cve_enrichment"
	KindBackupExport   = "backup_export"
	KindBackupRestore  = "backup_restore"
	KindScheduledScan  = "scheduled_scan"

	StatusQueued          = "queued"
	StatusRunning         = "running"
	StatusWaitingApproval = "waiting_approval"
	StatusSucceeded       = "succeeded"
	StatusFailed          = "failed"
	StatusCancelled       = "cancelled"
	StatusInterrupted     = "interrupted"

	PhaseDial         = "dial"
	PhasePrechecks    = "prechecks"
	PhaseAptUpdate    = "apt_update"
	PhaseApprovalWait = "approval_wait"
	PhaseAptUpgrade   = "apt_upgrade"
	PhasePostchecks   = "postchecks"
	PhaseAutoremove   = "autoremove"
	PhaseApply        = "apply"
	PhaseSnapshot     = "snapshot"
	PhaseEncrypt      = "encrypt"
	PhaseDecrypt      = "decrypt"
	PhaseLookup       = "lookup"
	PhaseComplete     = "complete"

	TimestampLayout = "2006-01-02T15:04:05.000000000Z"
)

type IntentKind string

const (
	IntentStart               IntentKind = "start"
	IntentAdvance             IntentKind = "advance"
	IntentWaitForApproval     IntentKind = "wait_for_approval"
	IntentResumeAfterApproval IntentKind = "resume_after_approval"
	IntentSucceed             IntentKind = "succeed"
	IntentFail                IntentKind = "fail"
	IntentCancel              IntentKind = "cancel"
	IntentInterrupt           IntentKind = "interrupt"
	IntentAmendProgress       IntentKind = "amend_progress"
	IntentReplaceMetadata     IntentKind = "replace_metadata"
)

var ErrTransitionConflict = errors.New("job state transition conflict")

type Record struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	ParentJobID     string `json:"parent_job_id"`
	ServerName      string `json:"server_name"`
	Actor           string `json:"actor"`
	ClientIP        string `json:"client_ip"`
	Status          string `json:"status"`
	Phase           string `json:"phase"`
	Summary         string `json:"summary"`
	LogsText        string `json:"logs_text"`
	ErrorClass      string `json:"error_class"`
	RetryPolicyJSON string `json:"retry_policy_json"`
	MetaJSON        string `json:"meta_json"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	StartedAt       string `json:"started_at"`
	FinishedAt      string `json:"finished_at"`
	Revision        int64  `json:"-"`
}

type Intent struct {
	Kind       IntentKind
	Status     *string
	Phase      *string
	Summary    *string
	LogsText   *string
	AppendLog  string
	ErrorClass *string
	MetaJSON   *string
}

type CreateParams struct {
	Kind            string
	ParentJobID     string
	ServerName      string
	Actor           string
	ClientIP        string
	Status          string
	Phase           string
	Summary         string
	LogsText        string
	ErrorClass      string
	RetryPolicyJSON string
	MetaJSON        string
}

type Repository interface {
	Create(record Record) error
	Upsert(record Record) error
	ApplyTransition(record Record, expectedRevision int64, activeOnly bool) (bool, error)
	Get(id string) (Record, error)
	FindLatestActiveByServerAndKind(serverName, kind string) (*Record, error)
	ListUnfinishedServerNames() ([]string, error)
	MarkUnfinishedInterrupted(now string) error
}

type SQLiteRepository struct {
	db *sql.DB
}

type ManagerOptions struct {
	Notify                func(string)
	SyncRuntime           func(Record)
	SyncInterruptedServer func([]string)
	Now                   func() time.Time
	NewID                 func() string
}

type Manager struct {
	repo Repository
	opts ManagerOptions
}

func NewSQLiteRepository(db *sql.DB) *SQLiteRepository {
	return &SQLiteRepository{db: db}
}

func NewManager(repo Repository, opts ManagerOptions) *Manager {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewID == nil {
		opts.NewID = NewID
	}
	return &Manager{repo: repo, opts: opts}
}

func EnsureSchema(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS jobs (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			parent_job_id TEXT NOT NULL DEFAULT '',
			server_name TEXT NOT NULL DEFAULT '',
			actor TEXT NOT NULL,
			client_ip TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			phase TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			logs_text TEXT NOT NULL DEFAULT '',
			error_class TEXT NOT NULL DEFAULT '',
			retry_policy_json TEXT NOT NULL DEFAULT '{}',
			meta_json TEXT NOT NULL DEFAULT '{}',
			-- Fixed-width UTC timestamps keep TEXT ordering chronological.
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			started_at TEXT NOT NULL DEFAULT '',
			finished_at TEXT NOT NULL DEFAULT '',
			revision INTEGER NOT NULL DEFAULT 0
		)
	`); err != nil {
		return err
	}
	if err := ensureRevisionColumn(db); err != nil {
		return err
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_jobs_server_created_at ON jobs (server_name, created_at DESC)"); err != nil {
		return err
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_jobs_status_created_at ON jobs (status, created_at DESC)"); err != nil {
		return err
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_jobs_kind_created_at ON jobs (kind, created_at DESC)"); err != nil {
		return err
	}
	return nil
}

func ensureRevisionColumn(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(jobs)")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == "revision" {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec("ALTER TABLE jobs ADD COLUMN revision INTEGER NOT NULL DEFAULT 0")
	return err
}

func NewID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err == nil {
		return hex.EncodeToString(buf)
	}
	return fmt.Sprintf("job-%d", time.Now().UTC().UnixNano())
}

func MarshalJSON(v any) string {
	if v == nil {
		return "{}"
	}
	blob, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(blob)
}

func FormatTimestamp(t time.Time) string {
	return t.UTC().Format(TimestampLayout)
}

func ActiveStatuses() []string {
	return []string{StatusQueued, StatusRunning, StatusWaitingApproval}
}

func (m *Manager) timestampNow() string {
	return FormatTimestamp(m.now())
}

func (m *Manager) now() time.Time {
	if m == nil || m.opts.Now == nil {
		return time.Now()
	}
	return m.opts.Now()
}

func (m *Manager) CreateJob(params CreateParams) (Record, error) {
	if m == nil || m.repo == nil {
		return Record{}, errors.New("job manager is not initialized")
	}
	now := m.timestampNow()
	if strings.TrimSpace(params.Actor) == "" {
		params.Actor = "unknown"
	}
	if strings.TrimSpace(params.Status) == "" {
		params.Status = StatusQueued
	}
	if strings.TrimSpace(params.RetryPolicyJSON) == "" {
		params.RetryPolicyJSON = "{}"
	}
	if strings.TrimSpace(params.MetaJSON) == "" {
		params.MetaJSON = "{}"
	}
	record := Record{
		ID:              m.opts.NewID(),
		Kind:            strings.TrimSpace(params.Kind),
		ParentJobID:     strings.TrimSpace(params.ParentJobID),
		ServerName:      strings.TrimSpace(params.ServerName),
		Actor:           strings.TrimSpace(params.Actor),
		ClientIP:        truncateString(strings.TrimSpace(params.ClientIP), 128),
		Status:          strings.TrimSpace(params.Status),
		Phase:           strings.TrimSpace(params.Phase),
		Summary:         strings.TrimSpace(params.Summary),
		LogsText:        params.LogsText,
		ErrorClass:      strings.TrimSpace(params.ErrorClass),
		RetryPolicyJSON: params.RetryPolicyJSON,
		MetaJSON:        params.MetaJSON,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if record.Status == StatusRunning {
		record.StartedAt = now
	}
	if isTerminalStatus(record.Status) {
		record.Phase = PhaseComplete
		record.FinishedAt = now
	}
	if err := m.repo.Create(record); err != nil {
		return Record{}, err
	}
	m.notify("job.create")
	return record, nil
}

func (m *Manager) ImportJobRecord(record Record) error {
	if m == nil || m.repo == nil {
		return errors.New("job manager is not initialized")
	}
	now := m.timestampNow()
	if strings.TrimSpace(record.ID) == "" {
		record.ID = m.opts.NewID()
	}
	if strings.TrimSpace(record.CreatedAt) == "" {
		record.CreatedAt = now
	}
	record.UpdatedAt = now
	if strings.TrimSpace(record.Actor) == "" {
		record.Actor = "unknown"
	}
	if strings.TrimSpace(record.RetryPolicyJSON) == "" {
		record.RetryPolicyJSON = "{}"
	}
	if strings.TrimSpace(record.MetaJSON) == "" {
		record.MetaJSON = "{}"
	}
	if err := m.repo.Upsert(record); err != nil {
		return err
	}
	m.syncRuntime(record.ID)
	m.notify("job.upsert")
	return nil
}

func (m *Manager) TransitionActive(id string, intent Intent) (bool, error) {
	return m.transition(id, intent, true)
}

func (m *Manager) Transition(id string, intent Intent) error {
	_, err := m.transition(id, intent, false)
	return err
}

func (m *Manager) GetJob(id string) (Record, error) {
	if m == nil || m.repo == nil {
		return Record{}, errors.New("job manager is not initialized")
	}
	return m.repo.Get(id)
}

func (m *Manager) FindLatestActiveJobByServerAndKind(serverName, kind string) (*Record, error) {
	if m == nil || m.repo == nil {
		return nil, errors.New("job manager is not initialized")
	}
	serverName = strings.TrimSpace(serverName)
	kind = strings.TrimSpace(kind)
	if serverName == "" || kind == "" {
		return nil, sql.ErrNoRows
	}
	return m.repo.FindLatestActiveByServerAndKind(serverName, kind)
}

func (m *Manager) MarkUnfinishedJobsInterrupted() error {
	if m == nil || m.repo == nil {
		return nil
	}
	unfinishedServers, err := m.repo.ListUnfinishedServerNames()
	if err != nil {
		return err
	}
	if len(unfinishedServers) == 0 {
		return nil
	}
	now := m.timestampNow()
	if err := m.repo.MarkUnfinishedInterrupted(now); err != nil {
		return err
	}
	affected := make([]string, 0, len(unfinishedServers))
	seen := make(map[string]struct{}, len(unfinishedServers))
	for _, unfinishedServer := range unfinishedServers {
		serverName := strings.TrimSpace(unfinishedServer)
		if serverName == "" {
			continue
		}
		if _, ok := seen[serverName]; ok {
			continue
		}
		seen[serverName] = struct{}{}
		affected = append(affected, serverName)
	}
	if len(affected) > 0 && m.opts.SyncInterruptedServer != nil {
		m.opts.SyncInterruptedServer(affected)
	}
	m.notify("job.update")
	return nil
}

func (m *Manager) transition(id string, intent Intent, activeOnly bool) (bool, error) {
	if m == nil || m.repo == nil || strings.TrimSpace(id) == "" {
		return false, nil
	}
	current, err := m.repo.Get(id)
	if err != nil {
		return false, err
	}
	if activeOnly && isTerminalStatus(current.Status) {
		return false, nil
	}
	next, alreadyApplied, err := m.applyIntent(current, intent)
	if err != nil {
		return false, err
	}
	if alreadyApplied {
		return false, nil
	}
	updated, err := m.repo.ApplyTransition(next, current.Revision, activeOnly)
	if err != nil {
		return false, err
	}
	if !updated {
		if activeOnly {
			return false, nil
		}
		return false, ErrTransitionConflict
	}
	if activeOnly || (intent.Kind != IntentResumeAfterApproval && intent.Kind != IntentCancel) {
		m.syncRuntime(id)
	}
	m.notify("job.update")
	return true, nil
}

func (m *Manager) applyIntent(current Record, intent Intent) (Record, bool, error) {
	next := current
	target := transitionStatus(current.Status, intent)
	if isTerminalStatus(current.Status) {
		if target == current.Status {
			return current, true, nil
		}
		return Record{}, false, fmt.Errorf("%w: terminal job %s cannot become %s", ErrTransitionConflict, current.Status, target)
	}
	if !validTransition(current.Status, target) {
		return Record{}, false, fmt.Errorf("%w: %s cannot become %s", ErrTransitionConflict, current.Status, target)
	}
	now := m.timestampNow()
	next.Status = target
	next.UpdatedAt = now
	next.Revision = current.Revision + 1
	if intent.Phase != nil {
		next.Phase = strings.TrimSpace(*intent.Phase)
	}
	if intent.Summary != nil {
		next.Summary = strings.TrimSpace(*intent.Summary)
	}
	if intent.LogsText != nil {
		next.LogsText = *intent.LogsText
	}
	if intent.AppendLog != "" {
		next.LogsText += intent.AppendLog
	}
	if intent.ErrorClass != nil {
		next.ErrorClass = strings.TrimSpace(*intent.ErrorClass)
	}
	if intent.MetaJSON != nil {
		next.MetaJSON = strings.TrimSpace(*intent.MetaJSON)
	}
	if target == StatusRunning && next.StartedAt == "" {
		next.StartedAt = now
	}
	if target == StatusWaitingApproval {
		next.Phase = PhaseApprovalWait
	}
	if isTerminalStatus(target) {
		next.Phase = PhaseComplete
		next.FinishedAt = now
	}
	return next, false, nil
}

func transitionStatus(current string, intent Intent) string {
	switch intent.Kind {
	case IntentStart, IntentResumeAfterApproval:
		return StatusRunning
	case IntentWaitForApproval:
		return StatusWaitingApproval
	case IntentSucceed:
		return StatusSucceeded
	case IntentFail:
		return StatusFailed
	case IntentCancel:
		return StatusCancelled
	case IntentInterrupt:
		return StatusInterrupted
	}
	if intent.Status != nil {
		return strings.TrimSpace(*intent.Status)
	}
	return current
}

func validTransition(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case StatusQueued:
		return to == StatusRunning || isTerminalStatus(to)
	case StatusRunning:
		return to == StatusWaitingApproval || isTerminalStatus(to)
	case StatusWaitingApproval:
		return to == StatusRunning || isTerminalStatus(to)
	default:
		return false
	}
}

func isTerminalStatus(status string) bool {
	return status == StatusSucceeded || status == StatusFailed || status == StatusCancelled || status == StatusInterrupted
}

func (m *Manager) syncRuntime(id string) {
	if m == nil || m.opts.SyncRuntime == nil {
		return
	}
	record, err := m.GetJob(id)
	if err != nil {
		return
	}
	m.opts.SyncRuntime(record)
}

func (m *Manager) notify(reason string) {
	if m != nil && m.opts.Notify != nil {
		m.opts.Notify(reason)
	}
}

func (r *SQLiteRepository) Create(record Record) error {
	if r == nil || r.db == nil {
		return errors.New("job repository is not initialized")
	}
	_, err := r.db.Exec(`
		INSERT INTO jobs (
			id, kind, parent_job_id, server_name, actor, client_ip, status, phase, summary, logs_text,
			error_class, retry_policy_json, meta_json, created_at, updated_at, started_at, finished_at, revision
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		record.ID,
		record.Kind,
		record.ParentJobID,
		record.ServerName,
		record.Actor,
		record.ClientIP,
		record.Status,
		record.Phase,
		record.Summary,
		record.LogsText,
		record.ErrorClass,
		record.RetryPolicyJSON,
		record.MetaJSON,
		record.CreatedAt,
		record.UpdatedAt,
		record.StartedAt,
		record.FinishedAt,
		record.Revision,
	)
	return err
}

func (r *SQLiteRepository) Upsert(record Record) error {
	if r == nil || r.db == nil {
		return errors.New("job repository is not initialized")
	}
	_, err := r.db.Exec(`
		INSERT INTO jobs (
			id, kind, parent_job_id, server_name, actor, client_ip, status, phase, summary, logs_text,
			error_class, retry_policy_json, meta_json, created_at, updated_at, started_at, finished_at, revision
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			kind = excluded.kind,
			parent_job_id = excluded.parent_job_id,
			server_name = excluded.server_name,
			actor = excluded.actor,
			client_ip = excluded.client_ip,
			status = excluded.status,
			phase = excluded.phase,
			summary = excluded.summary,
			logs_text = excluded.logs_text,
			error_class = excluded.error_class,
			retry_policy_json = excluded.retry_policy_json,
			meta_json = excluded.meta_json,
			updated_at = excluded.updated_at,
			started_at = excluded.started_at,
			finished_at = excluded.finished_at,
			revision = excluded.revision
	`,
		record.ID,
		record.Kind,
		record.ParentJobID,
		record.ServerName,
		record.Actor,
		record.ClientIP,
		record.Status,
		record.Phase,
		record.Summary,
		record.LogsText,
		record.ErrorClass,
		record.RetryPolicyJSON,
		record.MetaJSON,
		record.CreatedAt,
		record.UpdatedAt,
		record.StartedAt,
		record.FinishedAt,
		record.Revision,
	)
	return err
}

func (r *SQLiteRepository) ApplyTransition(record Record, expectedRevision int64, activeOnly bool) (bool, error) {
	if r == nil || r.db == nil || strings.TrimSpace(record.ID) == "" {
		return false, nil
	}
	query := `UPDATE jobs SET status=?, phase=?, summary=?, logs_text=?, error_class=?, meta_json=?,
		updated_at=?, started_at=?, finished_at=?, revision=? WHERE id=? AND revision=?`
	args := []any{record.Status, record.Phase, record.Summary, record.LogsText, record.ErrorClass, record.MetaJSON,
		record.UpdatedAt, record.StartedAt, record.FinishedAt, record.Revision, record.ID, expectedRevision}
	if activeOnly {
		query += " AND status IN (?, ?, ?)"
		args = append(args, StatusQueued, StatusRunning, StatusWaitingApproval)
	}
	result, err := r.db.Exec(query, args...)
	if err != nil {
		return false, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		rowsAffected = 1
	}
	return rowsAffected > 0, nil
}

func (r *SQLiteRepository) Get(id string) (Record, error) {
	if r == nil || r.db == nil {
		return Record{}, errors.New("job repository is not initialized")
	}
	var record Record
	err := r.db.QueryRow(`
		SELECT id, kind, parent_job_id, server_name, actor, client_ip, status, phase, summary, logs_text,
		       error_class, retry_policy_json, meta_json, created_at, updated_at, started_at, finished_at, revision
		  FROM jobs
		 WHERE id = ?
	`, id).Scan(
		&record.ID,
		&record.Kind,
		&record.ParentJobID,
		&record.ServerName,
		&record.Actor,
		&record.ClientIP,
		&record.Status,
		&record.Phase,
		&record.Summary,
		&record.LogsText,
		&record.ErrorClass,
		&record.RetryPolicyJSON,
		&record.MetaJSON,
		&record.CreatedAt,
		&record.UpdatedAt,
		&record.StartedAt,
		&record.FinishedAt,
		&record.Revision,
	)
	return record, err
}

func (r *SQLiteRepository) FindLatestActiveByServerAndKind(serverName, kind string) (*Record, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("job repository is not initialized")
	}
	var record Record
	err := r.db.QueryRow(`
		SELECT id, kind, parent_job_id, server_name, actor, client_ip, status, phase, summary, logs_text,
		       error_class, retry_policy_json, meta_json, created_at, updated_at, started_at, finished_at, revision
		  FROM jobs
		 WHERE server_name = ?
		   AND kind = ?
		   AND status IN (?, ?, ?)
		 ORDER BY created_at DESC
		 LIMIT 1
	`, serverName, kind, StatusQueued, StatusRunning, StatusWaitingApproval).Scan(
		&record.ID,
		&record.Kind,
		&record.ParentJobID,
		&record.ServerName,
		&record.Actor,
		&record.ClientIP,
		&record.Status,
		&record.Phase,
		&record.Summary,
		&record.LogsText,
		&record.ErrorClass,
		&record.RetryPolicyJSON,
		&record.MetaJSON,
		&record.CreatedAt,
		&record.UpdatedAt,
		&record.StartedAt,
		&record.FinishedAt,
		&record.Revision,
	)
	if err == sql.ErrNoRows {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func (r *SQLiteRepository) ListUnfinishedServerNames() ([]string, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("job repository is not initialized")
	}
	rows, err := r.db.Query(`
		SELECT server_name
		  FROM jobs
		 WHERE status IN (?, ?, ?)
	`, StatusQueued, StatusRunning, StatusWaitingApproval)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var serverNames []string
	for rows.Next() {
		var serverName string
		if err := rows.Scan(&serverName); err != nil {
			return nil, err
		}
		serverNames = append(serverNames, serverName)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return serverNames, nil
}

func (r *SQLiteRepository) MarkUnfinishedInterrupted(now string) error {
	if r == nil || r.db == nil {
		return errors.New("job repository is not initialized")
	}
	_, err := r.db.Exec(`
		UPDATE jobs
		   SET status = ?, phase = ?, summary = ?, error_class = ?, finished_at = ?, updated_at = ?, revision = revision + 1
		 WHERE status IN (?, ?, ?)
	`, StatusInterrupted, PhaseComplete, "Interrupted during restart recovery", "restart", now, now, StatusQueued, StatusRunning, StatusWaitingApproval)
	return err
}

func truncateString(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= maxLen {
		return string(runes)
	}
	return string(runes[:maxLen])
}
