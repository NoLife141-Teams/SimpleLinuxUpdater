package servers

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openSchemaTestDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSchemaCreatesServersTableAndIsIdempotent(t *testing.T) {
	db := openSchemaTestDB(t, "servers-schema.db")
	for i := 0; i < 2; i++ {
		if err := EnsureSchema(db); err != nil {
			t.Fatalf("EnsureSchema run %d error = %v", i+1, err)
		}
	}
	assertColumnsExist(t, db, "servers", "name", "host", "port", "user", "pass_enc", "key_enc", "key_path", "tags")
}

func TestSchemaMigratesLegacyServersColumns(t *testing.T) {
	db := openSchemaTestDB(t, "servers-legacy.db")
	if _, err := db.Exec(`
		CREATE TABLE servers (
			name TEXT PRIMARY KEY,
			host TEXT NOT NULL,
			user TEXT NOT NULL,
			pass_enc TEXT NOT NULL
		)
	`); err != nil {
		t.Fatalf("create legacy servers table: %v", err)
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatalf("EnsureSchema() error = %v", err)
	}
	assertColumnsExist(t, db, "servers", "port", "key_enc", "key_path", "tags")
}

func TestSQLiteRepositoryPreservesExistingHostSpelling(t *testing.T) {
	db := openSchemaTestDB(t, "servers-canonical-host.db")
	if err := EnsureSchema(db); err != nil {
		t.Fatalf("EnsureSchema() error = %v", err)
	}
	repo := SQLiteRepository{DB: func() *sql.DB { return db }}
	if err := repo.Save([]Server{
		{Name: "ipv6", Host: "[2001:0DB8:0:0:0:0:0:1]", Port: 22, User: "root"},
		{Name: "dns", Host: "NODE.EXAMPLE", Port: 22, User: "root"},
	}, nil); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	var ipv6Host, dnsHost string
	if err := db.QueryRow("SELECT host FROM servers WHERE name = 'ipv6'").Scan(&ipv6Host); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT host FROM servers WHERE name = 'dns'").Scan(&dnsHost); err != nil {
		t.Fatal(err)
	}
	if ipv6Host != "[2001:0DB8:0:0:0:0:0:1]" || dnsHost != "NODE.EXAMPLE" {
		t.Fatalf("persisted hosts = %q, %q, want unchanged legacy spelling", ipv6Host, dnsHost)
	}

	if _, err := db.Exec("UPDATE servers SET host = ? WHERE name = 'ipv6'", "2001:0db8:0:0:0:0:0:1"); err != nil {
		t.Fatal(err)
	}
	loaded, err := repo.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded) != 2 || loaded[1].Name != "ipv6" || loaded[1].Host != "2001:0db8:0:0:0:0:0:1" {
		t.Fatalf("Load() = %+v, want preserved legacy IPv6 spelling", loaded)
	}
	if err := db.QueryRow("SELECT host FROM servers WHERE name = 'ipv6'").Scan(&ipv6Host); err != nil {
		t.Fatal(err)
	}
	if ipv6Host != "2001:0db8:0:0:0:0:0:1" {
		t.Fatalf("Load() rewrote stored host to %q, want non-destructive read", ipv6Host)
	}
}

func assertColumnsExist(t *testing.T, db *sql.DB, table string, names ...string) {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s) error = %v", table, err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		seen[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}
	for _, name := range names {
		if !seen[name] {
			t.Fatalf("table %s missing column %s; columns=%v", table, name, seen)
		}
	}
}
