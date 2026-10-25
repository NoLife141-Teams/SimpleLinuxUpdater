package postchecks

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func validCheck() Check {
	return Check{ID: "web", Name: "Website", Kind: "http_status", URL: "https://localhost/health", ExpectedStatus: 200, TimeoutSeconds: 10, IntervalSeconds: 1, Mode: "blocking"}
}
func validConfig() Config {
	return Config{Profiles: []Profile{{ID: "web", Name: "Web applications", TargetTags: []string{"web"}, Checks: []Check{validCheck()}}}}
}
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "postchecks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}
func TestValidationRejectsUnsafeAndUnboundedChecks(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Check)
	}{
		{"shell unit", func(c *Check) {
			c.Kind = "systemd_active"
			c.Unit = "nginx.service;id"
			c.URL = ""
			c.ExpectedStatus = 0
		}},
		{"option unit", func(c *Check) { c.Kind = "systemd_active"; c.Unit = "--root.service"; c.URL = ""; c.ExpectedStatus = 0 }},
		{"bad scheme", func(c *Check) { c.URL = "file:///etc/passwd" }},
		{"credentials", func(c *Check) { c.URL = "https://user:password@localhost/health" }},
		{"query", func(c *Check) { c.URL = "https://localhost/health?token=secret" }},
		{"fragment", func(c *Check) { c.URL = "https://localhost/health#secret" }},
		{"newline", func(c *Check) { c.URL = "https://localhost/\nhealth" }},
		{"empty host", func(c *Check) { c.URL = "http:/health" }},
		{"bad status", func(c *Check) { c.ExpectedStatus = 999 }},
		{"bad interval", func(c *Check) { c.IntervalSeconds = 11 }},
		{"bad timeout", func(c *Check) { c.TimeoutSeconds = 301 }},
		{"bad mode", func(c *Check) { c.Mode = "ignore" }},
		{"bad type", func(c *Check) { c.Kind = "script" }},
		{"bad id", func(c *Check) { c.ID = "x y" }},
		{"bad name", func(c *Check) { c.Name = "\n" }},
		{"mixed", func(c *Check) { c.Unit = "nginx.service" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validCheck()
			tt.change(&c)
			if ValidateCheck(c) == nil {
				t.Fatalf("accepted unsafe check %+v", c)
			}
		})
	}
	c := validCheck()
	c.Kind = "systemd_active"
	c.URL = ""
	c.ExpectedStatus = 0
	c.Unit = "nginx@production.service"
	if err := ValidateCheck(c); err != nil {
		t.Fatal(err)
	}
}
func TestProfilesResolveDirectBeforeTagsAndRejectConflicts(t *testing.T) {
	c := validConfig()
	c.Profiles = append(c.Profiles, Profile{ID: "db", Name: "DB", Revision: 2, TargetTags: []string{"database"}, Checks: []Check{validCheck()}})
	if p, err := Resolve(c, "unmatched", nil); err != nil || p != nil {
		t.Fatalf("unmatched: %+v %v", p, err)
	}
	if p, err := Resolve(c, "a", []string{"web"}); err != nil || p.ID != "web" {
		t.Fatalf("tag: %+v %v", p, err)
	}
	if _, err := Resolve(c, "a", []string{"web", "database"}); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatal(err)
	}
	c.Profiles[1].TargetServers = []string{"a"}
	p, err := Resolve(c, "a", []string{"web", "database"})
	if err != nil || p.ID != "db" || p.Revision != 2 {
		t.Fatalf("direct: %+v %v", p, err)
	}
	copy := CloneSnapshot(p)
	copy.Checks[0].Name = "Changed"
	if p.Checks[0].Name == copy.Checks[0].Name {
		t.Fatal("snapshot shares checks")
	}
	if CloneSnapshot(nil) != nil {
		t.Fatal("nil snapshot")
	}
	c.Profiles[0].TargetServers = []string{"a"}
	if Validate(c) == nil {
		t.Fatal("duplicate direct assignments")
	}
}
func TestProfileValidation(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"duplicate IDs", func(c *Config) { c.Profiles = append(c.Profiles, c.Profiles[0]) }},
		{"bad ID", func(c *Config) { c.Profiles[0].ID = "bad id" }},
		{"empty name", func(c *Config) { c.Profiles[0].Name = "" }},
		{"empty checks", func(c *Config) { c.Profiles[0].Checks = nil }},
		{"duplicate checks", func(c *Config) { c.Profiles[0].Checks = append(c.Profiles[0].Checks, c.Profiles[0].Checks[0]) }},
		{"bad check", func(c *Config) { c.Profiles[0].Checks[0].Mode = "none" }},
		{"bad tag", func(c *Config) { c.Profiles[0].TargetTags = []string{""} }},
		{"bad server", func(c *Config) { c.Profiles[0].TargetServers = []string{""} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.change(&c)
			if Validate(c) == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}
func TestProfilesPersistRevisionsAndInventoryContinuity(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	empty, err := Load(ctx, db)
	if err != nil || empty.Revision != 0 || len(empty.Profiles) != 0 {
		t.Fatalf("empty %+v %v", empty, err)
	}
	c := validConfig()
	c.Profiles[0].TargetServers = []string{"old"}
	saved, err := Save(ctx, db, c)
	if err != nil || saved.Revision != 1 || saved.Profiles[0].Revision != 1 {
		t.Fatalf("save %+v %v", saved, err)
	}
	if _, err = Save(ctx, db, c); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	saved, err = Save(ctx, db, saved)
	if err != nil || saved.Profiles[0].Revision != 1 {
		t.Fatalf("unchanged %+v %v", saved, err)
	}
	saved.Profiles[0].Name = "New name"
	saved, err = Save(ctx, db, saved)
	if err != nil || saved.Profiles[0].Revision != 2 {
		t.Fatalf("revision %+v %v", saved, err)
	}
	for _, names := range [][2]string{{"absent", "new"}, {"old", "new"}, {"new", ""}} {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err = RenameServerTx(tx, names[0], names[1]); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	restored, err := Load(ctx, db)
	if err != nil || len(restored.Profiles[0].TargetServers) != 0 || restored.Profiles[0].Revision != 4 {
		t.Fatalf("continuity %+v %v", restored, err)
	}
	// A standalone database snapshot keeps profiles/revisions across restart or restore.
	backupPath := filepath.Join(t.TempDir(), "restored.db")
	if _, err = db.Exec("VACUUM INTO ?", backupPath); err != nil {
		t.Fatal(err)
	}
	backupDB, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer backupDB.Close()
	if err = EnsureSchema(backupDB); err != nil {
		t.Fatal(err)
	}
	copied, err := Load(ctx, backupDB)
	if err != nil || copied.Revision != restored.Revision || copied.Profiles[0].Revision != 4 || len(copied.Profiles[0].Checks) != 1 {
		t.Fatalf("restored %+v %v", copied, err)
	}

	if _, err = Load(ctx, nil); err == nil {
		t.Fatal("nil database")
	}
	bad := validConfig()
	bad.Profiles[0].Checks = nil
	if _, err = Save(ctx, db, bad); err == nil {
		t.Fatal("invalid save")
	}
}

func TestSavingEmptyProfilesKeepsJSONList(t *testing.T) {
	db := openDB(t)
	saved, err := Save(context.Background(), db, Config{})
	if err != nil || saved.Profiles == nil || len(saved.Profiles) != 0 {
		t.Fatalf("empty profiles %+v %v", saved, err)
	}
	loaded, err := Load(context.Background(), db)
	if err != nil || loaded.Profiles == nil {
		t.Fatalf("loaded empty %+v %v", loaded, err)
	}
}
