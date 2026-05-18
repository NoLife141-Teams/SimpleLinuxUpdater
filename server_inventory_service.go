package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

var (
	errServerRequiredFields = errors.New("name, host, and user are required")
	errInvalidSSHUsername   = errors.New("invalid ssh username")
	errServerNameExists     = errors.New("server name already exists")
	errServerHostExists     = errors.New("server host already exists")
	errServerNotFound       = errors.New("server not found")
)

type ServerInventoryService struct {
	db      func() *sql.DB
	encrypt func(string) (string, error)
	decrypt func(string) (string, error)
}

type serverInventoryActionError struct {
	status string
}

func (e serverInventoryActionError) Error() string {
	return errActionInProgress.Error()
}

func (e serverInventoryActionError) Unwrap() error {
	return errActionInProgress
}

type serverHostKeyScanResult struct {
	Host              string
	Port              int
	Algorithm         string
	FingerprintSHA256 string
	KnownHostsLine    string
	AlreadyTrusted    bool
}

type serverHostKeyTrustResult struct {
	Message           string
	Host              string
	Port              int
	FingerprintSHA256 string
	KnownHostsLine    string
	AlreadyTrusted    bool
}

type serverHostKeyClearResult struct {
	Message        string
	Host           string
	Port           int
	RemovedEntries int
}

func newServerInventoryService() *ServerInventoryService {
	return &ServerInventoryService{
		db:      getDB,
		encrypt: encryptSecret,
		decrypt: decryptSecret,
	}
}

var serverInventoryService = newServerInventoryService()

func (s *ServerInventoryService) dbConn() *sql.DB {
	if s != nil && s.db != nil {
		return s.db()
	}
	return getDB()
}

func (s *ServerInventoryService) encryptSecretValue(value string) (string, error) {
	if s != nil && s.encrypt != nil {
		return s.encrypt(value)
	}
	return encryptSecret(value)
}

func (s *ServerInventoryService) decryptSecretValue(value string) (string, error) {
	if s != nil && s.decrypt != nil {
		return s.decrypt(value)
	}
	return decryptSecret(value)
}

func (s *ServerInventoryService) Load() {
	rows, err := s.dbConn().Query("SELECT name, host, port, user, pass_enc, key_enc, tags FROM servers ORDER BY name")
	if err != nil {
		log.Fatalf("Failed to load servers: %v", err)
	}
	defer rows.Close()

	servers = nil
	for rows.Next() {
		var name, host, user, passEnc, keyEnc, tags string
		var port int
		if err := rows.Scan(&name, &host, &port, &user, &passEnc, &keyEnc, &tags); err != nil {
			log.Fatalf("Failed to scan server row: %v", err)
		}
		pass, err := s.decryptSecretValue(passEnc)
		if err != nil {
			log.Fatalf("Failed to decrypt password for %s: %v", name, err)
		}
		key, err := s.decryptSecretValue(keyEnc)
		if err != nil {
			log.Fatalf("Failed to decrypt SSH key for %s: %v", name, err)
		}
		servers = append(servers, Server{
			Name: name,
			Host: host,
			Port: normalizePort(port),
			User: user,
			Pass: pass,
			Key:  key,
			Tags: parseTags(tags),
		})
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("Failed to read servers: %v", err)
	}

	if len(servers) == 0 {
		loadLegacyServers()
	}
}

func (s *ServerInventoryService) SaveWithTxHook(txHook saveServersTxHook) error {
	tx, err := s.dbConn().Begin()
	if err != nil {
		return fmt.Errorf("start db transaction: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM servers"); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("clear servers table: %w", err)
	}
	stmt, err := tx.Prepare("INSERT INTO servers (name, host, port, user, pass_enc, key_enc, tags) VALUES (?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()
	for _, server := range servers {
		enc, err := s.encryptSecretValue(server.Pass)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("encrypt password for %s: %w", server.Name, err)
		}
		keyEnc, err := s.encryptSecretValue(server.Key)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("encrypt SSH key for %s: %w", server.Name, err)
		}
		tags := joinTags(server.Tags)
		port := normalizePort(server.Port)
		if _, err := stmt.Exec(server.Name, server.Host, port, server.User, enc, keyEnc, tags); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert server %s: %w", server.Name, err)
		}
	}
	if txHook != nil {
		if err := txHook(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := pruneUpdatePolicyOverridesForServersTx(tx, servers); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prune policy overrides: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit servers: %w", err)
	}
	return nil
}

func (s *ServerInventoryService) SaveOrRollbackLocked(prevServers []Server, prevStatusMap map[string]*ServerStatus, txHook saveServersTxHook) error {
	save := saveServersFunc
	if txHook != nil {
		save = func() error {
			return s.SaveWithTxHook(txHook)
		}
	}
	if err := save(); err != nil {
		servers = prevServers
		statusMap = prevStatusMap
		return err
	}
	return nil
}

func (s *ServerInventoryService) ListStatuses() []ServerStatus {
	mu.Lock()
	defer mu.Unlock()
	statuses := make([]ServerStatus, 0, len(servers))
	for _, server := range servers {
		status := statusMap[server.Name]
		if status == nil {
			continue
		}
		status.Host = server.Host
		status.Port = normalizePort(server.Port)
		status.User = server.User
		status.HasPassword = server.Pass != ""
		status.HasKey = server.Key != ""
		status.Tags = server.Tags
		copyStatus := *status
		copyStatus.Upgradable = append([]string(nil), status.Upgradable...)
		copyStatus.PendingUpdates = clonePendingUpdates(status.PendingUpdates)
		copyStatus.Tags = append([]string(nil), status.Tags...)
		statuses = append(statuses, copyStatus)
	}
	return statuses
}

func (s *ServerInventoryService) Create(server Server) (Server, error) {
	server.Name = strings.TrimSpace(server.Name)
	server.Host = strings.TrimSpace(server.Host)
	server.User = strings.TrimSpace(server.User)
	if server.Name == "" || server.Host == "" || server.User == "" {
		return server, errServerRequiredFields
	}
	if !isValidSSHUsername(server.User) {
		return server, errInvalidSSHUsername
	}
	server.Port = normalizePort(server.Port)
	server.Tags = parseTags(joinTags(server.Tags))

	mu.Lock()
	defer mu.Unlock()
	prevServers := cloneServers(servers)
	prevStatusMap := cloneStatusMap(statusMap)
	if serverNameExistsLocked(server.Name, -1) {
		return server, errServerNameExists
	}
	if serverHostExistsLocked(server.Host, -1) {
		return server, errServerHostExists
	}
	servers = append(servers, server)
	statusMap[server.Name] = newIdleServerStatus(server)
	if err := s.SaveOrRollbackLocked(prevServers, prevStatusMap, nil); err != nil {
		return server, err
	}
	return server, nil
}

func (s *ServerInventoryService) Update(name string, server Server) (Server, error) {
	server.Name = strings.TrimSpace(server.Name)
	server.Host = strings.TrimSpace(server.Host)
	server.User = strings.TrimSpace(server.User)
	if server.Name == "" || server.Host == "" || server.User == "" {
		return server, errServerRequiredFields
	}
	if !isValidSSHUsername(server.User) {
		return server, errInvalidSSHUsername
	}

	mu.Lock()
	defer mu.Unlock()
	prevServers := cloneServers(servers)
	prevStatusMap := cloneStatusMap(statusMap)
	for i, existing := range servers {
		if existing.Name != name {
			continue
		}
		if status := statusMap[name]; status != nil && statusInProgress(status.Status) {
			return server, serverInventoryActionError{status: status.Status}
		}
		if strings.TrimSpace(server.Pass) == "" {
			server.Pass = existing.Pass
		}
		if strings.TrimSpace(server.Key) == "" {
			server.Key = existing.Key
		}
		if server.Port == 0 {
			server.Port = existing.Port
		}
		server.Port = normalizePort(server.Port)
		if server.Tags == nil {
			server.Tags = existing.Tags
		}
		server.Tags = parseTags(joinTags(server.Tags))
		if serverNameExistsLocked(server.Name, i) {
			return server, errServerNameExists
		}
		if serverHostExistsLocked(server.Host, i) {
			return server, errServerHostExists
		}
		servers[i] = server
		renamedServer := server.Name != name
		if renamedServer {
			delete(statusMap, name)
			statusMap[server.Name] = newIdleServerStatus(server)
		} else {
			updateStatusFromServer(name, server)
		}
		var txHook saveServersTxHook
		if renamedServer {
			oldServerName := name
			newServerName := server.Name
			txHook = func(tx *sql.Tx) error {
				if err := renameUpdatePolicyOverridesServerTx(tx, oldServerName, newServerName); err != nil {
					return err
				}
				if err := renameUpdatePolicyTargetServersTx(tx, oldServerName, newServerName); err != nil {
					return err
				}
				return renameServerFactsTx(tx, oldServerName, newServerName)
			}
		}
		if err := s.SaveOrRollbackLocked(prevServers, prevStatusMap, txHook); err != nil {
			return server, err
		}
		return server, nil
	}
	return server, errServerNotFound
}

func (s *ServerInventoryService) Delete(name string) error {
	mu.Lock()
	defer mu.Unlock()
	prevServers := cloneServers(servers)
	prevStatusMap := cloneStatusMap(statusMap)
	for i, server := range servers {
		if server.Name != name {
			continue
		}
		if status := statusMap[name]; status != nil && statusInProgress(status.Status) {
			return serverInventoryActionError{status: status.Status}
		}
		servers = append(servers[:i], servers[i+1:]...)
		delete(statusMap, name)
		txHook := func(tx *sql.Tx) error {
			_, err := tx.Exec("DELETE FROM server_facts WHERE server_name = ?", name)
			return err
		}
		if err := s.SaveOrRollbackLocked(prevServers, prevStatusMap, txHook); err != nil {
			return err
		}
		return nil
	}
	return errServerNotFound
}

func (s *ServerInventoryService) ClearPassword(name string) error {
	mu.Lock()
	defer mu.Unlock()
	prevServers := cloneServers(servers)
	prevStatusMap := cloneStatusMap(statusMap)
	for i, server := range servers {
		if server.Name != name {
			continue
		}
		if blocked, status := serverActionStatusInProgressLocked(name); blocked {
			return serverInventoryActionError{status: status}
		}
		servers[i].Pass = ""
		if err := s.SaveOrRollbackLocked(prevServers, prevStatusMap, nil); err != nil {
			return err
		}
		if status, ok := statusMap[name]; ok {
			status.HasPassword = false
		}
		return nil
	}
	return errServerNotFound
}

func (s *ServerInventoryService) CheckMutationAllowed(name string) error {
	mu.Lock()
	defer mu.Unlock()
	if _, found := findServerByNameLocked(name); !found {
		return errServerNotFound
	}
	if blocked, status := serverActionStatusInProgressLocked(name); blocked {
		return serverInventoryActionError{status: status}
	}
	return nil
}

func (s *ServerInventoryService) SetKey(name, key string) error {
	mu.Lock()
	defer mu.Unlock()
	for i, server := range servers {
		if server.Name != name {
			continue
		}
		if blocked, status := serverActionStatusInProgressLocked(name); blocked {
			return serverInventoryActionError{status: status}
		}
		if err := s.updateServerKey(name, key); err != nil {
			return err
		}
		servers[i].Key = key
		if status, ok := statusMap[name]; ok {
			status.HasKey = key != ""
		}
		return nil
	}
	return errServerNotFound
}

func (s *ServerInventoryService) ClearKey(name string) error {
	return s.SetKey(name, "")
}

func (s *ServerInventoryService) updateServerKey(name, key string) error {
	enc, err := s.encryptSecretValue(key)
	if err != nil {
		return err
	}
	_, err = s.dbConn().Exec("UPDATE servers SET key_enc = ? WHERE name = ?", enc, name)
	return err
}

func (s *ServerInventoryService) ScanHostKey(host string, port int) (serverHostKeyScanResult, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return serverHostKeyScanResult{}, errors.New("host is required")
	}
	port = normalizePort(port)
	key, err := scanHostKeyFunc(host, port)
	if err != nil {
		return serverHostKeyScanResult{}, err
	}
	line := buildKnownHostsLine(host, port, key)
	alreadyTrusted := false
	if trusted, trustedErr := knownHostLineExists(line); trustedErr == nil {
		alreadyTrusted = trusted
	} else {
		log.Printf("hostkey.scan trusted-check failed for %s:%d: %v", host, port, trustedErr)
	}
	return serverHostKeyScanResult{
		Host:              host,
		Port:              port,
		Algorithm:         key.Type(),
		FingerprintSHA256: ssh.FingerprintSHA256(key),
		KnownHostsLine:    line,
		AlreadyTrusted:    alreadyTrusted,
	}, nil
}

func (s *ServerInventoryService) TrustHostKey(host string, port int, expectedFingerprint string) (serverHostKeyTrustResult, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return serverHostKeyTrustResult{}, errors.New("host is required")
	}
	port = normalizePort(port)
	fingerprint, line, alreadyTrusted, err := trustHostKey(host, port, strings.TrimSpace(expectedFingerprint))
	if err != nil {
		return serverHostKeyTrustResult{}, err
	}
	message := "Host key trusted"
	if alreadyTrusted {
		message = "Host key already trusted"
	}
	return serverHostKeyTrustResult{
		Message:           message,
		Host:              host,
		Port:              port,
		FingerprintSHA256: fingerprint,
		KnownHostsLine:    line,
		AlreadyTrusted:    alreadyTrusted,
	}, nil
}

func (s *ServerInventoryService) ClearKnownHost(host string, port int) (serverHostKeyClearResult, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return serverHostKeyClearResult{}, errors.New("host is required")
	}
	port = normalizePort(port)
	removed, err := removeKnownHostEntries(host, port)
	if err != nil {
		return serverHostKeyClearResult{}, err
	}
	message := "Known host entry not found"
	if removed > 0 {
		message = "Known host entry cleared"
	}
	return serverHostKeyClearResult{
		Message:        message,
		Host:           host,
		Port:           port,
		RemovedEntries: removed,
	}, nil
}

func newIdleServerStatus(server Server) *ServerStatus {
	return &ServerStatus{
		Name:           server.Name,
		Host:           server.Host,
		Port:           normalizePort(server.Port),
		User:           server.User,
		Status:         "idle",
		Logs:           "",
		Upgradable:     []string{},
		PendingUpdates: []PendingUpdate{},
		HasPassword:    server.Pass != "",
		HasKey:         server.Key != "",
		Tags:           server.Tags,
	}
}

func updateStatusFromServer(name string, server Server) {
	if statusMap[name] == nil {
		statusMap[name] = &ServerStatus{
			Name:           server.Name,
			Status:         "idle",
			Upgradable:     []string{},
			PendingUpdates: []PendingUpdate{},
		}
	}
	statusMap[name].Host = server.Host
	statusMap[name].Port = normalizePort(server.Port)
	statusMap[name].User = server.User
	statusMap[name].HasPassword = server.Pass != ""
	statusMap[name].HasKey = server.Key != ""
	statusMap[name].Tags = server.Tags
}

func serverInventoryActionStatus(err error) string {
	var actionErr serverInventoryActionError
	if errors.As(err, &actionErr) {
		return actionErr.status
	}
	return ""
}

func parseTags(raw string) []string {
	parts := strings.Split(raw, ",")
	var tags []string
	for _, part := range parts {
		tag := strings.TrimSpace(part)
		if tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

func joinTags(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	cleaned := make([]string, 0, len(tags))
	seen := make(map[string]struct{})
	for _, tag := range tags {
		clean := strings.TrimSpace(tag)
		if clean == "" {
			continue
		}
		if _, exists := seen[clean]; exists {
			continue
		}
		seen[clean] = struct{}{}
		cleaned = append(cleaned, clean)
	}
	return strings.Join(cleaned, ", ")
}

func normalizePort(port int) int {
	if port <= 0 || port > 65535 {
		return 22
	}
	return port
}

func normalizeServerName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeServerHost(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func serverNameExistsLocked(name string, skipIndex int) bool {
	normalized := normalizeServerName(name)
	for i, existing := range servers {
		if i == skipIndex {
			continue
		}
		if normalizeServerName(existing.Name) == normalized {
			return true
		}
	}
	return false
}

func serverHostExistsLocked(host string, skipIndex int) bool {
	normalized := normalizeServerHost(host)
	for i, existing := range servers {
		if i == skipIndex {
			continue
		}
		if normalizeServerHost(existing.Host) == normalized {
			return true
		}
	}
	return false
}

func knownHostsPaths() []string {
	if raw := strings.TrimSpace(os.Getenv("DEBIAN_UPDATER_KNOWN_HOSTS")); raw != "" {
		parts := strings.Split(raw, ":")
		paths := make([]string, 0, len(parts))
		for _, part := range parts {
			path := strings.TrimSpace(part)
			if path != "" {
				paths = append(paths, path)
			}
		}
		return paths
	}
	paths := []string{filepath.Join(filepath.Dir(dbPath()), "known_hosts")}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		paths = append(paths, filepath.Join(home, ".ssh", "known_hosts"))
	}
	paths = append(paths, "/etc/ssh/ssh_known_hosts")
	seen := make(map[string]struct{}, len(paths))
	unique := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		unique = append(unique, path)
	}
	return unique
}

func knownHostsDefaultWritePath() string {
	return filepath.Join(filepath.Dir(dbPath()), "known_hosts")
}

func getHostKeyCallback() (ssh.HostKeyCallback, error) {
	candidates := knownHostsPaths()
	existing := make([]string, 0, len(candidates))
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			existing = append(existing, path)
		}
	}
	if len(existing) == 0 {
		return nil, errors.New("no known_hosts file found; set DEBIAN_UPDATER_KNOWN_HOSTS or create ~/.ssh/known_hosts")
	}
	cb, err := knownhosts.New(existing...)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts: %w", err)
	}
	return cb, nil
}
