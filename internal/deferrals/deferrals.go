// Package deferrals owns expiring, cumulative package maintenance exceptions.
package deferrals

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

var selectorPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+(:[a-z0-9][a-z0-9-]*)?$`)

func ValidSelector(s string) bool {
	name, arch, hasArch := strings.Cut(s, ":")
	return selectorPattern.MatchString(s) && !strings.HasSuffix(name, "-") && !strings.Contains(name, ".+") && (!hasArch || !strings.HasSuffix(arch, "-"))
}

type Rule struct {
	ID               string `json:"id"`
	ServerName       string `json:"server_name,omitempty"`
	PolicyID         int64  `json:"policy_id,omitempty"`
	Selector         string `json:"selector"`
	Reason           string `json:"reason"`
	Author           string `json:"author"`
	CreatedAt        string `json:"created_at"`
	ExpiresAt        string `json:"expires_at"`
	Origin           string `json:"origin"`
	ExpiresAtDisplay string `json:"expires_at_display,omitempty"`
}

type Snapshot struct {
	Revision string `json:"revision"`
	Rules    []Rule `json:"rules"`
}

func EnsureSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS package_deferrals (id TEXT PRIMARY KEY, server_name TEXT NOT NULL DEFAULT '', policy_id INTEGER NOT NULL DEFAULT 0, selector TEXT NOT NULL, reason TEXT NOT NULL, author TEXT NOT NULL, created_at TEXT NOT NULL, expires_at TEXT NOT NULL, CHECK ((server_name <> '' AND policy_id = 0) OR (server_name = '' AND policy_id > 0))); CREATE INDEX IF NOT EXISTS package_deferrals_server ON package_deferrals(server_name); CREATE INDEX IF NOT EXISTS package_deferrals_policy ON package_deferrals(policy_id); CREATE TABLE IF NOT EXISTS package_deferral_snapshots (revision TEXT PRIMARY KEY, snapshot_json TEXT NOT NULL)`)
	return err
}

// Retain complete immutable evidence even when the human-readable audit preview
// is truncated. Backup/restore carries this table with the rest of SQLite.
func RecordSnapshot(ctx context.Context, db *sql.DB, snapshot Snapshot) error {
	if snapshot.Revision == "" {
		return nil
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT OR IGNORE INTO package_deferral_snapshots(revision,snapshot_json) VALUES(?,?)`, snapshot.Revision, string(raw))
	return err
}

func LoadSnapshot(ctx context.Context, db *sql.DB, revision string) (Snapshot, error) {
	var s Snapshot
	var raw string
	err := db.QueryRowContext(ctx, `SELECT snapshot_json FROM package_deferral_snapshots WHERE revision=?`, revision).Scan(&raw)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal([]byte(raw), &s)
	return s, err
}

func List(ctx context.Context, db *sql.DB) ([]Rule, error) {
	if db == nil {
		return nil, errors.New("package deferral persistence unavailable")
	}
	rows, err := db.QueryContext(ctx, `SELECT id, server_name, policy_id, selector, reason, author, created_at, expires_at FROM package_deferrals ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rules := []Rule{}
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.ID, &r.ServerName, &r.PolicyID, &r.Selector, &r.Reason, &r.Author, &r.CreatedAt, &r.ExpiresAt); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func Create(ctx context.Context, db *sql.DB, r Rule, author string, now time.Time) (Rule, error) {
	if !ValidSelector(r.Selector) {
		return Rule{}, errors.New("use one exact Debian package selector, optionally :architecture")
	}
	r.Reason = strings.TrimSpace(r.Reason)
	if r.Reason == "" || len(r.Reason) > 1000 {
		return Rule{}, errors.New("a reason of 1–1000 bytes is required")
	}
	if (r.ServerName == "") == (r.PolicyID == 0) || r.PolicyID < 0 {
		return Rule{}, errors.New("choose exactly one Server or policy")
	}
	expires, err := time.Parse(time.RFC3339, r.ExpiresAt)
	if err != nil || !expires.After(now) {
		return Rule{}, errors.New("expiration must be a future timestamp with timezone")
	}
	if strings.TrimSpace(author) == "" {
		return Rule{}, errors.New("audit actor is required")
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return Rule{}, err
	}
	r.ID = hex.EncodeToString(id[:])
	r.Author = author
	r.CreatedAt = now.UTC().Format(time.RFC3339Nano)
	r.ExpiresAt = expires.UTC().Format(time.RFC3339Nano)
	r.Origin = ""
	r.ExpiresAtDisplay = ""
	_, err = db.ExecContext(ctx, `INSERT INTO package_deferrals (id,server_name,policy_id,selector,reason,author,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`, r.ID, r.ServerName, r.PolicyID, r.Selector, r.Reason, r.Author, r.CreatedAt, r.ExpiresAt)
	return r, err
}

func Delete(ctx context.Context, db *sql.DB, id string) error {
	r, err := db.ExecContext(ctx, `DELETE FROM package_deferrals WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}

// Resolve includes all matching policy rules. Duplicate selectors retain their
// distinct reasons and expirations; one rule ending never cancels another.
func Resolve(rules []Rule, server string, policies map[int64]string, now time.Time) (Snapshot, error) {
	s := Snapshot{Rules: []Rule{}}
	for _, r := range rules {
		name, policyMatch := policies[r.PolicyID]
		if r.ServerName != server && !policyMatch {
			continue
		}
		expires, err := time.Parse(time.RFC3339Nano, r.ExpiresAt)
		if err != nil || !ValidSelector(r.Selector) {
			return s, fmt.Errorf("invalid persisted package deferral %s", r.ID)
		}
		if !expires.After(now) {
			continue
		}
		if r.ServerName == server {
			r.Origin = "Server: " + server
		} else {
			r.Origin = "Policy: " + name
		}
		r.ExpiresAtDisplay = ""
		s.Rules = append(s.Rules, r)
	}
	slices.SortFunc(s.Rules, func(a, b Rule) int { return strings.Compare(a.ID, b.ID) })
	if len(s.Rules) > 0 {
		raw, _ := json.Marshal(s.Rules)
		sum := sha256.Sum256(raw)
		s.Revision = hex.EncodeToString(sum[:])
	}
	return s, nil
}

// An unqualified rule protects every installed architecture. A qualified rule
// requires matching architecture; missing discovery architecture is ambiguous
// and is conservatively protected until exact transaction data is available.
func Matches(rule, selector string) bool {
	rn, ra, rq := strings.Cut(rule, ":")
	pn, pa, pq := strings.Cut(selector, ":")
	return rn == pn && (!rq || !pq || ra == pa || pa == "all")
}

func (s Snapshot) Selectors() []string {
	r := []string{}
	for _, v := range s.Rules {
		r = append(r, v.Selector)
	}
	slices.Sort(r)
	return slices.Compact(r)
}
func (s Snapshot) Matches(selector string) bool {
	for _, r := range s.Rules {
		if Matches(r.Selector, selector) {
			return true
		}
	}
	return false
}
func (s Snapshot) Deadline() int64 {
	var deadline int64
	for _, r := range s.Rules {
		t, err := time.Parse(time.RFC3339Nano, r.ExpiresAt)
		if err == nil && (deadline == 0 || t.Unix() < deadline) {
			deadline = t.Unix()
		}
	}
	return deadline
}

func RenameServerTx(tx *sql.Tx, oldName, newName string) error {
	if oldName == newName {
		return nil
	}
	if newName == "" {
		_, err := tx.Exec(`DELETE FROM package_deferrals WHERE server_name=?`, oldName)
		return err
	}
	_, err := tx.Exec(`UPDATE package_deferrals SET server_name=? WHERE server_name=?`, newName, oldName)
	return err
}
