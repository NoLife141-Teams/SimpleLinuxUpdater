package servers

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestServerInventoryCommandsCreateUpdateDeleteOutcomes(t *testing.T) {
	repo := &fakeRepo{}
	svc, _, _, statusMap := newTestService(repo, nil)
	commands := NewCommandService(svc)

	created := commands.CreateServer(Server{
		Name: " Alpha ",
		Host: " alpha.example ",
		User: "root",
		Tags: []string{" prod ", "prod"},
	})
	if !created.Succeeded() || created.Server == nil {
		t.Fatalf("CreateServer() = %+v, want success with server", created)
	}
	if created.Server.Name != "Alpha" || created.Audit.Action != "server.create" || created.Audit.TargetName != "Alpha" || created.Audit.Status != "success" {
		t.Fatalf("CreateServer() result/audit = %+v", created)
	}
	if created.Audit.Meta["host"] != "alpha.example" || created.Audit.Meta["port"] != 22 || created.Audit.Meta["tags_count"] != 1 {
		t.Fatalf("CreateServer() audit meta = %+v, want host/port/tags_count", created.Audit.Meta)
	}

	duplicate := commands.CreateServer(Server{Name: "alpha", Host: "other.example", User: "root"})
	if duplicate.Outcome != CommandOutcomeConflict || duplicate.Error != "Server name already exists" || duplicate.Audit.Message != "Server name already exists" {
		t.Fatalf("CreateServer(duplicate) = %+v, want conflict name-exists outcome", duplicate)
	}

	invalidUser := commands.UpdateServer("Alpha", Server{Name: "Alpha", Host: "beta.example", User: "root!"})
	if invalidUser.Outcome != CommandOutcomeInvalid || invalidUser.Audit.Meta["user"] != "root!" {
		t.Fatalf("UpdateServer(invalid user) = %+v, want invalid username audit meta", invalidUser)
	}

	(*statusMap)["Alpha"].Status = "updating"
	blocked := commands.UpdateServer("Alpha", Server{Name: "Alpha", Host: "beta.example", User: "root"})
	if blocked.Outcome != CommandOutcomeConflict || blocked.Audit.Meta["status"] != "updating" {
		t.Fatalf("UpdateServer(active) = %+v, want conflict with status", blocked)
	}
	(*statusMap)["Alpha"].Status = "idle"

	updated := commands.UpdateServer("Alpha", Server{Name: "Beta", Host: "beta.example", User: "admin"})
	if !updated.Succeeded() || updated.Server == nil || updated.Server.Name != "Beta" {
		t.Fatalf("UpdateServer() = %+v, want renamed server", updated)
	}
	if updated.Audit.TargetName != "Beta" || updated.Audit.Meta["from"] != "Alpha" || updated.Audit.Meta["tags_count"] != 1 {
		t.Fatalf("UpdateServer() audit = %+v, want renamed audit facts", updated.Audit)
	}

	missing := commands.DeleteServer("Alpha")
	if missing.Outcome != CommandOutcomeNotFound || missing.Error != "Server not found" {
		t.Fatalf("DeleteServer(missing) = %+v, want not found", missing)
	}
	deleted := commands.DeleteServer("Beta")
	if !deleted.Succeeded() || deleted.Message != "Server deleted" || deleted.Audit.Action != "server.delete" {
		t.Fatalf("DeleteServer() = %+v, want delete success", deleted)
	}
}

func TestServerInventoryCommandsCredentialOutcomes(t *testing.T) {
	repo := &fakeRepo{}
	svc, _, stateServers, statusMap := newTestService(repo, []Server{{Name: "srv", Host: "srv.example", User: "root", Pass: "pw", Key: "key"}})
	commands := NewCommandService(svc)

	(*statusMap)["srv"].Status = "updating"
	check := commands.CheckServerKeyUpload("srv")
	if check.Outcome != CommandOutcomeConflict || check.Audit.Meta["status"] != "updating" || check.Error != "wait for the active server action to finish before updating this server key" {
		t.Fatalf("CheckServerKeyUpload(active) = %+v, want conflict with status", check)
	}
	clearPassword := commands.ClearPassword("srv")
	if clearPassword.Outcome != CommandOutcomeConflict || clearPassword.Audit.Meta["status"] != "updating" {
		t.Fatalf("ClearPassword(active) = %+v, want conflict with status", clearPassword)
	}

	(*statusMap)["srv"].Status = "idle"
	clearPassword = commands.ClearPassword("srv")
	if !clearPassword.Succeeded() || clearPassword.Message != "Password cleared" || (*stateServers)[0].Pass != "" {
		t.Fatalf("ClearPassword() = %+v servers=%+v, want password cleared", clearPassword, *stateServers)
	}
	setKey := commands.SetServerKey("srv", "new-key")
	if !setKey.Succeeded() || setKey.Message != "Key uploaded" || repo.updatedName != "srv" || repo.updatedKey != "new-key" {
		t.Fatalf("SetServerKey() = %+v repo=%+v, want key upload", setKey, repo)
	}
	clearKey := commands.ClearServerKey("srv")
	if !clearKey.Succeeded() || clearKey.Message != "Key cleared" || repo.updatedKey != "" {
		t.Fatalf("ClearServerKey() = %+v repo=%+v, want key clear", clearKey, repo)
	}

	missing := commands.SetServerKey("missing", "key")
	if missing.Outcome != CommandOutcomeNotFound || missing.Audit.TargetName != "missing" {
		t.Fatalf("SetServerKey(missing) = %+v, want not found", missing)
	}
}

func TestServerInventoryCommandsHostKeyOutcomes(t *testing.T) {
	tmpDir := t.TempDir()
	knownHostsPath := filepath.Join(tmpDir, "known_hosts")
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	hostKey, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("NewPublicKey() error = %v", err)
	}
	repo := &fakeRepo{}
	svc, _, _, _ := newTestService(repo, nil)
	svc.deps.KnownHosts = KnownHostsDeps{
		DBPath: func() string {
			return filepath.Join(tmpDir, "app.db")
		},
		Getenv: func(key string) string {
			if key == "DEBIAN_UPDATER_KNOWN_HOSTS" {
				return knownHostsPath
			}
			return ""
		},
		UserHomeDir:         func() (string, error) { return tmpDir, nil },
		ScanHostKey:         func(string, int) (ssh.PublicKey, error) { return hostKey, nil },
		KnownHostsMu:        &sync.Mutex{},
		ConstantTimeCompare: func(a, b string) bool { return a == b },
	}
	commands := NewCommandService(svc)

	missingHost := commands.ScanHostKey(" ", 22)
	if missingHost.Outcome != CommandOutcomeInvalid || missingHost.Audit.TargetName != "-" || missingHost.Error != "host is required" {
		t.Fatalf("ScanHostKey(missing host) = %+v, want invalid host", missingHost)
	}

	scanned := commands.ScanHostKey(" example.com ", 2222)
	if !scanned.Succeeded() || scanned.HostKeyScan == nil || scanned.HostKeyScan.Host != "example.com" || scanned.Audit.Meta["algorithm"] == "" {
		t.Fatalf("ScanHostKey() = %+v, want scan payload and audit meta", scanned)
	}
	missingFingerprint := commands.TrustHostKey("example.com", 2222, " ")
	if missingFingerprint.Outcome != CommandOutcomeInvalid || missingFingerprint.Audit.Message != "Fingerprint is required" {
		t.Fatalf("TrustHostKey(missing fingerprint) = %+v, want invalid fingerprint", missingFingerprint)
	}
	mismatch := commands.TrustHostKey("example.com", 2222, "SHA256:not-real")
	if mismatch.Outcome != CommandOutcomeConflict || mismatch.Audit.Message != "Host key fingerprint mismatch" {
		t.Fatalf("TrustHostKey(mismatch) = %+v, want fingerprint conflict", mismatch)
	}
	trusted := commands.TrustHostKey("example.com", 2222, scanned.HostKeyScan.FingerprintSHA256)
	if !trusted.Succeeded() || trusted.HostKeyTrust == nil || trusted.HostKeyTrust.FingerprintSHA256 == "" || trusted.Audit.Meta["already_trusted"] != false {
		t.Fatalf("TrustHostKey() = %+v, want trust payload", trusted)
	}
	cleared := commands.ClearKnownHost("example.com", 2222)
	if !cleared.Succeeded() || cleared.HostKeyClear == nil || cleared.HostKeyClear.RemovedEntries != 1 || cleared.Audit.Meta["removed_entries"] != 1 {
		t.Fatalf("ClearKnownHost() = %+v, want clear payload", cleared)
	}
}
