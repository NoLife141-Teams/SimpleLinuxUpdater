// Package postchecks owns versioned application expectations and their assignment.
package postchecks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

var ErrConflict = errors.New("post-check profiles changed; reload before saving")
var ErrAssignmentConflict = errors.New("resolve conflicting direct post-check assignments in Admin before renaming this Server")
var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
var unitName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.@:-]{0,199}\.(service|socket|target|timer|mount|path)$`)

type Check struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	Unit            string `json:"unit,omitempty"`
	URL             string `json:"url,omitempty"`
	ExpectedStatus  int    `json:"expected_status,omitempty"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	IntervalSeconds int    `json:"interval_seconds"`
	Mode            string `json:"mode"`
}
type Profile struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Revision      int64    `json:"revision"`
	TargetServers []string `json:"target_servers"`
	TargetTags    []string `json:"target_tags"`
	Checks        []Check  `json:"checks"`
}
type Config struct {
	Revision int64     `json:"revision"`
	Profiles []Profile `json:"profiles"`
}
type Snapshot struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Revision int64   `json:"revision"`
	Checks   []Check `json:"checks"`
}
type Result struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Mode       string `json:"mode"`
	Passed     bool   `json:"passed"`
	Attempts   int    `json:"attempts"`
	DurationMS int64  `json:"duration_ms"`
	Details    string `json:"details"`
}

func EnsureSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS application_postchecks (id INTEGER PRIMARY KEY CHECK(id=1), revision INTEGER NOT NULL, config_json TEXT NOT NULL); INSERT OR IGNORE INTO application_postchecks VALUES(1,0,'{"revision":0,"profiles":[]}')`)
	return err
}
func Load(ctx context.Context, db *sql.DB) (Config, error) {
	var c Config
	var raw string
	if db == nil {
		return c, errors.New("post-check persistence unavailable")
	}
	if err := db.QueryRowContext(ctx, `SELECT config_json FROM application_postchecks WHERE id=1`).Scan(&raw); err != nil {
		return c, err
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return c, err
	}
	if c.Profiles == nil {
		c.Profiles = []Profile{}
	}
	return c, nil
}
func Save(ctx context.Context, db *sql.DB, c Config) (Config, error) {
	if c.Profiles == nil {
		c.Profiles = []Profile{}
	}
	if err := Validate(c); err != nil {
		return Config{}, err
	}
	old, err := Load(ctx, db)
	if err != nil {
		return Config{}, err
	}
	if c.Revision != old.Revision {
		return Config{}, ErrConflict
	}
	for i := range c.Profiles {
		p := &c.Profiles[i]
		p.Revision = 1
		for _, prev := range old.Profiles {
			if prev.ID == p.ID {
				p.Revision = prev.Revision
				if !reflect.DeepEqual(prev, *p) {
					p.Revision++
				}
				break
			}
		}
	}
	c.Revision++
	raw, err := json.Marshal(c)
	if err != nil {
		return Config{}, err
	}
	result, err := db.ExecContext(ctx, `UPDATE application_postchecks SET revision=?,config_json=? WHERE id=1 AND revision=?`, c.Revision, string(raw), old.Revision)
	if err != nil {
		return Config{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return Config{}, err
	}
	if n != 1 {
		return Config{}, ErrConflict
	}
	return c, nil
}
func Validate(c Config) error {
	if len(c.Profiles) > 50 {
		return errors.New("at most 50 post-check profiles are allowed")
	}
	ids := map[string]bool{}
	direct := map[string]bool{}
	for _, p := range c.Profiles {
		if !identifier.MatchString(p.ID) || ids[p.ID] {
			return errors.New("profile IDs must be unique identifiers (1–64 characters)")
		}
		ids[p.ID] = true
		if err := validateLabel(p.Name); err != nil {
			return fmt.Errorf("profile name: %w", err)
		}
		if len(p.TargetServers) > 1000 || len(p.TargetTags) > 100 {
			return errors.New("too many profile assignments")
		}
		for _, name := range p.TargetServers {
			if strings.TrimSpace(name) == "" || len(name) > 200 || direct[name] {
				return errors.New("each Server can have only one direct profile assignment")
			}
			direct[name] = true
		}
		for _, tag := range p.TargetTags {
			if strings.TrimSpace(tag) == "" || len(tag) > 200 {
				return errors.New("invalid profile tag")
			}
		}
		if len(p.Checks) == 0 || len(p.Checks) > 10 {
			return errors.New("each profile needs 1–10 checks")
		}
		checkIDs := map[string]bool{}
		totalTimeout := 0
		for _, check := range p.Checks {
			totalTimeout += check.TimeoutSeconds
			if totalTimeout > 600 {
				return errors.New("profile recovery budget must not exceed 600 seconds")
			}
			if err := ValidateCheck(check); err != nil {
				return fmt.Errorf("profile %s: %w", p.ID, err)
			}
			if checkIDs[check.ID] {
				return errors.New("check IDs must be unique within a profile")
			}
			checkIDs[check.ID] = true
		}
	}
	return nil
}
func validateLabel(s string) error {
	if strings.TrimSpace(s) == "" || len(s) > 100 || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return errors.New("use a name of 1–100 printable characters")
	}
	return nil
}
func ValidateCheck(c Check) error {
	if !identifier.MatchString(c.ID) {
		return errors.New("invalid check ID")
	}
	if err := validateLabel(c.Name); err != nil {
		return err
	}
	if c.Mode != "blocking" && c.Mode != "warning" {
		return errors.New("check mode must be blocking or warning")
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > 300 || c.IntervalSeconds < 1 || c.IntervalSeconds > 30 || c.IntervalSeconds > c.TimeoutSeconds {
		return errors.New("check timeout must be 1–300 seconds, interval 1–30 seconds and no greater than timeout")
	}
	switch c.Kind {
	case "systemd_active":
		if !unitName.MatchString(c.Unit) || c.URL != "" || c.ExpectedStatus != 0 {
			return errors.New("systemd check requires a valid unit and no HTTP fields")
		}
	case "http_status":
		u, err := url.Parse(c.URL)
		if err != nil || len(c.URL) > 2048 || u == nil || u.Host == "" || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || strings.IndexFunc(c.URL, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
			return errors.New("HTTP check requires an HTTP(S) URL without credentials, fragments or whitespace")
		}
		// Queries commonly contain access tokens. Configuration and command diagnostics must never carry them.
		if u.RawQuery != "" || u.ForceQuery {
			return errors.New("HTTP check query parameters are not supported; use a public health endpoint")
		}
		if c.Unit != "" || c.ExpectedStatus < 100 || c.ExpectedStatus > 599 {
			return errors.New("HTTP check requires an expected status from 100 to 599 and no systemd unit")
		}
	default:
		return errors.New("unsupported application check kind")
	}
	return nil
}
func Resolve(c Config, name string, tags []string) (*Snapshot, error) {
	direct := []Profile{}
	tagged := []Profile{}
	for _, p := range c.Profiles {
		if slices.Contains(p.TargetServers, name) {
			direct = append(direct, p)
			continue
		}
		for _, tag := range tags {
			if slices.Contains(p.TargetTags, tag) {
				tagged = append(tagged, p)
				break
			}
		}
	}
	candidates := tagged
	if len(direct) > 0 {
		candidates = direct
	}
	if len(candidates) > 1 {
		ids := []string{}
		for _, p := range candidates {
			ids = append(ids, p.ID)
		}
		slices.Sort(ids)
		return nil, fmt.Errorf("conflicting post-check profiles: %s; assign one profile directly to this Server", strings.Join(ids, ", "))
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	p := candidates[0]
	return &Snapshot{ID: p.ID, Name: p.Name, Revision: p.Revision, Checks: append([]Check(nil), p.Checks...)}, nil
}

// RenameServerTx preserves direct assignments through inventory rename; deletion retires them.
func RenameServerTx(tx *sql.Tx, oldName, newName string) error {
	var raw string
	err := tx.QueryRow(`SELECT config_json FROM application_postchecks WHERE id=1`).Scan(&raw)
	if err != nil {
		return err
	}
	var c Config
	if err = json.Unmarshal([]byte(raw), &c); err != nil {
		return err
	}
	if newName != "" {
		for _, source := range c.Profiles {
			if !slices.Contains(source.TargetServers, oldName) {
				continue
			}
			for _, target := range c.Profiles {
				if target.ID != source.ID && slices.Contains(target.TargetServers, newName) {
					return ErrAssignmentConflict
				}
			}
		}
	}
	changed := false
	for i := range c.Profiles {
		p := &c.Profiles[i]
		names := []string{}
		found := false
		for _, name := range p.TargetServers {
			if name == oldName {
				found = true
				name = newName
			}
			if name != "" && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
		if found {
			p.TargetServers = names
			p.Revision++
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err = Validate(c); err != nil {
		return fmt.Errorf("%w: %v", ErrAssignmentConflict, err)
	}
	c.Revision++
	encoded, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE application_postchecks SET revision=?,config_json=? WHERE id=1`, c.Revision, string(encoded))
	return err
}

func CloneSnapshot(s *Snapshot) *Snapshot {
	if s == nil {
		return nil
	}
	copy := *s
	copy.Checks = append([]Check(nil), s.Checks...)
	return &copy
}
