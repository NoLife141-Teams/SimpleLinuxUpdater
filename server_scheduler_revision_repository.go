package main

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	policypkg "debian-updater/internal/policies"
	serverpkg "debian-updater/internal/servers"
)

type schedulerRevisionServerRepository struct {
	base serverpkg.SQLiteRepository
	db   func() *sql.DB
}

func newSchedulerRevisionServerRepository(dbProvider func() *sql.DB, encrypt, decrypt func(string) (string, error)) serverpkg.Repository {
	return schedulerRevisionServerRepository{
		base: serverpkg.SQLiteRepository{
			DB:      dbProvider,
			Encrypt: encrypt,
			Decrypt: decrypt,
		},
		db: dbProvider,
	}
}

func (r schedulerRevisionServerRepository) Load() ([]serverpkg.Server, error) {
	return r.base.Load()
}

func (r schedulerRevisionServerRepository) UpdateServerKey(name, key string) error {
	// Credential rotation does not affect scheduler matching state.
	return r.base.UpdateServerKey(name, key)
}

func (r schedulerRevisionServerRepository) Save(servers []serverpkg.Server, txHook serverpkg.TxHook) error {
	changed, err := schedulerServerMatchingStateChanged(r.db, servers)
	if err != nil {
		return err
	}
	combinedHook := txHook
	if changed {
		combinedHook = func(tx *sql.Tx) error {
			if txHook != nil {
				if err := txHook(tx); err != nil {
					return err
				}
			}
			return bumpSchedulerServerStateRevisionIfPresent(tx)
		}
	}
	return r.base.Save(servers, combinedHook)
}

func schedulerServerMatchingStateChanged(dbProvider func() *sql.DB, desired []serverpkg.Server) (bool, error) {
	if dbProvider == nil || dbProvider() == nil {
		return false, fmt.Errorf("compare scheduler server matching state: database is unavailable")
	}
	rows, err := dbProvider().Query("SELECT name, tags FROM servers")
	if err != nil {
		return false, fmt.Errorf("load scheduler server matching state: %w", err)
	}
	defer rows.Close()

	current := make([]string, 0)
	for rows.Next() {
		var name, tags string
		if err := rows.Scan(&name, &tags); err != nil {
			return false, fmt.Errorf("scan scheduler server matching state: %w", err)
		}
		current = append(current, schedulerServerMatchingKey(name, serverpkg.ParseTags(tags)))
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate scheduler server matching state: %w", err)
	}

	wanted := make([]string, 0, len(desired))
	for _, server := range desired {
		wanted = append(wanted, schedulerServerMatchingKey(server.Name, server.Tags))
	}
	sort.Strings(current)
	sort.Strings(wanted)
	if len(current) != len(wanted) {
		return true, nil
	}
	for i := range current {
		if current[i] != wanted[i] {
			return true, nil
		}
	}
	return false, nil
}

func schedulerServerMatchingKey(name string, tags []string) string {
	normalizedTags := serverpkg.ParseTags(serverpkg.JoinTags(tags))
	for i := range normalizedTags {
		normalizedTags[i] = strings.ToLower(strings.TrimSpace(normalizedTags[i]))
	}
	sort.Strings(normalizedTags)
	return strings.ToLower(strings.TrimSpace(name)) + "\x00" + strings.Join(normalizedTags, "\x00")
}

func bumpSchedulerServerStateRevisionIfPresent(tx *sql.Tx) error {
	if tx == nil {
		return fmt.Errorf("bump scheduler server state revision: transaction is unavailable")
	}
	var exists int
	if err := tx.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?",
		policypkg.SchedulerStateRevisionTable,
	).Scan(&exists); err != nil {
		return fmt.Errorf("check scheduler state revision table: %w", err)
	}
	if exists == 0 {
		return nil
	}
	if _, err := tx.Exec(
		"UPDATE "+policypkg.SchedulerStateRevisionTable+" SET revision = revision + 1 WHERE id = 1",
	); err != nil {
		return fmt.Errorf("bump scheduler server state revision: %w", err)
	}
	return nil
}
