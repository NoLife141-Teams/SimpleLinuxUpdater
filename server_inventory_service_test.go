package main

import (
	"bytes"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func prepareServerInventoryServiceTest(t *testing.T, dbName string) *ServerInventoryService {
	t.Helper()
	preserveDBState(t)
	preserveServerState(t)
	preserveEncryptionState(t)
	t.Setenv("DEBIAN_UPDATER_DB_PATH", filepath.Join(t.TempDir(), dbName))
	_ = getDB()
	return serverInventoryService
}

func preserveGlobalKeyState(t *testing.T) {
	t.Helper()
	globalKeyMu.RLock()
	origGlobalKey := globalKey
	globalKeyMu.RUnlock()
	t.Cleanup(func() {
		globalKeyMu.Lock()
		globalKey = origGlobalKey
		globalKeyMu.Unlock()
	})
}

func TestServerInventoryServiceCreateAndUpdateValidation(t *testing.T) {
	svc := prepareServerInventoryServiceTest(t, "server-inventory-create.db")

	created, err := svc.Create(Server{
		Name: " Alpha ",
		Host: "Node.EXAMPLE",
		Port: 0,
		User: "root",
		Pass: "pw",
		Key:  "private-key",
		Tags: []string{" prod ", "prod", "web"},
	})
	if err != nil {
		t.Fatalf("Create() unexpected error: %v", err)
	}
	if created.Name != "Alpha" || created.Port != 22 || !reflect.DeepEqual(created.Tags, []string{"prod", "web"}) {
		t.Fatalf("created server = %+v, want trimmed name, default port, deduped tags", created)
	}

	if _, err := svc.Create(Server{Name: "alpha", Host: "other.example", User: "root"}); !errors.Is(err, errServerNameExists) {
		t.Fatalf("Create(duplicate name) error = %v, want %v", err, errServerNameExists)
	}
	if _, err := svc.Create(Server{Name: "Beta", Host: "node.example", Port: 2222, User: "root"}); !errors.Is(err, errServerHostExists) {
		t.Fatalf("Create(duplicate host different port) error = %v, want %v", err, errServerHostExists)
	}
	if _, err := svc.Create(Server{Name: "bad-user", Host: "bad.example", User: "root!"}); !errors.Is(err, errInvalidSSHUsername) {
		t.Fatalf("Create(invalid user) error = %v, want %v", err, errInvalidSSHUsername)
	}

	updated, err := svc.Update("Alpha", Server{Name: "Alpha", Host: "node2.example", User: "admin"})
	if err != nil {
		t.Fatalf("Update() unexpected error: %v", err)
	}
	if updated.Pass != "pw" || updated.Key != "private-key" || updated.Port != 22 || !reflect.DeepEqual(updated.Tags, []string{"prod", "web"}) {
		t.Fatalf("updated fallback fields = %+v, want original pass/key/port/tags", updated)
	}
	mu.Lock()
	status := statusMap["Alpha"]
	mu.Unlock()
	if status == nil || status.Host != "node2.example" || status.User != "admin" || !status.HasPassword || !status.HasKey {
		t.Fatalf("status after update = %+v, want synced inventory fields", status)
	}
}

func TestServerInventoryServiceRenameMigratesPolicyAndFacts(t *testing.T) {
	svc := prepareServerInventoryServiceTest(t, "server-inventory-rename.db")

	if _, err := svc.Create(Server{Name: "srv-old", Host: "old.example", Port: 22, User: "root", Pass: "pw"}); err != nil {
		t.Fatalf("Create() unexpected error: %v", err)
	}
	policy, err := createUpdatePolicy(UpdatePolicy{
		Name:          "Explicit target",
		Enabled:       true,
		TargetServers: []string{"srv-old"},
		PackageScope:  updatePolicyPackageScopeSecurity,
		ExecutionMode: updatePolicyExecutionScanOnly,
		CadenceKind:   updatePolicyCadenceDaily,
		TimeLocal:     "03:00",
	})
	if err != nil {
		t.Fatalf("createUpdatePolicy() unexpected error: %v", err)
	}
	if _, err := setUpdatePolicyOverride(policy.ID, "srv-old", true); err != nil {
		t.Fatalf("setUpdatePolicyOverride() unexpected error: %v", err)
	}
	if err := saveServerFacts(serverFactsRecord{ServerName: "srv-old", OSPrettyName: "Debian", RawJSON: `{"ok":true}`}); err != nil {
		t.Fatalf("saveServerFacts() unexpected error: %v", err)
	}

	if _, err := svc.Update("srv-old", Server{Name: "srv-new", Host: "old.example", Port: 22, User: "root"}); err != nil {
		t.Fatalf("Update(rename) unexpected error: %v", err)
	}
	overrides, err := listUpdatePolicyOverrides(policy.ID)
	if err != nil {
		t.Fatalf("listUpdatePolicyOverrides() unexpected error: %v", err)
	}
	if len(overrides) != 1 || overrides[0].ServerName != "srv-new" || !overrides[0].Disabled {
		t.Fatalf("overrides after rename = %+v, want disabled srv-new", overrides)
	}
	renamedPolicy, err := getUpdatePolicy(policy.ID)
	if err != nil {
		t.Fatalf("getUpdatePolicy() unexpected error: %v", err)
	}
	if !reflect.DeepEqual(renamedPolicy.TargetServers, []string{"srv-new"}) {
		t.Fatalf("target servers after rename = %+v, want [srv-new]", renamedPolicy.TargetServers)
	}
	facts, err := loadServerFacts()
	if err != nil {
		t.Fatalf("loadServerFacts() unexpected error: %v", err)
	}
	if _, ok := facts["srv-new"]; !ok {
		t.Fatalf("facts after rename missing srv-new: %+v", facts)
	}
	if _, ok := facts["srv-old"]; ok {
		t.Fatalf("facts after rename still contain srv-old: %+v", facts)
	}

	if err := svc.Delete("srv-new"); err != nil {
		t.Fatalf("Delete() unexpected error: %v", err)
	}
	facts, err = loadServerFacts()
	if err != nil {
		t.Fatalf("loadServerFacts(after delete) unexpected error: %v", err)
	}
	if _, ok := facts["srv-new"]; ok {
		t.Fatalf("facts after delete still contain srv-new: %+v", facts)
	}
	overrides, err = listUpdatePolicyOverrides(policy.ID)
	if err != nil {
		t.Fatalf("listUpdatePolicyOverrides(after delete) unexpected error: %v", err)
	}
	if len(overrides) != 0 {
		t.Fatalf("overrides after delete = %+v, want empty", overrides)
	}
}

func TestServerInventoryServiceRollbackAndActionGuards(t *testing.T) {
	svc := prepareServerInventoryServiceTest(t, "server-inventory-rollback.db")

	mu.Lock()
	servers = []Server{{Name: "srv-a", Host: "a.example", Port: 22, User: "root", Pass: "pw", Key: "key"}}
	statusMap = map[string]*ServerStatus{
		"srv-a": {Name: "srv-a", Status: "updating", HasPassword: true, HasKey: true},
	}
	wantServers := cloneServers(servers)
	wantStatusMap := cloneStatusMap(statusMap)
	saveServersFunc = func() error {
		return errors.New("forced save failure")
	}
	mu.Unlock()

	if _, err := svc.Create(Server{Name: "srv-b", Host: "b.example", User: "root"}); err == nil {
		t.Fatalf("Create() error = nil, want forced save failure")
	}
	mu.Lock()
	if !reflect.DeepEqual(servers, wantServers) || !reflect.DeepEqual(statusMap, wantStatusMap) {
		t.Fatalf("state after rollback servers=%+v status=%+v, want original", servers, statusMap)
	}
	saveServersFunc = saveServers
	mu.Unlock()

	if err := svc.ClearPassword("srv-a"); !errors.Is(err, errActionInProgress) {
		t.Fatalf("ClearPassword(active) error = %v, want %v", err, errActionInProgress)
	}
	if err := svc.SetKey("srv-a", "new-key"); !errors.Is(err, errActionInProgress) {
		t.Fatalf("SetKey(active) error = %v, want %v", err, errActionInProgress)
	}
	if err := svc.ClearKey("srv-a"); !errors.Is(err, errActionInProgress) {
		t.Fatalf("ClearKey(active) error = %v, want %v", err, errActionInProgress)
	}
	mu.Lock()
	defer mu.Unlock()
	if servers[0].Pass != "pw" || servers[0].Key != "key" {
		t.Fatalf("active action mutated credentials: %+v", servers[0])
	}
}

func TestServerInventoryServiceKnownHostsLifecycle(t *testing.T) {
	svc := prepareServerInventoryServiceTest(t, "server-inventory-known-hosts.db")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	t.Setenv("DEBIAN_UPDATER_KNOWN_HOSTS", knownHosts)

	_, privateKey, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatalf("NewSignerFromKey() error = %v", err)
	}
	origScanner := scanHostKeyFunc
	scanHostKeyFunc = func(_ string, _ int) (ssh.PublicKey, error) {
		return signer.PublicKey(), nil
	}
	t.Cleanup(func() {
		scanHostKeyFunc = origScanner
	})

	scanned, err := svc.ScanHostKey(" example.com ", 2222)
	if err != nil {
		t.Fatalf("ScanHostKey() unexpected error: %v", err)
	}
	if scanned.Host != "example.com" || scanned.Port != 2222 || scanned.AlreadyTrusted {
		t.Fatalf("ScanHostKey() = %+v, want trimmed host, port 2222, not trusted", scanned)
	}
	trusted, err := svc.TrustHostKey("example.com", 2222, scanned.FingerprintSHA256)
	if err != nil {
		t.Fatalf("TrustHostKey() unexpected error: %v", err)
	}
	if trusted.Message != "Host key trusted" || trusted.AlreadyTrusted {
		t.Fatalf("TrustHostKey() = %+v, want first trust", trusted)
	}
	trusted, err = svc.TrustHostKey("example.com", 2222, scanned.FingerprintSHA256)
	if err != nil {
		t.Fatalf("TrustHostKey(duplicate) unexpected error: %v", err)
	}
	if trusted.Message != "Host key already trusted" || !trusted.AlreadyTrusted {
		t.Fatalf("TrustHostKey(duplicate) = %+v, want already trusted", trusted)
	}
	if _, err := svc.TrustHostKey("example.com", 2222, "SHA256:not-the-real-fingerprint"); !errors.Is(err, errFingerprintMismatch) {
		t.Fatalf("TrustHostKey(mismatch) error = %v, want %v", err, errFingerprintMismatch)
	}
	cleared, err := svc.ClearKnownHost("example.com", 2222)
	if err != nil {
		t.Fatalf("ClearKnownHost() unexpected error: %v", err)
	}
	if cleared.Message != "Known host entry cleared" || cleared.RemovedEntries != 1 {
		t.Fatalf("ClearKnownHost() = %+v, want one removed entry", cleared)
	}
	raw, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatalf("ReadFile(known_hosts) unexpected error: %v", err)
	}
	if strings.Contains(string(raw), "[example.com]:2222") {
		t.Fatalf("known_hosts still contains cleared host: %q", string(raw))
	}
}

func TestHostKeyRoutesUseInventoryServiceWireShape(t *testing.T) {
	preserveDBState(t)
	preserveServerState(t)
	preserveSessionState(t)
	preserveRateLimiterState(t)
	preserveMetricsTokenState(t)
	preserveEncryptionState(t)
	dbFile := filepath.Join(t.TempDir(), "hostkey-routes.db")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	t.Setenv("DEBIAN_UPDATER_KNOWN_HOSTS", knownHosts)
	handler, sessionCookie := setupAuthenticatedHandler(t, dbFile)

	_, privateKey, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatalf("NewSignerFromKey() error = %v", err)
	}
	origScanner := scanHostKeyFunc
	scanHostKeyFunc = func(_ string, _ int) (ssh.PublicKey, error) {
		return signer.PublicKey(), nil
	}
	t.Cleanup(func() {
		scanHostKeyFunc = origScanner
	})

	scanRec := httptest.NewRecorder()
	scanReq := httptest.NewRequest(http.MethodPost, "/api/hostkeys/scan", bytes.NewBufferString(`{"host":"example.com","port":2222}`))
	scanReq.AddCookie(sessionCookie)
	scanReq.Header.Set("Content-Type", "application/json")
	markSameOriginAuthRequest(scanReq)
	handler.ServeHTTP(scanRec, scanReq)
	if scanRec.Code != http.StatusOK {
		t.Fatalf("scan status = %d, want %d (body=%s)", scanRec.Code, http.StatusOK, scanRec.Body.String())
	}
	var scanResp struct {
		Host              string `json:"host"`
		Port              int    `json:"port"`
		Algorithm         string `json:"algorithm"`
		FingerprintSHA256 string `json:"fingerprint_sha256"`
		KnownHostsLine    string `json:"known_hosts_line"`
		AlreadyTrusted    bool   `json:"already_trusted"`
	}
	if err := json.Unmarshal(scanRec.Body.Bytes(), &scanResp); err != nil {
		t.Fatalf("unmarshal scan response: %v", err)
	}
	if scanResp.Host != "example.com" || scanResp.Port != 2222 || scanResp.Algorithm == "" || scanResp.FingerprintSHA256 == "" || scanResp.KnownHostsLine == "" || scanResp.AlreadyTrusted {
		t.Fatalf("scan response = %+v, want full untrusted hostkey payload", scanResp)
	}

	trustBody := `{"host":"example.com","port":2222,"fingerprint_sha256":"` + scanResp.FingerprintSHA256 + `"}`
	trustRec := httptest.NewRecorder()
	trustReq := httptest.NewRequest(http.MethodPost, "/api/hostkeys/trust", bytes.NewBufferString(trustBody))
	trustReq.AddCookie(sessionCookie)
	trustReq.Header.Set("Content-Type", "application/json")
	markSameOriginAuthRequest(trustReq)
	handler.ServeHTTP(trustRec, trustReq)
	if trustRec.Code != http.StatusOK {
		t.Fatalf("trust status = %d, want %d (body=%s)", trustRec.Code, http.StatusOK, trustRec.Body.String())
	}

	clearRec := httptest.NewRecorder()
	clearReq := httptest.NewRequest(http.MethodPost, "/api/hostkeys/clear", bytes.NewBufferString(`{"host":"example.com","port":2222}`))
	clearReq.AddCookie(sessionCookie)
	clearReq.Header.Set("Content-Type", "application/json")
	markSameOriginAuthRequest(clearReq)
	handler.ServeHTTP(clearRec, clearReq)
	if clearRec.Code != http.StatusOK {
		t.Fatalf("clear status = %d, want %d (body=%s)", clearRec.Code, http.StatusOK, clearRec.Body.String())
	}
	var clearResp struct {
		Message        string `json:"message"`
		Host           string `json:"host"`
		Port           int    `json:"port"`
		RemovedEntries int    `json:"removed_entries"`
	}
	if err := json.Unmarshal(clearRec.Body.Bytes(), &clearResp); err != nil {
		t.Fatalf("unmarshal clear response: %v", err)
	}
	if clearResp.Message != "Known host entry cleared" || clearResp.Host != "example.com" || clearResp.Port != 2222 || clearResp.RemovedEntries != 1 {
		t.Fatalf("clear response = %+v, want cleared example.com:2222", clearResp)
	}
}

func TestServerInventoryServiceAuthKeyFallback(t *testing.T) {
	prepareServerInventoryServiceTest(t, "server-inventory-auth-key.db")
	preserveGlobalKeyState(t)

	serverKey := testPrivateKeyPEM(t)
	globalKeyValue := testPrivateKeyPEM(t)
	if err := setGlobalKey(globalKeyValue); err != nil {
		t.Fatalf("setGlobalKey(valid) unexpected error: %v", err)
	}
	if _, err := buildAuthMethods(Server{}); err != nil {
		t.Fatalf("buildAuthMethods(empty server key) unexpected error with global fallback: %v", err)
	}
	if _, err := buildAuthMethods(Server{Key: "not-a-private-key"}); err == nil {
		t.Fatalf("buildAuthMethods(invalid server key) error = nil, want server key parse failure before global fallback")
	}
	if err := setGlobalKey("not-a-private-key"); err != nil {
		t.Fatalf("setGlobalKey(invalid) unexpected error: %v", err)
	}
	if _, err := buildAuthMethods(Server{Key: serverKey}); err != nil {
		t.Fatalf("buildAuthMethods(valid server key) unexpected error with invalid global key: %v", err)
	}
}

func testPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey() error = %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}
