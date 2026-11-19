package notifications

import (
	"context"
	"database/sql"
	"slices"
	"strings"
)

const aptAdmissionPrefix = "condition/" + aptConditionPrefix

// The key is derived from the incident and destination fingerprint. It
// survives outbox retention so older backups cannot reintroduce a delivery.
func aptAdmissionID(fact ConditionFact, fingerprint string) string {
	return "condition/" + fact.ID + "/" + fingerprint
}

func ensureAPTAdmissionSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS notification_apt_admissions (
		identity TEXT PRIMARY KEY NOT NULL
	) WITHOUT ROWID`); err != nil {
		return err
	}
	state, err := readConditionState(tx.QueryRowContext(ctx, conditionStateQuery))
	if err != nil {
		return err
	}
	var legacy []string
	foundLegacy := false
	for identity, admitted := range state.Admissions {
		if strings.HasPrefix(identity, aptAdmissionPrefix) {
			foundLegacy = true
			if admitted {
				legacy = append(legacy, identity)
			}
			delete(state.Admissions, identity)
		}
	}
	if foundLegacy {
		slices.Sort(legacy)
		if err := mergeAPTAdmissionsTx(ctx, tx, legacy); err != nil {
			return err
		}
		if err := saveConditionState(ctx, tx, &state); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func hasAPTAdmission(ctx context.Context, db *sql.DB, identity string) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_apt_admissions WHERE identity=?)`, identity).Scan(&exists)
	return exists, err
}

func loadAPTAdmissions(ctx context.Context, db *sql.DB) ([]string, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT identity FROM notification_apt_admissions ORDER BY identity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var identities []string
	for rows.Next() {
		var identity string
		if err := rows.Scan(&identity); err != nil {
			return nil, err
		}
		identities = append(identities, identity)
	}
	return identities, rows.Err()
}

const insertAPTAdmissionSQL = `INSERT INTO notification_apt_admissions(identity) VALUES(?) ON CONFLICT(identity) DO NOTHING`

func mergeAPTAdmissionsTx(ctx context.Context, tx *sql.Tx, identities []string) error {
	if len(identities) == 0 {
		return nil
	}
	insert, err := tx.PrepareContext(ctx, insertAPTAdmissionSQL)
	if err != nil {
		return err
	}
	defer insert.Close()
	for _, identity := range identities {
		if _, err := insert.ExecContext(ctx, identity); err != nil {
			return err
		}
	}
	return nil
}
