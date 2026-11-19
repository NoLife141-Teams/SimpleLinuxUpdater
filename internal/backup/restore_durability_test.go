package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"debian-updater/internal/auth"
	"debian-updater/internal/jobs"
	"debian-updater/internal/maintenance"
)

const (
	originalRestoreServer = "slu-original-inventory"
	archivedRestoreServer = "slu-archived-inventory"
	originalRestoreTrust  = "# original trust fixture\n"
	archivedRestoreTrust  = "# archived trust fixture\n"
)

type durabilityFixture struct {
	service *Service
	files   map[string]string
	db      string
	hosts   string
}

func newDurabilityFixture(t *testing.T, root string, runtime testRestoredRuntime) durabilityFixture {
	t.Helper()
	archive := filepath.Join(root, "incoming")
	trust := filepath.Join(root, "ssh")
	for _, dir := range []string{archive, trust} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	f := durabilityFixture{
		db: filepath.Join(root, "servers.db"), hosts: filepath.Join(trust, "known_hosts"),
		files: map[string]string{
			"servers.db":  filepath.Join(archive, "servers.db"),
			"config.json": filepath.Join(archive, "config.json"),
			"known_hosts": filepath.Join(archive, "known_hosts"),
		},
	}
	for path, name := range map[string]string{f.db: originalRestoreServer, f.files["servers.db"]: archivedRestoreServer} {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`CREATE TABLE servers(name TEXT PRIMARY KEY, pass_enc TEXT, key_enc TEXT);
			CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);
			CREATE TABLE sessions(token TEXT PRIMARY KEY);
			INSERT INTO servers VALUES(?, '', '')`, name)
		if err := errors.Join(err, db.Close()); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		f.hosts: originalRestoreTrust, f.files["known_hosts"]: archivedRestoreTrust,
		f.files["config.json"]: `{"encryption_key":"fixture"}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	fs := restoreFiles{}
	for _, path := range []string{f.db, f.hosts, trust, root, filepath.Dir(root)} {
		if err := fs.syncPath(path); err != nil {
			t.Fatal(err)
		}
	}
	f.service = NewService(ServiceDeps{
		DBPath:              func() string { return f.db },
		KnownHostsWritePath: func() (string, error) { return f.hosts, nil },
		EnsureSchema: func(db *sql.DB) error {
			if err := jobs.EnsureSchema(db); err != nil {
				return err
			}
			return auth.EnsureSchema(db)
		},
		DecodeEncryptionKey:  func(string) ([]byte, error) { return []byte("fixture"), nil },
		CurrentEncryptionKey: func() []byte { return []byte("fixture") },
		DecryptSecretWithKey: func(v string, _ []byte) (string, error) { return v, nil },
		RestoredRuntime:      runtime,
		TempDir:              func() string { return archive },
	})
	return f
}

func requireDurableRestoreState(t *testing.T, dbPath, hostsPath, inventory, trust string, pending bool) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var check, found string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("SQLite integrity check failed: %q %v", check, err)
	}
	if err := db.QueryRow("SELECT name FROM servers").Scan(&found); err != nil || found != inventory {
		t.Fatalf("inventory mismatch: %q %v", found, err)
	}
	data, err := os.ReadFile(hostsPath)
	if err != nil || string(data) != trust {
		t.Fatal("host-key evidence mismatch")
	}
	coord := maintenance.NewCoordinator(maintenance.Deps{
		Store:           maintenance.SQLiteStore{DB: func() *sql.DB { return db }},
		RecoveryPending: func() (bool, error) { return RestoreRecoveryPending(dbPath) },
	})
	if err := coord.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if coord.Snapshot().RecoveryRequired != pending {
		t.Fatalf("recovery state = %+v, want recovery required %v", coord.Snapshot(), pending)
	}
	lease, decision := coord.TryShared(maintenance.WorkInteractive)
	if lease != nil {
		lease.Close()
	}
	if decision.Allowed == pending {
		t.Fatalf("work admission after startup = %v, want %v", decision.Allowed, !pending)
	}
}

func requireDurableRollbackEvidence(t *testing.T, dbPath, hostsPath string) {
	t.Helper()
	line, err := os.ReadFile(RestoreRecoveryMarkerPath(dbPath))
	if err != nil {
		t.Fatal("durable recovery marker unavailable")
	}
	snapshot := strings.TrimSpace(string(line))
	if filepath.Base(snapshot) != snapshot || !strings.HasPrefix(snapshot, "slu-restore-rollback-") {
		t.Fatal("invalid rollback directory identity")
	}
	manifest, err := os.ReadFile(filepath.Join(filepath.Dir(dbPath), snapshot, "recovery.json"))
	if err != nil {
		t.Fatal("rollback manifest unavailable")
	}
	var entries []fileRestoreSnapshot
	if err := json.Unmarshal(manifest, &entries); err != nil {
		t.Fatal(err)
	}
	var originalDB, originalHosts string
	for _, entry := range entries {
		if !entry.Exists {
			continue
		}
		switch entry.Target {
		case dbPath:
			originalDB = entry.BackupPath
		case hostsPath:
			originalHosts = entry.BackupPath
		}
	}
	if originalDB == "" || originalHosts == "" {
		t.Fatal("rollback manifest did not include SQLite and host trust")
	}
	requireDurableRestoreState(t, originalDB, originalHosts, originalRestoreServer, originalRestoreTrust, false)
}

func TestRestoreSynchronizesBeforeReplacementAndKeepsEvidenceOnFailedRollback(t *testing.T) {
	for _, step := range []string{"rollback snapshot", "rollback manifest", "forward host keys", "forward trust directory", "rollback database"} {
		t.Run(step, func(t *testing.T) {
			root := t.TempDir()
			reloads := 0
			f := newDurabilityFixture(t, root, testRestoredRuntime{reload: func(context.Context) error {
				reloads++
				if step == "rollback database" {
					return errors.New("force rollback")
				}
				return nil
			}})
			injected := false
			f.service.syncRestoreFile = func(file *os.File) error {
				path := file.Name()
				temporary := strings.HasPrefix(filepath.Base(path), ".restore-")
				if !injected && ((step == "rollback snapshot" && filepath.Base(path) == "snapshot-0") ||
					(step == "rollback manifest" && filepath.Base(path) == "recovery.json") ||
					(step == "forward host keys" && temporary && filepath.Dir(path) == filepath.Dir(f.hosts)) ||
					(step == "forward trust directory" && path == filepath.Dir(f.hosts)) ||
					(step == "rollback database" && temporary && filepath.Dir(path) == root && reloads == 1)) {
					injected = true
					return syscall.EIO
				}
				return file.Sync()
			}
			err := f.service.applyArchiveFiles(t.Context(), f.files, RestoreOptions{})
			incomplete := false
			switch step {
			case "rollback database":
				var recovery *IncompleteRecoveryError
				incomplete = errors.As(err, &recovery)
				if !incomplete || !errors.Is(recovery.Recovery, syscall.EIO) || reloads != 1 {
					t.Fatalf("failed rollback = %v, reload count = %d", err, reloads)
				}
			default:
				if !errors.Is(err, syscall.EIO) || reloads == 0 {
					t.Fatalf("sync failure = %v, reload count = %d", err, reloads)
				}
			}
			if incomplete {
				requireDurableRestoreState(t, f.db, f.hosts, archivedRestoreServer, originalRestoreTrust, true)
				requireDurableRollbackEvidence(t, f.db, f.hosts)
				return
			}
			requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, false)
		})
	}
}

func TestRestoreRecoveryMarkerProtectsReservedEntryAndExistingEvidence(t *testing.T) {
	for _, test := range []string{"host trust conflicts", "recovery already pending"} {
		t.Run(test, func(t *testing.T) {
			root := t.TempDir()
			f := newDurabilityFixture(t, root, testRestoredRuntime{prepare: func() {
				t.Fatal("runtime was paused despite the invalid restore target")
			}})
			markerPath := RestoreRecoveryMarkerPath(f.db)
			pending := test == "recovery already pending"
			if pending {
				if err := os.WriteFile(markerPath, []byte("slu-restore-rollback-existing\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				alias := filepath.Join(root, "alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				f.service.deps.KnownHostsWritePath = func() (string, error) {
					return filepath.Join(alias, filepath.Base(markerPath)), nil
				}
			}
			err := f.service.applyArchiveFiles(t.Context(), f.files, RestoreOptions{})
			var recovery *IncompleteRecoveryError
			if pending {
				if !errors.As(err, &recovery) || recovery.SnapshotDir != "" {
					t.Fatalf("restore with existing recovery = %v", err)
				}
				if raw, err := os.ReadFile(markerPath); err != nil || string(raw) != "slu-restore-rollback-existing\n" {
					t.Fatal("existing recovery evidence was changed")
				}
			} else if err == nil || !strings.Contains(err.Error(), "known_hosts restore target conflicts") {
				t.Fatalf("restore with aliased target = %v", err)
			}
			requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, pending)
		})
	}
}

// Only the disposable QEMU guest invokes this test with a checkpoint. The
// production binary has no power-cut configuration or test synchronization.
func TestRestoreDurabilityGuest(t *testing.T) {
	phase := os.Getenv("SLU_RESTORE_VM_PHASE")
	if phase == "" {
		t.Skip("requires isolated power-cut VM")
	}
	if phase != "before" && phase != "forward" && phase != "rollback" {
		t.Fatal("invalid restore checkpoint")
	}
	owner, err := os.ReadFile("/var/lib/slu-fixture-owner")
	if err != nil || len(owner) != 32 || string(owner) != os.Getenv("SLU_RESTORE_VM_OWNER") {
		t.Fatal("restore qualification requires an owned VM")
	}
	root := filepath.Join("/var/lib/slu-restore-qualification", phase)
	if os.Getenv("SLU_RESTORE_VM_VERIFY") == "1" {
		inventory, trust := originalRestoreServer, originalRestoreTrust
		if phase == "forward" {
			inventory = archivedRestoreServer
		} else if phase == "rollback" {
			trust = archivedRestoreTrust
		}
		db, hosts := filepath.Join(root, "servers.db"), filepath.Join(root, "ssh", "known_hosts")
		requireDurableRestoreState(t, db, hosts, inventory, trust, phase != "before")
		if phase != "before" {
			requireDurableRollbackEvidence(t, db, hosts)
		}
		return
	}
	f := newDurabilityFixture(t, root, testRestoredRuntime{reload: func(context.Context) error {
		if phase == "rollback" {
			return errors.New("fixture requests rollback")
		}
		return nil
	}})
	f.service.syncRestoreFile = func(file *os.File) error {
		cut := phase == "before" && filepath.Base(file.Name()) == "recovery.json"
		if !cut {
			if err := file.Sync(); err != nil {
				return err
			}
		}
		if file.Name() == root && phase != "before" {
			db, dbErr := os.ReadFile(f.db)
			hosts, hostsErr := os.ReadFile(f.hosts)
			if err := errors.Join(dbErr, hostsErr); err != nil {
				return err
			}
			cut = (phase == "forward" && bytes.Contains(db, []byte(archivedRestoreServer)) && string(hosts) == originalRestoreTrust) ||
				(phase == "rollback" && bytes.Contains(db, []byte(originalRestoreServer)) && string(hosts) == archivedRestoreTrust)
		}
		if cut {
			if err := os.WriteFile(filepath.Join(root, "checkpoint"), []byte(phase), 0600); err != nil {
				return err
			}
			for {
				time.Sleep(time.Second)
			}
		}
		return nil
	}
	t.Fatalf("restore returned before its checkpoint: %v", f.service.applyArchiveFiles(t.Context(), f.files, RestoreOptions{}))
}
