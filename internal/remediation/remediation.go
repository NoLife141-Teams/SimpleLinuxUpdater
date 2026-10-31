// Package remediation owns immutable maintenance evidence and independent CVE verification.
package remediation

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"debian-updater/internal/vulnerabilities"
)

type Plan struct {
	TargetIdentity     string                      `json:"-"`
	VerificationScans  []string                    `json:"verification_scans,omitempty"`
	Inventory          json.RawMessage             `json:"-"`
	VerificationServer string                      `json:"verification_server,omitempty"`
	UpdateStatus       string                      `json:"update_status,omitempty"`
	ID                 string                      `json:"id"`
	ServerKey          string                      `json:"server_key"`
	Server             string                      `json:"server"`
	JobID              string                      `json:"job_id"`
	Generation         uint64                      `json:"approval_generation"`
	Digest             string                      `json:"digest"`
	Configuration      string                      `json:"-"`
	CreatedAt          string                      `json:"created_at"`
	Actor              string                      `json:"actor"`
	Snapshot           json.RawMessage             `json:"transaction"`
	Before             vulnerabilities.Observation `json:"-"`
	BeforeScanID       string                      `json:"before_scan_id"`
	BeforeAt           string                      `json:"before_at,omitempty"`
	State              string                      `json:"state"`
	ScanID             string                      `json:"scan_id,omitempty"`
	Message            string                      `json:"message,omitempty"`
	Completion         map[string]any              `json:"maintenance,omitempty"`
	Comparison         *Comparison                 `json:"comparison,omitempty"`
}

// DiskPlan preserves evidence that is deliberately omitted from ordinary API reads.
type diskPlan struct {
	TargetIdentity string
	Inventory      json.RawMessage
	Plan
	Configuration string
	Before        vulnerabilities.Observation
}

func marshal(p Plan) ([]byte, error) {
	return json.Marshal(diskPlan{Plan: p, Configuration: p.Configuration, Before: p.Before, Inventory: p.Inventory, TargetIdentity: p.TargetIdentity})
}
func unmarshal(b []byte) (Plan, error) {
	var d diskPlan
	err := json.Unmarshal(b, &d)
	d.Plan.TargetIdentity = d.TargetIdentity
	d.Plan.Inventory = d.Inventory
	d.Plan.Before = d.Before
	d.Plan.Configuration = d.Configuration
	return d.Plan, err
}
func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

type Repository struct{ DB func() *sql.DB }

func EnsureSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS remediation_servers (id TEXT PRIMARY KEY, server_name TEXT UNIQUE);
 CREATE TABLE IF NOT EXISTS remediation_plans (id TEXT PRIMARY KEY, server_key TEXT NOT NULL, job_id TEXT NOT NULL, state TEXT NOT NULL, body TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS remediation_plan_job ON remediation_plans(job_id);
 CREATE TABLE IF NOT EXISTS vulnerability_risk_reviews (id TEXT PRIMARY KEY, server_key TEXT NOT NULL, body TEXT NOT NULL);`)
	return err
}
func RenameServerTx(tx *sql.Tx, oldName, newName string) error {
	_, err := tx.Exec(`UPDATE remediation_servers SET server_name=? WHERE server_name=?`, newName, oldName)
	return err
}
func DeleteServerTx(tx *sql.Tx, name string) error {
	_, err := tx.Exec(`UPDATE remediation_servers SET server_name=NULL WHERE server_name=?`, name)
	return err
}
func (r Repository) ServerKey(ctx context.Context, name string) (string, error) {
	_, err := r.DB().ExecContext(ctx, `INSERT INTO remediation_servers(id,server_name) VALUES(?,?) ON CONFLICT(server_name) DO NOTHING`, ID(), name)
	if err != nil {
		return "", err
	}
	var id string
	err = r.DB().QueryRowContext(ctx, `SELECT id FROM remediation_servers WHERE server_name=?`, name).Scan(&id)
	return id, err
}
func (r Repository) ServerName(ctx context.Context, key string) (string, error) {
	var name sql.NullString
	err := r.DB().QueryRowContext(ctx, `SELECT server_name FROM remediation_servers WHERE id=?`, key).Scan(&name)
	if err == nil && !name.Valid {
		err = sql.ErrNoRows
	}
	return name.String, err
}
func (r Repository) Insert(ctx context.Context, p Plan) error {
	body, err := marshal(p)
	if err != nil {
		return err
	}
	tx, err := r.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE remediation_plans SET state='superseded' WHERE job_id=? AND state='prepared'`, p.JobID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO remediation_plans(id,server_key,job_id,state,body) VALUES(?,?,?,?,?)`, p.ID, p.ServerKey, p.JobID, p.State, string(body)); err != nil {
		return err
	}
	return tx.Commit()
}
func (r Repository) Load(ctx context.Context, id string) (Plan, error) {
	var body, state string
	err := r.DB().QueryRowContext(ctx, `SELECT body,state FROM remediation_plans WHERE id=?`, id).Scan(&body, &state)
	if err != nil {
		return Plan{}, err
	}
	p, err := unmarshal([]byte(body))
	p.State = state
	return p, err
}

// Save is a compare-and-swap: retry, recovery, and completion cannot overwrite a newer state.
func (r Repository) Save(ctx context.Context, p Plan, expected string) error {
	b, err := marshal(p)
	if err != nil {
		return err
	}
	result, err := r.DB().ExecContext(ctx, `UPDATE remediation_plans SET state=?,body=? WHERE id=? AND state=?`, p.State, string(b), p.ID, expected)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("remediation state changed")
	}
	return err
}
func (r Repository) List(ctx context.Context, key string) ([]Plan, error) {
	rows, err := r.DB().QueryContext(ctx, `SELECT body,state FROM remediation_plans WHERE (?='' OR server_key=?) ORDER BY rowid DESC LIMIT 100`, key, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		var body, state string
		if err = rows.Scan(&body, &state); err != nil {
			return nil, err
		}
		p, e := unmarshal([]byte(body))
		if e != nil {
			return nil, e
		}
		p.State = state
		out = append(out, p)
	}
	return out, rows.Err()
}

type Outcome struct {
	Package       string                      `json:"package"`
	CVE           string                      `json:"cve"`
	State         string                      `json:"state"`
	BeforeVersion string                      `json:"before_version,omitempty"`
	AfterVersion  string                      `json:"after_version,omitempty"`
	Resolution    *vulnerabilities.Resolution `json:"resolution,omitempty"`
}
type Comparison struct {
	BeforeScanID       string         `json:"before_scan_id"`
	AfterScanID        string         `json:"after_scan_id"`
	VerifiedAt         string         `json:"verified_at"`
	State              string         `json:"state"`
	Counts             map[string]int `json:"counts"`
	FullyCorrectedCVEs int            `json:"fully_corrected_cves"`
	Outcomes           []Outcome      `json:"outcomes"`
}

// Compare uses only the canonical admitted package resolution ledger. It never infers closure from maintenance or absence alone.
func Compare(before vulnerabilities.Observation, scan vulnerabilities.Scan) Comparison {
	out := Comparison{AfterScanID: scan.ID, VerifiedAt: scan.FinishedAt, State: "inconclusive", Counts: map[string]int{}, Outcomes: []Outcome{}}
	if before.Accepted != nil {
		out.BeforeScanID = before.Accepted.ID
	}
	accepted := vulnerabilities.Accept(before, scan)
	after := map[string]vulnerabilities.Finding{}
	for _, f := range accepted.Findings {
		after[f.Package+"\x00"+f.ID] = f
	}
	observed := map[string]bool{}
	if scan.State == "complete" || scan.State == "partial" {
		for _, f := range scan.Findings {
			observed[f.Package+"\x00"+f.ID] = true
		}
	}
	previous := map[string]bool{}
	closed := map[string]bool{}
	for _, f := range before.Findings {
		if f.ResolvedAt != "" {
			continue
		}
		key := f.Package + "\x00" + f.ID
		previous[key] = true
		a := after[key]
		o := Outcome{Package: f.Package, CVE: f.ID, BeforeVersion: f.InstalledVersion, AfterVersion: a.InstalledVersion, State: "not_reverified"}
		if a.Resolution != nil && a.Resolution.ScanID == scan.ID && a.ResolvedAt != "" {
			o.Resolution = a.Resolution
			o.AfterVersion = a.Resolution.InstalledVersion
			switch a.Resolution.Reason {
			case "package_removed":
				o.State = "package_removed"
			case "installed_version_changed":
				o.State = "corrected"
			default:
				o.State = "requalified"
			}
		} else if observed[key] && scan.ID != "" && (scan.State == "complete" || scan.State == "partial") {
			o.State = "still_present"
			if a.Assessment.VendorStatus != f.Assessment.VendorStatus || a.Runtime.Execution != f.Runtime.Execution {
				o.State = "requalified"
			}
		}
		out.Counts[o.State]++
		out.Outcomes = append(out.Outcomes, o)
		if _, ok := closed[f.ID]; !ok {
			closed[f.ID] = true
		}
		if o.State != "corrected" && o.State != "package_removed" {
			closed[f.ID] = false
		}
	}
	for _, f := range scan.Findings {
		if !previous[f.Package+"\x00"+f.ID] {
			out.Outcomes = append(out.Outcomes, Outcome{Package: f.Package, CVE: f.ID, State: "newly_observed", AfterVersion: f.InstalledVersion})
			out.Counts["newly_observed"]++
			closed[f.ID] = false
		}
	}
	for _, yes := range closed {
		if yes {
			out.FullyCorrectedCVEs++
		}
	}
	if scan.State == "complete" || scan.State == "partial" {
		out.State = scan.State
	}
	return out
}

type Review struct {
	ID           string `json:"id"`
	Package      string `json:"package"`
	CVE          string `json:"cve"`
	Owner        string `json:"owner"`
	Reason       string `json:"reason"`
	Decision     string `json:"decision"`
	Mitigation   string `json:"mitigation"`
	ReevaluateAt string `json:"reevaluate_at"`
	Actor        string `json:"actor"`
	CreatedAt    string `json:"created_at"`
}

func (v Review) Valid(now time.Time) bool {
	at, err := time.Parse(time.RFC3339, v.ReevaluateAt)
	return err == nil && at.After(now) && strings.TrimSpace(v.Package) != "" && len(v.Package) <= 300 && strings.TrimSpace(v.Owner) != "" && len(v.Owner) <= 200 && strings.TrimSpace(v.Reason) != "" && len(v.Reason) <= 4000 && strings.TrimSpace(v.Decision) != "" && len(v.Decision) <= 4000 && len(v.Mitigation) <= 4000 && len(v.CVE) <= 100
}
func (r Repository) SaveReview(ctx context.Context, key, name string, v Review) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	result, err := r.DB().ExecContext(ctx, `INSERT INTO vulnerability_risk_reviews(id,server_key,body) SELECT ?,id,? FROM remediation_servers WHERE id=? AND server_name=?`, v.ID, string(b), key, name)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return sql.ErrNoRows
	}
	return err
}
func (r Repository) Reviews(ctx context.Context, key string) ([]Review, error) {
	rows, err := r.DB().QueryContext(ctx, `SELECT body FROM vulnerability_risk_reviews WHERE server_key=? ORDER BY rowid DESC LIMIT 100`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Review{}
	for rows.Next() {
		var b string
		var v Review
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(b), &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r Repository) Pending(ctx context.Context) ([]Plan, error) {
	rows, err := r.DB().QueryContext(ctx, `SELECT body,state FROM remediation_plans WHERE state IN ('prepared','queued','scanning') ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		var b, state string
		if err = rows.Scan(&b, &state); err != nil {
			return nil, err
		}
		p, e := unmarshal([]byte(b))
		if e != nil {
			return nil, e
		}
		p.State = state
		out = append(out, p)
	}
	return out, rows.Err()
}
