package deferrals

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestExactSelectorsAndArchitecture(t *testing.T) {
	for _, s := range []string{"openssl", "libssl3t64:amd64", "libstdc++6:i386", "linux-image-6.1-amd64", "ab:all"} {
		if !ValidSelector(s) {
			t.Errorf("rejected %q", s)
		}
	}
	for _, s := range []string{"", "a", " openssl", "openssl ", "openssl*", "openssl?", "openssl/stable", "openssl=1.0", "-y", "--help", "../openssl", "openssl:amd64:extra", "openssl:", "openssl:amd64-", "openssl.+", "x\nfoo"} {
		if ValidSelector(s) {
			t.Errorf("accepted %q", s)
		}
	}
	for _, tt := range []struct {
		rule, pkg string
		want      bool
	}{{"openssl", "openssl:amd64", true}, {"openssl:amd64", "openssl:i386", false}, {"openssl:amd64", "openssl:amd64", true}, {"openssl:i386", "openssl", true}, {"openssl", "openssl-tools", false}, {"openssl:amd64", "openssl:all", true}} {
		if got := Matches(tt.rule, tt.pkg); got != tt.want {
			t.Errorf("match %s %s=%v", tt.rule, tt.pkg, got)
		}
	}
}

func TestPersistenceCumulativeExpirationRenameAndBackup(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "rules.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for range 2 {
		if err = EnsureSchema(db); err != nil {
			t.Fatal(err)
		}
	}
	direct, err := Create(ctx, db, Rule{ServerName: "web", Selector: "openssl:amd64", Reason: "Investigating compatibility", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), Author: "spoofed"}, "operator", now)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := Create(ctx, db, Rule{PolicyID: 7, Selector: "openssl", Reason: "Vendor investigation", ExpiresAt: now.Add(2 * time.Hour).Format(time.RFC3339)}, "operator", now)
	if err != nil {
		t.Fatal(err)
	}
	if direct.Author != "operator" || direct.CreatedAt == "" {
		t.Fatal(direct)
	}
	rules, err := List(ctx, db)
	if err != nil || len(rules) != 2 {
		t.Fatal(rules, err)
	}
	s, err := Resolve(rules, "web", map[int64]string{7: "Production (revision 2)"}, now)
	if err != nil || len(s.Rules) != 2 || len(s.Selectors()) != 2 || !s.Matches("openssl:i386") || s.Deadline() != now.Add(time.Hour).Unix() {
		t.Fatal(s, err)
	}
	first := s.Revision
	s, err = Resolve(rules, "web", map[int64]string{7: "Production (revision 2)"}, now.Add(time.Hour))
	if err != nil || len(s.Rules) != 1 || s.Revision == first || s.Deadline() != now.Add(2*time.Hour).Unix() {
		t.Fatal(s, err)
	}
	s, _ = Resolve(rules, "web", map[int64]string{7: "Production"}, now.Add(2*time.Hour))
	if s.Revision != "" || len(s.Rules) != 0 {
		t.Fatal(s)
	}
	s, _ = Resolve(rules, "other", nil, now)
	if len(s.Rules) != 0 {
		t.Fatal(s)
	}
	broken := direct
	broken.ExpiresAt = "bad"
	if _, err = Resolve([]Rule{broken}, "web", nil, now); err == nil {
		t.Fatal("accepted corrupt expiry")
	}
	broken = direct
	broken.Selector = "*"
	if _, err = Resolve([]Rule{broken}, "web", nil, now); err == nil {
		t.Fatal("accepted corrupt selector")
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if _, err = db.Exec(`VACUUM INTO ?`, backup); err != nil {
		t.Fatal(err)
	}
	copyDB, err := sql.Open("sqlite", backup)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	if err = EnsureSchema(copyDB); err != nil {
		t.Fatal(err)
	}
	restored, err := List(ctx, copyDB)
	if err != nil || len(restored) != 2 {
		t.Fatal(restored, err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = RenameServerTx(tx, "web", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rules, _ = List(ctx, db)
	s, _ = Resolve(rules, "renamed", nil, now)
	if len(s.Rules) != 1 || s.Rules[0].ID != direct.ID {
		t.Fatal(s)
	}
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = RenameServerTx(tx, "renamed", ""); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rules, _ = List(ctx, db)
	if len(rules) != 1 || rules[0].ID != policy.ID {
		t.Fatal(rules)
	}
	if err = Delete(ctx, db, policy.ID); err != nil {
		t.Fatal(err)
	}
	if err = Delete(ctx, db, policy.ID); err != sql.ErrNoRows {
		t.Fatal(err)
	}
}

func TestCreateRejectsIncompleteOrUnsafeRules(t *testing.T) {
	now := time.Now().UTC()
	base := Rule{ServerName: "web", Selector: "openssl", Reason: "Investigation", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}
	for _, tt := range []struct {
		name   string
		change func(*Rule)
		actor  string
	}{{"selector", func(r *Rule) { r.Selector = "openssl*" }, "admin"}, {"reason", func(r *Rule) { r.Reason = " " }, "admin"}, {"target", func(r *Rule) { r.PolicyID = 1 }, "admin"}, {"no target", func(r *Rule) { r.ServerName = "" }, "admin"}, {"past", func(r *Rule) { r.ExpiresAt = now.Format(time.RFC3339) }, "admin"}, {"missing expiry", func(r *Rule) { r.ExpiresAt = "" }, "admin"}, {"no actor", func(*Rule) {}, ""}} {
		t.Run(tt.name, func(t *testing.T) {
			r := base
			tt.change(&r)
			if _, err := Create(context.Background(), nil, r, tt.actor, now); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if _, err := List(context.Background(), nil); err == nil {
		t.Fatal("nil db accepted")
	}
}
