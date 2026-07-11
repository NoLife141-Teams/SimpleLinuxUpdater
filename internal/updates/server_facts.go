package updates

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	HealthSnapshotRetentionSettingKey  = "health_snapshot_retention_days"
	DefaultHealthSnapshotRetentionDays = 90
)

type ServerFactsRepository interface {
	Save(ServerFactsRecord) error
	LoadAll() (map[string]ServerFactsRecord, error)
	RenameServerTx(*sql.Tx, string, string) error
	DeleteServerTx(*sql.Tx, string) error
}

// HealthSnapshotCapture records accepted, time-ordered Server health observations.
type HealthSnapshotCapture interface {
	CaptureFacts(ServerFactsRecord) error
	CaptureCompletion(MaintenanceCompletion) error
}

type SQLiteServerFactsRepository struct {
	DB  func() *sql.DB
	Now func() time.Time
}

func (r SQLiteServerFactsRepository) dbConn() *sql.DB {
	if r.DB != nil {
		return r.DB()
	}
	return nil
}

func (r SQLiteServerFactsRepository) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

func EnsureServerFactsSchema(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS server_facts (
			server_name TEXT PRIMARY KEY,
			collected_at TEXT NOT NULL,
			os_pretty_name TEXT NOT NULL DEFAULT '',
			uptime_seconds INTEGER NOT NULL DEFAULT 0,
			disk_status TEXT NOT NULL DEFAULT 'unknown',
			disk_free_kb INTEGER NOT NULL DEFAULT 0,
			disk_total_kb INTEGER NOT NULL DEFAULT 0,
			disk_details TEXT NOT NULL DEFAULT '',
			apt_status TEXT NOT NULL DEFAULT 'unknown',
			apt_details TEXT NOT NULL DEFAULT '',
			reboot_required INTEGER,
			raw_json TEXT NOT NULL DEFAULT '{}'
		)
	`); err != nil {
		return err
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_server_facts_collected_at ON server_facts (collected_at DESC)"); err != nil {
		return err
	}
	if err := ensureColumn(db, "server_facts", "disk_total_kb", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS server_health_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			server_name TEXT NOT NULL,
			captured_at TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT 'unknown',
			package_count INTEGER NOT NULL DEFAULT 0,
			security_count INTEGER NOT NULL DEFAULT 0,
			last_scan_status TEXT NOT NULL DEFAULT '',
			last_update_status TEXT NOT NULL DEFAULT '',
			disk_status TEXT NOT NULL DEFAULT 'unknown',
			disk_free_kb INTEGER NOT NULL DEFAULT 0,
			disk_total_kb INTEGER NOT NULL DEFAULT 0,
			apt_status TEXT NOT NULL DEFAULT 'unknown',
			reboot_required INTEGER,
			os_pretty_name TEXT NOT NULL DEFAULT '',
			raw_json TEXT NOT NULL DEFAULT '{}'
		)
	`); err != nil {
		return err
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_server_health_snapshots_server_time ON server_health_snapshots (server_name, captured_at DESC)"); err != nil {
		return err
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_server_health_snapshots_captured_at ON server_health_snapshots (captured_at DESC)"); err != nil {
		return err
	}
	if _, err := db.Exec(
		"INSERT OR IGNORE INTO settings(key, value) VALUES(?, ?)",
		HealthSnapshotRetentionSettingKey,
		strconv.Itoa(DefaultHealthSnapshotRetentionDays),
	); err != nil {
		return err
	}
	return pruneHealthSnapshotsWithDB(db, time.Now().UTC())
}

func (r SQLiteServerFactsRepository) HealthSnapshotRetentionDays() (int, error) {
	db := r.dbConn()
	if db == nil {
		return 0, errors.New("database is not initialized")
	}
	return healthSnapshotRetentionDays(db)
}

func healthSnapshotRetentionDays(db *sql.DB) (int, error) {
	var raw string
	err := db.QueryRow("SELECT value FROM settings WHERE key = ?", HealthSnapshotRetentionSettingKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultHealthSnapshotRetentionDays, nil
	}
	if err != nil {
		return 0, err
	}
	days, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || days <= 0 {
		return DefaultHealthSnapshotRetentionDays, nil
	}
	return days, nil
}

func pruneHealthSnapshotsWithDB(db *sql.DB, now time.Time) error {
	retentionDays, err := healthSnapshotRetentionDays(db)
	if err != nil {
		return err
	}
	cutoff := now.UTC().AddDate(0, 0, -retentionDays).Format(time.RFC3339)
	_, err = db.Exec("DELETE FROM server_health_snapshots WHERE captured_at < ?", cutoff)
	return err
}

func (r SQLiteServerFactsRepository) PruneHealthSnapshots() error {
	db := r.dbConn()
	if db == nil {
		return errors.New("database is not initialized")
	}
	return pruneHealthSnapshotsWithDB(db, r.now())
}

func ensureColumn(db *sql.DB, table, name, definition string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var columnName, columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if columnName == name {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + name + " " + definition)
	return err
}

func (r SQLiteServerFactsRepository) Save(record ServerFactsRecord) error {
	db := r.dbConn()
	if db == nil {
		return errors.New("database is not initialized")
	}
	record.ServerName = strings.TrimSpace(record.ServerName)
	if record.ServerName == "" {
		return errors.New("server name is required")
	}
	if strings.TrimSpace(record.CollectedAt) == "" {
		record.CollectedAt = r.now().UTC().Format(time.RFC3339)
	}
	if strings.TrimSpace(record.DiskStatus) == "" {
		record.DiskStatus = "unknown"
	}
	if strings.TrimSpace(record.AptStatus) == "" {
		record.AptStatus = "unknown"
	}
	if strings.TrimSpace(record.RawJSON) == "" {
		record.RawJSON = "{}"
	}
	var rebootValue any
	if record.RebootRequired != nil {
		rebootValue = boolToInt(*record.RebootRequired)
	}
	_, err := db.Exec(`
		INSERT INTO server_facts (
			server_name, collected_at, os_pretty_name, uptime_seconds,
			disk_status, disk_free_kb, disk_total_kb, disk_details, apt_status, apt_details,
			reboot_required, raw_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(server_name) DO UPDATE SET
			collected_at = excluded.collected_at,
			os_pretty_name = excluded.os_pretty_name,
			uptime_seconds = excluded.uptime_seconds,
			disk_status = excluded.disk_status,
			disk_free_kb = excluded.disk_free_kb,
			disk_total_kb = excluded.disk_total_kb,
			disk_details = excluded.disk_details,
			apt_status = excluded.apt_status,
			apt_details = excluded.apt_details,
			reboot_required = excluded.reboot_required,
			raw_json = excluded.raw_json
	`,
		record.ServerName,
		record.CollectedAt,
		record.OSPrettyName,
		record.UptimeSeconds,
		record.DiskStatus,
		record.DiskFreeKB,
		record.DiskTotalKB,
		record.DiskDetails,
		record.AptStatus,
		record.AptDetails,
		rebootValue,
		record.RawJSON,
	)
	if err != nil {
		return err
	}
	return r.CaptureFacts(record)
}

func (r SQLiteServerFactsRepository) LoadAll() (map[string]ServerFactsRecord, error) {
	db := r.dbConn()
	if db == nil {
		return nil, errors.New("database is not initialized")
	}
	rows, err := db.Query(`
		SELECT server_name, collected_at, os_pretty_name, uptime_seconds,
		       disk_status, disk_free_kb, disk_total_kb, disk_details, apt_status, apt_details,
		       reboot_required, raw_json
		  FROM server_facts
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := map[string]ServerFactsRecord{}
	for rows.Next() {
		var record ServerFactsRecord
		var reboot sql.NullInt64
		if err := rows.Scan(
			&record.ServerName,
			&record.CollectedAt,
			&record.OSPrettyName,
			&record.UptimeSeconds,
			&record.DiskStatus,
			&record.DiskFreeKB,
			&record.DiskTotalKB,
			&record.DiskDetails,
			&record.AptStatus,
			&record.AptDetails,
			&reboot,
			&record.RawJSON,
		); err != nil {
			return nil, err
		}
		if reboot.Valid {
			required := reboot.Int64 != 0
			record.RebootRequired = &required
		}
		records[record.ServerName] = record
	}
	return records, rows.Err()
}

func (r SQLiteServerFactsRepository) RenameServerTx(tx *sql.Tx, oldName, newName string) error {
	if strings.TrimSpace(oldName) == "" || strings.TrimSpace(newName) == "" || oldName == newName {
		return nil
	}
	if _, err := tx.Exec("UPDATE server_facts SET server_name = ? WHERE server_name = ?", newName, oldName); err != nil {
		return err
	}
	_, err := tx.Exec("UPDATE server_health_snapshots SET server_name = ? WHERE server_name = ?", newName, oldName)
	return err
}

func (r SQLiteServerFactsRepository) DeleteServerTx(tx *sql.Tx, name string) error {
	if _, err := tx.Exec("DELETE FROM server_facts WHERE server_name = ?", name); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM server_health_snapshots WHERE server_name = ?", name)
	return err
}

func (r SQLiteServerFactsRepository) CaptureFacts(record ServerFactsRecord) error {
	return r.saveHealthSnapshot(HealthSnapshotRecord{
		ServerName:     record.ServerName,
		CapturedAt:     record.CollectedAt,
		Source:         "facts",
		DiskStatus:     record.DiskStatus,
		DiskFreeKB:     record.DiskFreeKB,
		DiskTotalKB:    record.DiskTotalKB,
		AptStatus:      record.AptStatus,
		RebootRequired: record.RebootRequired,
		OSPrettyName:   record.OSPrettyName,
		RawJSON:        record.RawJSON,
	})
}

func (r SQLiteServerFactsRepository) saveHealthSnapshot(record HealthSnapshotRecord) error {
	db := r.dbConn()
	if db == nil {
		return errors.New("database is not initialized")
	}
	record.ServerName = strings.TrimSpace(record.ServerName)
	if record.ServerName == "" {
		return errors.New("server name is required")
	}
	if strings.TrimSpace(record.CapturedAt) == "" {
		record.CapturedAt = r.now().UTC().Format(time.RFC3339)
	}
	if strings.TrimSpace(record.Source) == "" {
		record.Source = "unknown"
	}
	if strings.TrimSpace(record.DiskStatus) == "" {
		record.DiskStatus = "unknown"
	}
	if strings.TrimSpace(record.AptStatus) == "" {
		record.AptStatus = "unknown"
	}
	if strings.TrimSpace(record.RawJSON) == "" {
		record.RawJSON = "{}"
	}
	if record.PackageCount < 0 || record.SecurityCount < 0 {
		return fmt.Errorf("snapshot package counts must be non-negative")
	}
	var rebootValue any
	if record.RebootRequired != nil {
		rebootValue = boolToInt(*record.RebootRequired)
	}
	_, err := db.Exec(`
		INSERT INTO server_health_snapshots (
			server_name, captured_at, source, package_count, security_count,
			last_scan_status, last_update_status, disk_status, disk_free_kb, disk_total_kb,
			apt_status, reboot_required, os_pretty_name, raw_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		record.ServerName,
		record.CapturedAt,
		record.Source,
		record.PackageCount,
		record.SecurityCount,
		record.LastScanStatus,
		record.LastUpdateStatus,
		record.DiskStatus,
		record.DiskFreeKB,
		record.DiskTotalKB,
		record.AptStatus,
		rebootValue,
		record.OSPrettyName,
		record.RawJSON,
	)
	if err != nil {
		return err
	}
	return r.PruneHealthSnapshots()
}

func (r SQLiteServerFactsRepository) ListHealthSnapshots(from, to, serverName string) ([]HealthSnapshotRecord, error) {
	db := r.dbConn()
	if db == nil {
		return nil, errors.New("database is not initialized")
	}
	query := `
		SELECT id, server_name, captured_at, source, package_count, security_count,
		       last_scan_status, last_update_status, disk_status, disk_free_kb, disk_total_kb,
		       apt_status, reboot_required, os_pretty_name, raw_json
		  FROM server_health_snapshots
		 WHERE captured_at >= ? AND captured_at <= ?`
	args := []any{from, to}
	if strings.TrimSpace(serverName) != "" {
		query += " AND server_name = ?"
		args = append(args, strings.TrimSpace(serverName))
	}
	query += " ORDER BY server_name ASC, captured_at ASC, id ASC"
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []HealthSnapshotRecord
	for rows.Next() {
		var record HealthSnapshotRecord
		var reboot sql.NullInt64
		if err := rows.Scan(
			&record.ID,
			&record.ServerName,
			&record.CapturedAt,
			&record.Source,
			&record.PackageCount,
			&record.SecurityCount,
			&record.LastScanStatus,
			&record.LastUpdateStatus,
			&record.DiskStatus,
			&record.DiskFreeKB,
			&record.DiskTotalKB,
			&record.AptStatus,
			&reboot,
			&record.OSPrettyName,
			&record.RawJSON,
		); err != nil {
			return nil, err
		}
		if reboot.Valid {
			required := reboot.Int64 != 0
			record.RebootRequired = &required
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
