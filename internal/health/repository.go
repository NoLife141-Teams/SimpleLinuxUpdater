package health

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	HealthSnapshotRetentionSettingKey = "health_snapshot_retention_days"
	DefaultRetentionDays              = 90
)

type Reader interface {
	Latest() (map[string]CollectedFacts, error)
	History(from, to, serverName string) ([]Snapshot, error)
	RetentionDays() (int, error)
}

type Observation interface {
	Reader
	AcceptCollectedFacts(CollectedFacts) error
	AcceptMaintenance(MaintenanceOutcome) error
	RenameServerTx(*sql.Tx, string, string) error
	DeleteServerTx(*sql.Tx, string) error
}

type ReaderFuncs struct {
	LatestFunc        func() (map[string]CollectedFacts, error)
	HistoryFunc       func(string, string, string) ([]Snapshot, error)
	RetentionDaysFunc func() (int, error)
}

func (f ReaderFuncs) Latest() (map[string]CollectedFacts, error) { return f.LatestFunc() }
func (f ReaderFuncs) History(from, to, server string) ([]Snapshot, error) {
	return f.HistoryFunc(from, to, server)
}
func (f ReaderFuncs) RetentionDays() (int, error) { return f.RetentionDaysFunc() }

type SQLiteObservation struct {
	DB  func() *sql.DB
	Now func() time.Time
}

func (r SQLiteObservation) dbConn() *sql.DB {
	if r.DB != nil {
		return r.DB()
	}
	return nil
}

func (r SQLiteObservation) now() time.Time {
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
		strconv.Itoa(DefaultRetentionDays),
	); err != nil {
		return err
	}
	return pruneHealthSnapshotsWithDB(db, time.Now().UTC())
}

func (r SQLiteObservation) RetentionDays() (int, error) {
	db := r.dbConn()
	if db == nil {
		return 0, errors.New("database is not initialized")
	}
	return healthSnapshotRetentionDays(db)
}

type healthSnapshotStore interface {
	healthSnapshotExecer
	QueryRow(string, ...any) *sql.Row
}

func healthSnapshotRetentionDays(db healthSnapshotStore) (int, error) {
	var raw string
	err := db.QueryRow("SELECT value FROM settings WHERE key = ?", HealthSnapshotRetentionSettingKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultRetentionDays, nil
	}
	if err != nil {
		return 0, err
	}
	days, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || days <= 0 {
		return DefaultRetentionDays, nil
	}
	return days, nil
}

func pruneHealthSnapshots(db healthSnapshotStore, now time.Time) error {
	retentionDays, err := healthSnapshotRetentionDays(db)
	if err != nil {
		return err
	}
	cutoff := now.UTC().AddDate(0, 0, -retentionDays).Format(time.RFC3339)
	_, err = db.Exec("DELETE FROM server_health_snapshots WHERE captured_at < ?", cutoff)
	return err
}

func pruneHealthSnapshotsWithDB(db *sql.DB, now time.Time) error {
	return pruneHealthSnapshots(db, now)
}

func (r SQLiteObservation) PruneHealthSnapshots() error {
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

func (r SQLiteObservation) AcceptCollectedFacts(record CollectedFacts) error {
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
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
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
	if err := r.appendCollectedHistoryTx(tx, record); err != nil {
		return err
	}
	if err := pruneHealthSnapshots(tx, r.now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (r SQLiteObservation) Latest() (map[string]CollectedFacts, error) {
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
	records := map[string]CollectedFacts{}
	for rows.Next() {
		var record CollectedFacts
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

func (r SQLiteObservation) RenameServerTx(tx *sql.Tx, oldName, newName string) error {
	if strings.TrimSpace(oldName) == "" || strings.TrimSpace(newName) == "" || oldName == newName {
		return nil
	}
	if _, err := tx.Exec("UPDATE server_facts SET server_name = ? WHERE server_name = ?", newName, oldName); err != nil {
		return err
	}
	_, err := tx.Exec("UPDATE server_health_snapshots SET server_name = ? WHERE server_name = ?", newName, oldName)
	return err
}

func (r SQLiteObservation) DeleteServerTx(tx *sql.Tx, name string) error {
	if _, err := tx.Exec("DELETE FROM server_facts WHERE server_name = ?", name); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM server_health_snapshots WHERE server_name = ?", name)
	return err
}

func (r SQLiteObservation) appendCollectedHistory(record CollectedFacts) error {
	return r.saveHealthSnapshot(Snapshot{
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

func (r SQLiteObservation) appendCollectedHistoryTx(tx *sql.Tx, record CollectedFacts) error {
	return insertHealthSnapshot(tx, Snapshot{
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

func (r SQLiteObservation) saveHealthSnapshot(record Snapshot) error {
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
	if err := insertHealthSnapshot(db, record); err != nil {
		return err
	}
	return r.PruneHealthSnapshots()
}

type healthSnapshotExecer interface {
	Exec(string, ...any) (sql.Result, error)
}

func insertHealthSnapshot(exec healthSnapshotExecer, record Snapshot) error {
	var rebootValue any
	if record.RebootRequired != nil {
		rebootValue = boolToInt(*record.RebootRequired)
	}
	_, err := exec.Exec(`
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
	return err
}

func (r SQLiteObservation) History(from, to, serverName string) ([]Snapshot, error) {
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
	var records []Snapshot
	for rows.Next() {
		var record Snapshot
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
