package vulnerabilities

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

// RecordPositiveFindings retains approval-derived installed matches and their
// watches. The caller holds the Server admission/state lock and the existing
// maintenance lease through commit. It never changes scan attempts, accepted
// inventory, coverage, or package proofs, and absence never resolves a finding.
func (r Repository) RecordPositiveFindings(ctx context.Context, name string, findings []Finding) error {
	if len(findings) == 0 {
		return nil
	}
	tx, err := r.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previous Observation
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT observation FROM server_vulnerability_observations WHERE server_name = ?`, name).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if err = json.Unmarshal([]byte(raw), &previous); err != nil {
			return err
		}
	}
	byKey := map[string]Finding{}
	for _, f := range previous.Findings {
		byKey[f.Package+"\x00"+f.ID] = f
	}
	changed := false
	for _, f := range findings {
		at, err := time.Parse(time.RFC3339, f.LastSeen)
		if err != nil || f.ID == "" || f.Package == "" || f.InstalledVersion == "" || previous.SupersedesPackageEvidence(f.Package, f.LastSeen) {
			continue
		}
		key := f.Package + "\x00" + f.ID
		old := byKey[key]
		if before, e := time.Parse(time.RFC3339, old.LastSeen); e == nil && !at.After(before) {
			continue
		}
		byKey[key] = retainFindingHistory(f, old, f.LastSeen)
		changed = true
	}
	if !changed {
		return nil
	}
	previous.Findings = make([]Finding, 0, len(byKey))
	for _, f := range byKey {
		previous.Findings = append(previous.Findings, f)
	}
	sort.Slice(previous.Findings, func(i, j int) bool {
		a, b := previous.Findings[i], previous.Findings[j]
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		return a.ID < b.ID
	})
	NormalizeLegacyAvailability(&previous)
	encoded, err := json.Marshal(previous)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO server_vulnerability_observations(server_name, observation) VALUES (?, ?) ON CONFLICT(server_name) DO UPDATE SET observation = excluded.observation`, name, string(encoded)); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return tx.Commit()
}
