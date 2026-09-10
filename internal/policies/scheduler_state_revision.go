package policies

import (
	"database/sql"
	"errors"
)

const SchedulerStateRevisionTable = "update_policy_scheduler_state_revision"

// LoadSchedulerStateRevision returns a durable monotonic generation for every
// mutation that can change scheduler matching/competition semantics. The
// trigger schema is installed lazily when the scheduler first starts so older
// databases upgrade without requiring a separate migration entry point.
func (r *SQLiteRepository) LoadSchedulerStateRevision() (int64, error) {
	if r == nil || r.database() == nil {
		return 0, errors.New("policy repository database is unavailable")
	}
	db := r.database()
	if err := ensureSchedulerStateRevisionSchema(db); err != nil {
		return 0, err
	}
	var revision int64
	if err := db.QueryRow("SELECT revision FROM " + SchedulerStateRevisionTable + " WHERE id = 1").Scan(&revision); err != nil {
		return 0, err
	}
	return revision, nil
}

func ensureSchedulerStateRevisionSchema(db *sql.DB) error {
	if db == nil {
		return errors.New("policy scheduler state revision database is unavailable")
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS update_policy_scheduler_state_revision (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			revision INTEGER NOT NULL DEFAULT 0
		)
	`); err != nil {
		return err
	}
	if _, err := db.Exec(`
		INSERT INTO update_policy_scheduler_state_revision(id, revision)
		VALUES(1, 0)
		ON CONFLICT(id) DO NOTHING
	`); err != nil {
		return err
	}

	for _, trigger := range []string{
		`CREATE TRIGGER IF NOT EXISTS trg_scheduler_state_policy_insert AFTER INSERT ON update_policies BEGIN UPDATE update_policy_scheduler_state_revision SET revision = revision + 1 WHERE id = 1; END`,
		`CREATE TRIGGER IF NOT EXISTS trg_scheduler_state_policy_update AFTER UPDATE ON update_policies BEGIN UPDATE update_policy_scheduler_state_revision SET revision = revision + 1 WHERE id = 1; END`,
		`CREATE TRIGGER IF NOT EXISTS trg_scheduler_state_policy_delete AFTER DELETE ON update_policies BEGIN UPDATE update_policy_scheduler_state_revision SET revision = revision + 1 WHERE id = 1; END`,
		`CREATE TRIGGER IF NOT EXISTS trg_scheduler_state_override_insert AFTER INSERT ON update_policy_overrides BEGIN UPDATE update_policy_scheduler_state_revision SET revision = revision + 1 WHERE id = 1; END`,
		`CREATE TRIGGER IF NOT EXISTS trg_scheduler_state_override_update AFTER UPDATE ON update_policy_overrides BEGIN UPDATE update_policy_scheduler_state_revision SET revision = revision + 1 WHERE id = 1; END`,
		`CREATE TRIGGER IF NOT EXISTS trg_scheduler_state_override_delete AFTER DELETE ON update_policy_overrides BEGIN UPDATE update_policy_scheduler_state_revision SET revision = revision + 1 WHERE id = 1; END`,
	} {
		if _, err := db.Exec(trigger); err != nil {
			return err
		}
	}

	serversExists, err := schedulerStateTableExists(db, "servers")
	if err != nil {
		return err
	}
	if serversExists {
		// Older iterations used row-level INSERT/UPDATE/DELETE triggers for the
		// server table. The production server repository rewrites the full table
		// with DELETE+INSERT, so those triggers treated credential/host-only saves
		// as matching-state mutations. Drop them and let the composed repository
		// bump the generation once when the semantic name+tags set actually
		// changes. Keep a narrow UPDATE trigger as a guard for direct SQL changes.
		for _, legacy := range []string{
			"trg_scheduler_state_server_insert",
			"trg_scheduler_state_server_update",
			"trg_scheduler_state_server_delete",
		} {
			if _, err := db.Exec("DROP TRIGGER IF EXISTS " + legacy); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`
			CREATE TRIGGER IF NOT EXISTS trg_scheduler_state_server_matching_update
			AFTER UPDATE OF name, tags ON servers
			WHEN lower(trim(OLD.name)) IS NOT lower(trim(NEW.name)) OR OLD.tags IS NOT NEW.tags
			BEGIN
				UPDATE update_policy_scheduler_state_revision SET revision = revision + 1 WHERE id = 1;
			END
		`); err != nil {
			return err
		}
		if _, err := db.Exec(`CREATE TRIGGER IF NOT EXISTS trg_scheduler_state_server_availability_update
			AFTER UPDATE OF disabled ON servers WHEN OLD.disabled IS NOT NEW.disabled
			BEGIN UPDATE update_policy_scheduler_state_revision SET revision = revision + 1 WHERE id = 1; END`); err != nil {
			return err
		}
	}

	settingsExists, err := schedulerStateTableExists(db, "settings")
	if err != nil {
		return err
	}
	if settingsExists {
		for _, trigger := range []string{
			`CREATE TRIGGER IF NOT EXISTS trg_scheduler_state_setting_insert AFTER INSERT ON settings WHEN NEW.key IN ('update_policy_global_blackouts', 'app_timezone') BEGIN UPDATE update_policy_scheduler_state_revision SET revision = revision + 1 WHERE id = 1; END`,
			`CREATE TRIGGER IF NOT EXISTS trg_scheduler_state_setting_update AFTER UPDATE OF value ON settings WHEN NEW.key IN ('update_policy_global_blackouts', 'app_timezone') AND OLD.value IS NOT NEW.value BEGIN UPDATE update_policy_scheduler_state_revision SET revision = revision + 1 WHERE id = 1; END`,
			`CREATE TRIGGER IF NOT EXISTS trg_scheduler_state_setting_delete AFTER DELETE ON settings WHEN OLD.key IN ('update_policy_global_blackouts', 'app_timezone') BEGIN UPDATE update_policy_scheduler_state_revision SET revision = revision + 1 WHERE id = 1; END`,
		} {
			if _, err := db.Exec(trigger); err != nil {
				return err
			}
		}
	}
	return nil
}

func schedulerStateTableExists(db *sql.DB, table string) (bool, error) {
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}
