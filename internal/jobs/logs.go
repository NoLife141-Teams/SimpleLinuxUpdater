package jobs

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultLogRetentionDays = 30
	DefaultLogMaxBytes      = 2 * 1024 * 1024
	LogHeadBytes            = 64 * 1024
	LogPreviewMaxBytes      = 32 * 1024
	LogChunkMaxBytes        = 32 * 1024
	MinLogMaxBytes          = 128 * 1024
	MaxLogMaxBytes          = 1024 * 1024 * 1024

	LogStreamCombined = "combined"
	LogStreamStdout   = "stdout"
	LogStreamStderr   = "stderr"
	LogStreamSystem   = "system"

	LogTruncationMarker = "\n[... job log middle truncated ...]\n"
	logPreviewMarker    = "\n[... job log preview truncated ...]\n"
)

type LogConfig struct {
	RetentionDays int
	MaxBytes      int
	Now           func() time.Time
}

type LogChunk struct {
	Sequence  int64  `json:"sequence"`
	Stream    string `json:"stream"`
	Data      string `json:"data"`
	CreatedAt string `json:"created_at"`
}

type LogFragment struct {
	Stream string
	Data   string
}

type LogEvent struct {
	ServerName string `json:"server_name"`
	JobID      string `json:"job_id"`
	Sequence   int64  `json:"sequence"`
	Stream     string `json:"stream"`
	Data       string `json:"data"`
}

type LogPage struct {
	JobID         string     `json:"job_id"`
	Fragments     []LogChunk `json:"fragments"`
	NextSequence  int64      `json:"next_sequence"`
	HasMore       bool       `json:"has_more"`
	Expired       bool       `json:"expired"`
	Truncated     bool       `json:"truncated"`
	RetentionDays int        `json:"retention_days"`
}

type logAppendResult struct {
	ServerName string
	Chunks     []LogChunk
	Updated    bool
}

type structuredLogRepository interface {
	appendActiveLogFragments(id string, fragments []LogFragment, updatedAt string) (logAppendResult, error)
	ReadLogPage(id string, afterSequence int64, limit int) (LogPage, error)
	ReadFullLog(id string) (string, bool, bool, error)
	PurgeExpiredLogs() (int64, error)
	LogRetentionDays() int
}

func DefaultLogConfig() LogConfig {
	return LogConfig{
		RetentionDays: DefaultLogRetentionDays,
		MaxBytes:      DefaultLogMaxBytes,
		Now:           time.Now,
	}
}

func normalizeLogConfig(config LogConfig) LogConfig {
	defaults := DefaultLogConfig()
	if config.RetentionDays <= 0 {
		config.RetentionDays = defaults.RetentionDays
	}
	if config.MaxBytes < MinLogMaxBytes || config.MaxBytes > MaxLogMaxBytes {
		config.MaxBytes = defaults.MaxBytes
	}
	if config.Now == nil {
		config.Now = defaults.Now
	}
	return config
}

func NewSQLiteRepositoryWithLogConfig(db *sql.DB, config LogConfig) *SQLiteRepository {
	return &SQLiteRepository{db: db, logConfig: normalizeLogConfig(config)}
}

func EnsureSchemaWithLogConfig(db *sql.DB, config LogConfig) error {
	config = normalizeLogConfig(config)
	if err := ensureJobLogColumns(db); err != nil {
		return err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS job_log_chunks (
			job_id TEXT NOT NULL,
			sequence INTEGER NOT NULL,
			stream TEXT NOT NULL,
			data BLOB NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (job_id, sequence)
		)
	`); err != nil {
		return err
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_job_log_chunks_job_sequence ON job_log_chunks (job_id, sequence)"); err != nil {
		return err
	}
	repo := NewSQLiteRepositoryWithLogConfig(db, config)
	return repo.migrateLegacyLogs()
}

func ensureJobLogColumns(db *sql.DB) error {
	columns := []struct {
		name string
		sql  string
	}{
		{"logs_expired", "ALTER TABLE jobs ADD COLUMN logs_expired INTEGER NOT NULL DEFAULT 0"},
		{"logs_truncated", "ALTER TABLE jobs ADD COLUMN logs_truncated INTEGER NOT NULL DEFAULT 0"},
		{"log_next_sequence", "ALTER TABLE jobs ADD COLUMN log_next_sequence INTEGER NOT NULL DEFAULT 1"},
		{"log_source_bytes", "ALTER TABLE jobs ADD COLUMN log_source_bytes INTEGER NOT NULL DEFAULT 0"},
	}
	for _, column := range columns {
		found, err := tableHasColumn(db, "jobs", column.name)
		if err != nil {
			return err
		}
		if !found {
			if _, err := db.Exec(column.sql); err != nil {
				return err
			}
		}
	}
	return nil
}

func tableHasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

func (r *SQLiteRepository) configureLogClock(now func() time.Time) {
	if r == nil || now == nil {
		return
	}
	r.logConfig = normalizeLogConfig(r.logConfig)
	r.logConfig.Now = now
}

func (r *SQLiteRepository) LogRetentionDays() int {
	if r == nil {
		return DefaultLogRetentionDays
	}
	return normalizeLogConfig(r.logConfig).RetentionDays
}

func (r *SQLiteRepository) migrateLegacyLogs() error {
	if r == nil || r.db == nil {
		return errors.New("job repository is not initialized")
	}
	config := normalizeLogConfig(r.logConfig)
	cutoff := FormatTimestamp(config.Now().UTC().Add(-time.Duration(config.RetentionDays) * 24 * time.Hour))
	rows, err := r.db.Query(`
		SELECT id, logs_text, status, COALESCE(NULLIF(finished_at, ''), updated_at, created_at)
		  FROM jobs
		 WHERE NOT EXISTS (SELECT 1 FROM job_log_chunks WHERE job_id = jobs.id)
	`)
	if err != nil {
		return err
	}
	type legacyLog struct {
		id, data, status, reference string
	}
	var logs []legacyLog
	for rows.Next() {
		var item legacyLog
		if err := rows.Scan(&item.id, &item.data, &item.status, &item.reference); err != nil {
			_ = rows.Close()
			return err
		}
		logs = append(logs, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range logs {
		if isTerminalStatus(item.status) && item.reference <= cutoff {
			if _, err := r.db.Exec(`
				UPDATE jobs
				   SET logs_text = '', logs_expired = 1
				 WHERE id = ? AND status IN (?, ?, ?, ?)
			`, item.id, StatusSucceeded, StatusFailed, StatusCancelled, StatusInterrupted); err != nil {
				return err
			}
			continue
		}
		if item.data == "" {
			continue
		}
		tx, err := r.db.Begin()
		if err != nil {
			return err
		}
		if err := r.replaceLogSnapshotTx(tx, item.id, item.data, LogStreamCombined, FormatTimestamp(config.Now())); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (r *SQLiteRepository) appendActiveLogFragments(id string, fragments []LogFragment, updatedAt string) (logAppendResult, error) {
	if r == nil || r.db == nil || strings.TrimSpace(id) == "" || len(fragments) == 0 {
		return logAppendResult{}, nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return logAppendResult{}, err
	}
	defer tx.Rollback()
	var status, serverName string
	if err := tx.QueryRow("SELECT status, server_name FROM jobs WHERE id = ?", id).Scan(&status, &serverName); err != nil {
		return logAppendResult{}, err
	}
	if isTerminalStatus(status) {
		return logAppendResult{}, nil
	}
	chunks, err := r.appendFragmentsTx(tx, id, fragments, updatedAt)
	if err != nil {
		return logAppendResult{}, err
	}
	result, err := tx.Exec(`
		UPDATE jobs
		   SET updated_at = ?, revision = revision + 1
		 WHERE id = ? AND status IN (?, ?, ?)
	`, updatedAt, id, StatusQueued, StatusRunning, StatusWaitingApproval)
	if err != nil {
		return logAppendResult{}, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return logAppendResult{}, err
	}
	if rowsAffected == 0 {
		return logAppendResult{}, nil
	}
	if err := tx.Commit(); err != nil {
		return logAppendResult{}, err
	}
	return logAppendResult{ServerName: serverName, Chunks: chunks, Updated: true}, nil
}

func (r *SQLiteRepository) AppendActiveLogFragments(id string, fragments []LogFragment, updatedAt string) (bool, error) {
	result, err := r.appendActiveLogFragments(id, fragments, updatedAt)
	return result.Updated, err
}

func (r *SQLiteRepository) appendFragmentsTx(tx *sql.Tx, id string, fragments []LogFragment, createdAt string) ([]LogChunk, error) {
	var nextSequence int64
	var sourceBytes int64
	var expired int
	if err := tx.QueryRow(`
		SELECT log_next_sequence, log_source_bytes, logs_expired
		  FROM jobs
		 WHERE id = ?
	`, id).Scan(&nextSequence, &sourceBytes, &expired); err != nil {
		return nil, err
	}
	if expired != 0 {
		return nil, nil
	}
	chunks := make([]LogChunk, 0, len(fragments))
	for _, fragment := range fragments {
		if fragment.Data == "" {
			continue
		}
		stream := normalizeLogStream(fragment.Stream)
		for _, part := range splitLogData(fragment.Data, LogChunkMaxBytes) {
			if _, err := tx.Exec(`
				INSERT INTO job_log_chunks (job_id, sequence, stream, data, created_at)
				VALUES (?, ?, ?, ?, ?)
			`, id, nextSequence, stream, []byte(part), createdAt); err != nil {
				return nil, err
			}
			chunks = append(chunks, LogChunk{
				Sequence:  nextSequence,
				Stream:    stream,
				Data:      part,
				CreatedAt: createdAt,
			})
			nextSequence++
		}
		sourceBytes += int64(len(fragment.Data))
	}
	if _, err := tx.Exec(`
		UPDATE jobs
		   SET log_next_sequence = ?, log_source_bytes = ?
		 WHERE id = ?
	`, nextSequence, sourceBytes, id); err != nil {
		return nil, err
	}
	if err := r.enforceLogLimitTx(tx, id); err != nil {
		return nil, err
	}
	if err := r.updateLogPreviewTx(tx, id); err != nil {
		return nil, err
	}
	return chunks, nil
}

func (r *SQLiteRepository) replaceLogSnapshotTx(tx *sql.Tx, id, data, stream, createdAt string) error {
	var nextSequence int64
	var sourceBytes int
	var truncated int
	if err := tx.QueryRow(`
		SELECT log_next_sequence, log_source_bytes, logs_truncated
		  FROM jobs
		 WHERE id = ?
	`, id).Scan(&nextSequence, &sourceBytes, &truncated); err != nil {
		return err
	}
	rows, err := tx.Query(`
		SELECT stream, CAST(data AS TEXT)
		  FROM job_log_chunks
		 WHERE job_id = ?
		 ORDER BY sequence
	`, id)
	if err != nil {
		return err
	}
	var retained, head, tail strings.Builder
	afterMarker := false
	for rows.Next() {
		var chunkStream, chunkData string
		if err := rows.Scan(&chunkStream, &chunkData); err != nil {
			_ = rows.Close()
			return err
		}
		retained.WriteString(chunkData)
		if chunkStream == LogStreamSystem && chunkData == LogTruncationMarker {
			afterMarker = true
			continue
		}
		if afterMarker {
			tail.WriteString(chunkData)
		} else {
			head.WriteString(chunkData)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if truncated == 0 && retained.Len() > 0 && strings.HasPrefix(data, retained.String()) {
		delta := data[retained.Len():]
		if delta == "" {
			return nil
		}
		_, err := r.appendFragmentsTx(tx, id, []LogFragment{{Stream: stream, Data: delta}}, createdAt)
		return err
	}
	if truncated != 0 && sourceBytes <= len(data) &&
		strings.HasPrefix(data, head.String()) &&
		(len(tail.String()) == 0 || strings.HasSuffix(data[:sourceBytes], tail.String())) {
		delta := data[sourceBytes:]
		if delta == "" {
			return nil
		}
		_, err := r.appendFragmentsTx(tx, id, []LogFragment{{Stream: stream, Data: delta}}, createdAt)
		return err
	}
	if _, err := tx.Exec("DELETE FROM job_log_chunks WHERE job_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE jobs
		   SET logs_expired = 0, logs_truncated = 0, log_source_bytes = 0,
		       log_next_sequence = ?
		 WHERE id = ?
	`, nextSequence, id); err != nil {
		return err
	}
	_, err = r.appendFragmentsTx(tx, id, []LogFragment{{Stream: stream, Data: data}}, createdAt)
	return err
}

type persistedLogChunk struct {
	sequence  int64
	stream    string
	data      string
	createdAt string
}

func (r *SQLiteRepository) enforceLogLimitTx(tx *sql.Tx, id string) error {
	config := normalizeLogConfig(r.logConfig)
	rows, err := tx.Query(`
		SELECT sequence, stream, CAST(data AS TEXT), created_at
		  FROM job_log_chunks
		 WHERE job_id = ?
		 ORDER BY sequence
	`, id)
	if err != nil {
		return err
	}
	var chunks []persistedLogChunk
	total := 0
	markerIndex := -1
	for rows.Next() {
		var chunk persistedLogChunk
		if err := rows.Scan(&chunk.sequence, &chunk.stream, &chunk.data, &chunk.createdAt); err != nil {
			_ = rows.Close()
			return err
		}
		if chunk.stream == LogStreamSystem && chunk.data == LogTruncationMarker {
			markerIndex = len(chunks)
		}
		total += len(chunk.data)
		chunks = append(chunks, chunk)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if markerIndex < 0 && total <= config.MaxBytes {
		return nil
	}
	tailBudget := config.MaxBytes - LogHeadBytes - len(LogTruncationMarker)
	if tailBudget < 1 {
		return fmt.Errorf("job log max bytes %d is too small", config.MaxBytes)
	}
	if markerIndex >= 0 {
		tailBytes := 0
		for i := markerIndex + 1; i < len(chunks); i++ {
			tailBytes += len(chunks[i].data)
		}
		excess := tailBytes - tailBudget
		for i := markerIndex + 1; excess > 0 && i < len(chunks); i++ {
			chunk := chunks[i]
			if len(chunk.data) <= excess {
				if _, err := tx.Exec("DELETE FROM job_log_chunks WHERE job_id = ? AND sequence = ?", id, chunk.sequence); err != nil {
					return err
				}
				excess -= len(chunk.data)
				continue
			}
			trimmed := trimLogPrefix(chunk.data, excess)
			if _, err := tx.Exec("UPDATE job_log_chunks SET data = ? WHERE job_id = ? AND sequence = ?", []byte(trimmed), id, chunk.sequence); err != nil {
				return err
			}
			excess = 0
		}
		_, err := tx.Exec("UPDATE jobs SET logs_truncated = 1 WHERE id = ?", id)
		return err
	}

	headEnd := 0
	headBytes := 0
	for headEnd < len(chunks) && headBytes+len(chunks[headEnd].data) <= LogHeadBytes {
		headBytes += len(chunks[headEnd].data)
		headEnd++
	}
	if headBytes < LogHeadBytes && headEnd < len(chunks) {
		chunk := chunks[headEnd]
		keep := safePrefixBytes(chunk.data, LogHeadBytes-headBytes)
		if _, err := tx.Exec("UPDATE job_log_chunks SET data = ? WHERE job_id = ? AND sequence = ?", []byte(keep), id, chunk.sequence); err != nil {
			return err
		}
		headEnd++
	}
	tailStart := len(chunks)
	tailBytes := 0
	for tailStart > headEnd && tailBytes+len(chunks[tailStart-1].data) <= tailBudget {
		tailStart--
		tailBytes += len(chunks[tailStart].data)
	}
	if tailStart > headEnd && tailBytes < tailBudget {
		tailStart--
		chunk := chunks[tailStart]
		keep := safeSuffixBytes(chunk.data, tailBudget-tailBytes)
		if _, err := tx.Exec("UPDATE job_log_chunks SET data = ? WHERE job_id = ? AND sequence = ?", []byte(keep), id, chunk.sequence); err != nil {
			return err
		}
	}
	markerSequence := int64(0)
	if tailStart > headEnd {
		markerSequence = chunks[headEnd].sequence
	} else if tailStart < len(chunks) {
		markerSequence = chunks[tailStart].sequence
		tailStart++
	} else {
		return errors.New("job log truncation could not reserve a marker sequence")
	}
	if _, err := tx.Exec(`
		DELETE FROM job_log_chunks
		 WHERE job_id = ? AND sequence >= ? AND sequence < ?
	`, id, chunks[headEnd].sequence, func() int64 {
		if tailStart < len(chunks) {
			return chunks[tailStart].sequence
		}
		return chunks[len(chunks)-1].sequence + 1
	}()); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT OR REPLACE INTO job_log_chunks (job_id, sequence, stream, data, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, id, markerSequence, LogStreamSystem, []byte(LogTruncationMarker), FormatTimestamp(config.Now())); err != nil {
		return err
	}
	_, err = tx.Exec("UPDATE jobs SET logs_truncated = 1 WHERE id = ?", id)
	return err
}

func (r *SQLiteRepository) updateLogPreviewTx(tx *sql.Tx, id string) error {
	rows, err := tx.Query(`
		SELECT CAST(data AS TEXT)
		  FROM job_log_chunks
		 WHERE job_id = ?
		 ORDER BY sequence
	`, id)
	if err != nil {
		return err
	}
	var full strings.Builder
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			_ = rows.Close()
			return err
		}
		full.WriteString(data)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	preview := BoundedLogPreview(full.String())
	_, err = tx.Exec("UPDATE jobs SET logs_text = ? WHERE id = ?", preview, id)
	return err
}

func BoundedLogPreview(data string) string {
	if len(data) <= LogPreviewMaxBytes {
		return data
	}
	headBudget := LogPreviewMaxBytes / 2
	tailBudget := LogPreviewMaxBytes - headBudget - len(logPreviewMarker)
	return safePrefixBytes(data, headBudget) + logPreviewMarker + safeSuffixBytes(data, tailBudget)
}

func (r *SQLiteRepository) ReadLogPage(id string, afterSequence int64, limit int) (LogPage, error) {
	if r == nil || r.db == nil {
		return LogPage{}, errors.New("job repository is not initialized")
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	page := LogPage{JobID: id, NextSequence: afterSequence, RetentionDays: r.LogRetentionDays()}
	var expired, truncated int
	if err := r.db.QueryRow("SELECT logs_expired, logs_truncated FROM jobs WHERE id = ?", id).Scan(&expired, &truncated); err != nil {
		return LogPage{}, err
	}
	page.Expired = expired != 0
	page.Truncated = truncated != 0
	rows, err := r.db.Query(`
		SELECT sequence, stream, CAST(data AS TEXT), created_at
		  FROM job_log_chunks
		 WHERE job_id = ? AND sequence > ?
		 ORDER BY sequence
		 LIMIT ?
	`, id, afterSequence, limit+1)
	if err != nil {
		return LogPage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var chunk LogChunk
		if err := rows.Scan(&chunk.Sequence, &chunk.Stream, &chunk.Data, &chunk.CreatedAt); err != nil {
			return LogPage{}, err
		}
		if len(page.Fragments) == limit {
			page.HasMore = true
			break
		}
		page.Fragments = append(page.Fragments, chunk)
		page.NextSequence = chunk.Sequence
	}
	if page.Fragments == nil {
		page.Fragments = []LogChunk{}
	}
	return page, rows.Err()
}

func (r *SQLiteRepository) ReadFullLog(id string) (string, bool, bool, error) {
	page, err := r.ReadLogPage(id, 0, 500)
	if err != nil {
		return "", false, false, err
	}
	var full strings.Builder
	after := int64(0)
	for {
		for _, fragment := range page.Fragments {
			full.WriteString(fragment.Data)
			after = fragment.Sequence
		}
		if !page.HasMore {
			break
		}
		page, err = r.ReadLogPage(id, after, 500)
		if err != nil {
			return "", false, false, err
		}
	}
	if full.Len() == 0 && !page.Expired {
		var preview string
		if err := r.db.QueryRow("SELECT logs_text FROM jobs WHERE id = ?", id).Scan(&preview); err != nil {
			return "", false, false, err
		}
		full.WriteString(preview)
	}
	return full.String(), page.Expired, page.Truncated, nil
}

func (r *SQLiteRepository) PurgeExpiredLogs() (int64, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("job repository is not initialized")
	}
	config := normalizeLogConfig(r.logConfig)
	cutoff := FormatTimestamp(config.Now().UTC().Add(-time.Duration(config.RetentionDays) * 24 * time.Hour))
	tx, err := r.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`
		SELECT id
		  FROM jobs
		 WHERE status IN (?, ?, ?, ?)
		   AND COALESCE(NULLIF(finished_at, ''), updated_at, created_at) <= ?
		   AND logs_expired = 0
	`, StatusSucceeded, StatusFailed, StatusCancelled, StatusInterrupted, cutoff)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := tx.Exec("DELETE FROM job_log_chunks WHERE job_id = ?", id); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`
			UPDATE jobs
			   SET logs_text = '', logs_expired = 1
			 WHERE id = ? AND status IN (?, ?, ?, ?)
		`, id, StatusSucceeded, StatusFailed, StatusCancelled, StatusInterrupted); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(ids)), nil
}

func normalizeLogStream(stream string) string {
	switch strings.ToLower(strings.TrimSpace(stream)) {
	case LogStreamStdout:
		return LogStreamStdout
	case LogStreamStderr:
		return LogStreamStderr
	case LogStreamSystem:
		return LogStreamSystem
	default:
		return LogStreamCombined
	}
}

func splitLogData(data string, maxBytes int) []string {
	if data == "" {
		return nil
	}
	var parts []string
	for len(data) > maxBytes {
		cut := maxBytes
		for cut > 0 && cut < len(data) && !utf8.RuneStart(data[cut]) {
			cut--
		}
		if cut == 0 {
			cut = maxBytes
		}
		parts = append(parts, data[:cut])
		data = data[cut:]
	}
	parts = append(parts, data)
	return parts
}

func safePrefixBytes(data string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(data) <= maxBytes {
		return data
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(data[cut]) {
		cut--
	}
	return data[:cut]
}

func safeSuffixBytes(data string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(data) <= maxBytes {
		return data
	}
	start := len(data) - maxBytes
	for start < len(data) && !utf8.RuneStart(data[start]) {
		start++
	}
	return data[start:]
}

func trimLogPrefix(data string, bytes int) string {
	if bytes <= 0 {
		return data
	}
	if bytes >= len(data) {
		return ""
	}
	start := bytes
	for start < len(data) && !utf8.RuneStart(data[start]) {
		start++
	}
	return data[start:]
}
