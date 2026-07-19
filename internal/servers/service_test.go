package servers

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

type fakeRepo struct {
	loaded       []Server
	loadErr      error
	saved        []Server
	saveErr      error
	updateKeyErr error
	updatedName  string
	updatedKey   string
	saveCalls    int
}

func (r *fakeRepo) Load() ([]Server, error) {
	if r.loadErr != nil {
		return nil, r.loadErr
	}
	return CloneServers(r.loaded), nil
}

func TestServerInventoryServiceLoadReturnsRepositoryFailureWithoutReplacingState(t *testing.T) {
	loadErr := errors.New("inventory unavailable")
	repo := &fakeRepo{loadErr: loadErr}
	svc, _, stateServers, _ := newTestService(repo, []Server{{Name: "existing", Host: "existing.example", User: "root"}})

	err := svc.Load()
	if !errors.Is(err, loadErr) {
		t.Fatalf("Load() error = %v, want %v", err, loadErr)
	}
	if len(*stateServers) != 1 || (*stateServers)[0].Name != "existing" {
		t.Fatalf("Load() servers = %+v, want existing inventory preserved", *stateServers)
	}
}

func (r *fakeRepo) Save(servers []Server, txHook TxHook) error {
	r.saveCalls++
	if r.saveErr != nil {
		return r.saveErr
	}
	if txHook != nil {
		if err := txHook(nil); err != nil {
			return err
		}
	}
	r.saved = CloneServers(servers)
	return nil
}

func (r *fakeRepo) UpdateServerKey(name, key string) error {
	if r.updateKeyErr != nil {
		return r.updateKeyErr
	}
	r.updatedName = name
	r.updatedKey = key
	return nil
}

func newTestService(repo *fakeRepo, initial []Server) (*Service, *State, *[]Server, *map[string]*ServerStatus) {
	var mu sync.Mutex
	servers := CloneServers(initial)
	statusMap := make(map[string]*ServerStatus, len(initial))
	for _, server := range initial {
		statusMap[server.Name] = NewIdleStatus(server)
	}
	state := NewState(&mu, &servers, &statusMap, nil)
	return NewService(ServiceDeps{State: state, Repository: repo}), state, &servers, &statusMap
}

func TestServerInventoryServiceCRUDValidationAndRollback(t *testing.T) {
	repo := &fakeRepo{}
	svc, _, stateServers, stateStatus := newTestService(repo, nil)

	created, err := svc.Create(Server{
		Name: " Alpha ",
		Host: " Node.EXAMPLE ",
		User: "root",
		Tags: []string{" prod ", "prod", "db"},
	})
	if err != nil {
		t.Fatalf("Create() unexpected error: %v", err)
	}
	if created.Name != "Alpha" || created.Host != "Node.EXAMPLE" || created.Port != 22 || !reflect.DeepEqual(created.Tags, []string{"prod", "db"}) {
		t.Fatalf("Create() = %+v, want trimmed/defaulted server with deduplicated tags", created)
	}
	if _, err := svc.Create(Server{Name: "alpha", Host: "other.example", User: "root"}); !errors.Is(err, ErrNameExists) {
		t.Fatalf("Create(duplicate name) error = %v, want %v", err, ErrNameExists)
	}
	if _, err := svc.Create(Server{Name: "Beta", Host: "node.example", Port: 2200, User: "root"}); !errors.Is(err, ErrHostExists) {
		t.Fatalf("Create(duplicate host) error = %v, want %v", err, ErrHostExists)
	}
	if _, err := svc.Create(Server{Name: "BadUser", Host: "bad.example", User: "root!"}); !errors.Is(err, ErrInvalidSSHUsername) {
		t.Fatalf("Create(invalid user) error = %v, want %v", err, ErrInvalidSSHUsername)
	}

	repo.saveErr = errors.New("save failed")
	beforeServers := CloneServers(*stateServers)
	beforeStatus := CloneStatusMap(*stateStatus)
	if _, err := svc.Create(Server{Name: "Gamma", Host: "gamma.example", User: "root"}); err == nil {
		t.Fatalf("Create(save failure) error = nil")
	}
	if !reflect.DeepEqual(*stateServers, beforeServers) || !reflect.DeepEqual(*stateStatus, beforeStatus) {
		t.Fatalf("Create(save failure) did not roll back state")
	}
}

func TestServerInventoryServiceUpdateFallbackAndDeleteHooks(t *testing.T) {
	repo := &fakeRepo{}
	var renamedFrom, renamedTo, deleted string
	svc, _, stateServers, _ := newTestService(repo, []Server{{
		Name: "srv-old",
		Host: "old.example",
		Port: 2200,
		User: "root",
		Pass: "pw",
		Key:  "key",
		Tags: []string{"prod"},
	}})
	svc.deps.RenamePolicyOverridesServer = func(_ *sql.Tx, oldName, newName string) error {
		renamedFrom, renamedTo = oldName, newName
		return nil
	}
	svc.deps.RenamePolicyTargetServers = func(_ *sql.Tx, _, _ string) error { return nil }
	svc.deps.RenameServerFacts = func(_ *sql.Tx, _, _ string) error { return nil }
	svc.deps.DeleteServerFacts = func(_ *sql.Tx, name string) error {
		deleted = name
		return nil
	}

	updated, err := svc.Update("srv-old", Server{Name: "srv-new", Host: "new.example", User: "admin"})
	if err != nil {
		t.Fatalf("Update() unexpected error: %v", err)
	}
	if updated.Pass != "pw" || updated.Key != "key" || updated.Port != 2200 || !reflect.DeepEqual(updated.Tags, []string{"prod"}) {
		t.Fatalf("Update() fallback = %+v, want password/key/port/tags preserved", updated)
	}
	if renamedFrom != "srv-old" || renamedTo != "srv-new" {
		t.Fatalf("rename hook = %q -> %q, want srv-old -> srv-new", renamedFrom, renamedTo)
	}
	if err := svc.Delete("srv-new"); err != nil {
		t.Fatalf("Delete() unexpected error: %v", err)
	}
	if deleted != "srv-new" || len(*stateServers) != 0 {
		t.Fatalf("Delete() deleted=%q servers=%+v, want cleanup and empty inventory", deleted, *stateServers)
	}
}

func TestServerStateActionApprovalAndMutationGuards(t *testing.T) {
	repo := &fakeRepo{}
	svc, state, _, statusMap := newTestService(repo, []Server{{Name: "srv", Host: "srv.example", User: "root"}})

	server, err := state.BeginAction("srv", "updating")
	if err != nil {
		t.Fatalf("BeginAction() unexpected error: %v", err)
	}
	if server.Name != "srv" || (*statusMap)["srv"].Status != "updating" {
		t.Fatalf("BeginAction() server/status = %+v/%+v", server, (*statusMap)["srv"])
	}
	if err := svc.ClearPassword("srv"); !errors.Is(err, ErrActionInProgress) {
		t.Fatalf("ClearPassword(active) error = %v, want %v", err, ErrActionInProgress)
	}

	(*statusMap)["srv"].Status = "pending_approval"
	exists, approved := state.ApprovePendingUpdate("srv", "security")
	if !exists || !approved || (*statusMap)["srv"].Status != "approved" || (*statusMap)["srv"].ApprovalScope != "security" {
		t.Fatalf("ApprovePendingUpdate() exists=%t approved=%t status=%+v", exists, approved, (*statusMap)["srv"])
	}
	(*statusMap)["srv"].Status = "pending_approval"
	(*statusMap)["srv"].Logs = "pending"
	(*statusMap)["srv"].PendingUpdates = []PendingUpdate{{Package: "openssl"}}
	exists, cancelled := state.CancelPendingUpdate("srv")
	if !exists || !cancelled || (*statusMap)["srv"].Status != "cancelled" || (*statusMap)["srv"].Logs != "" || len((*statusMap)["srv"].PendingUpdates) != 0 {
		t.Fatalf("CancelPendingUpdate() exists=%t cancelled=%t status=%+v", exists, cancelled, (*statusMap)["srv"])
	}
}

func TestKnownHostsScanTrustClearAndGlobalKeyFallback(t *testing.T) {
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
	deps := KnownHostsDeps{
		DBPath: func() string {
			return filepath.Join(tmpDir, "app.db")
		},
		Getenv: func(key string) string {
			if key == "DEBIAN_UPDATER_KNOWN_HOSTS" {
				return knownHostsPath
			}
			return ""
		},
		UserHomeDir: func() (string, error) { return tmpDir, nil },
		ScanHostKey: func(string, int) (ssh.PublicKey, error) {
			return hostKey, nil
		},
		KnownHostsMu:        &sync.Mutex{},
		ConstantTimeCompare: func(a, b string) bool { return a == b },
	}
	repo := &fakeRepo{}
	svc, _, _, _ := newTestService(repo, nil)
	svc.deps.KnownHosts = deps

	scanned, err := svc.ScanHostKey(" example.com ", 2222)
	if err != nil {
		t.Fatalf("ScanHostKey() unexpected error: %v", err)
	}
	if scanned.Host != "example.com" || scanned.Port != 2222 || scanned.KnownHostsLine == "" || scanned.AlreadyTrusted || scanned.HostEntryExists {
		t.Fatalf("ScanHostKey() = %+v, want trimmed untrusted result", scanned)
	}
	trusted, err := svc.TrustHostKey("example.com", 2222, scanned.FingerprintSHA256)
	if err != nil {
		t.Fatalf("TrustHostKey() unexpected error: %v", err)
	}
	if trusted.AlreadyTrusted || trusted.KnownHostsLine == "" {
		t.Fatalf("TrustHostKey() = %+v, want first trust", trusted)
	}
	trusted, err = svc.TrustHostKey("example.com", 2222, trusted.FingerprintSHA256)
	if err != nil {
		t.Fatalf("TrustHostKey(duplicate) unexpected error: %v", err)
	}
	if !trusted.AlreadyTrusted {
		t.Fatalf("TrustHostKey(duplicate) = %+v, want already trusted", trusted)
	}
	trustedScan, err := svc.ScanHostKey("example.com", 2222)
	if err != nil {
		t.Fatalf("ScanHostKey(trusted) unexpected error: %v", err)
	}
	if !trustedScan.AlreadyTrusted || !trustedScan.HostEntryExists {
		t.Fatalf("ScanHostKey(trusted) = %+v, want matching host entry", trustedScan)
	}
	if err := os.WriteFile(knownHostsPath, []byte("[example.com]:2222 ssh-ed25519 AAAAold\n"), 0600); err != nil {
		t.Fatalf("WriteFile(changed host key) error = %v", err)
	}
	changedScan, err := svc.ScanHostKey("example.com", 2222)
	if err != nil {
		t.Fatalf("ScanHostKey(changed) unexpected error: %v", err)
	}
	if changedScan.AlreadyTrusted || !changedScan.HostEntryExists {
		t.Fatalf("ScanHostKey(changed) = %+v, want mismatched existing host entry", changedScan)
	}
	if _, err := svc.TrustHostKey("example.com", 2222, "SHA256:not-real"); !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("TrustHostKey(mismatch) error = %v, want %v", err, ErrFingerprintMismatch)
	}
	raw, err := os.ReadFile(knownHostsPath)
	if err != nil {
		t.Fatalf("ReadFile(after mismatch) error = %v", err)
	}
	if string(raw) != "[example.com]:2222 ssh-ed25519 AAAAold\n" {
		t.Fatalf("known_hosts after rejected replacement = %q, want original entry", raw)
	}
	replaced, err := svc.TrustHostKey("example.com", 2222, changedScan.FingerprintSHA256)
	if err != nil {
		t.Fatalf("TrustHostKey(replacement) unexpected error: %v", err)
	}
	if replaced.AlreadyTrusted {
		t.Fatalf("TrustHostKey(replacement) = %+v, want replaced entry", replaced)
	}
	raw, err = os.ReadFile(knownHostsPath)
	if err != nil {
		t.Fatalf("ReadFile(after replacement) error = %v", err)
	}
	if strings.Contains(string(raw), "AAAAold") || !strings.Contains(string(raw), replaced.KnownHostsLine) {
		t.Fatalf("known_hosts after replacement = %q, want only new host key", raw)
	}
	cleared, err := svc.ClearKnownHost("example.com", 2222)
	if err != nil {
		t.Fatalf("ClearKnownHost() unexpected error: %v", err)
	}
	if cleared.RemovedEntries != 1 {
		t.Fatalf("ClearKnownHost() removed = %d, want 1", cleared.RemovedEntries)
	}
	raw, err = os.ReadFile(knownHostsPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if len(raw) != 0 {
		t.Fatalf("known_hosts after clear = %q, want empty", raw)
	}

	if _, err := BuildAuthMethods(Server{Key: "not-a-private-key"}); err == nil {
		t.Fatalf("BuildAuthMethods(invalid key) error = nil")
	}
	if _, err := BuildAuthMethods(Server{Key: testPrivateKeyPEM(t)}); err != nil {
		t.Fatalf("BuildAuthMethods(valid key) unexpected error: %v", err)
	}
}

func testPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey() error = %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}
