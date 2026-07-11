package servers

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"golang.org/x/crypto/ssh"
)

type TxHook func(*sql.Tx) error

type Repository interface {
	Load() ([]Server, error)
	Save([]Server, TxHook) error
	UpdateServerKey(name, key string) error
}

type SQLiteRepository struct {
	DB      func() *sql.DB
	Encrypt func(string) (string, error)
	Decrypt func(string) (string, error)
}

func (r SQLiteRepository) dbConn() *sql.DB {
	if r.DB != nil {
		return r.DB()
	}
	return nil
}

func (r SQLiteRepository) encrypt(value string) (string, error) {
	if r.Encrypt == nil {
		return value, nil
	}
	return r.Encrypt(value)
}

func (r SQLiteRepository) decrypt(value string) (string, error) {
	if r.Decrypt == nil {
		return value, nil
	}
	return r.Decrypt(value)
}

func (r SQLiteRepository) Load() ([]Server, error) {
	db := r.dbConn()
	if db == nil {
		return nil, errors.New("database is not initialized")
	}
	rows, err := db.Query("SELECT name, host, port, user, pass_enc, key_enc, tags FROM servers ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	loaded := []Server{}
	for rows.Next() {
		var name, host, user, passEnc, keyEnc, tags string
		var port int
		if err := rows.Scan(&name, &host, &port, &user, &passEnc, &keyEnc, &tags); err != nil {
			return nil, err
		}
		pass, err := r.decrypt(passEnc)
		if err != nil {
			return nil, fmt.Errorf("decrypt password for %s: %w", name, err)
		}
		key, err := r.decrypt(keyEnc)
		if err != nil {
			return nil, fmt.Errorf("decrypt SSH key for %s: %w", name, err)
		}
		loaded = append(loaded, Server{
			Name: name,
			Host: host,
			Port: NormalizePort(port),
			User: user,
			Pass: pass,
			Key:  key,
			Tags: ParseTags(tags),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return loaded, nil
}

func (r SQLiteRepository) Save(servers []Server, txHook TxHook) error {
	db := r.dbConn()
	if db == nil {
		return errors.New("database is not initialized")
	}
	tx, err := db.Begin()
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
		enc, err := r.encrypt(server.Pass)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("encrypt password for %s: %w", server.Name, err)
		}
		keyEnc, err := r.encrypt(server.Key)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("encrypt SSH key for %s: %w", server.Name, err)
		}
		tags := JoinTags(server.Tags)
		port := NormalizePort(server.Port)
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit servers: %w", err)
	}
	return nil
}

func (r SQLiteRepository) UpdateServerKey(name, key string) error {
	db := r.dbConn()
	if db == nil {
		return errors.New("database is not initialized")
	}
	enc, err := r.encrypt(key)
	if err != nil {
		return err
	}
	_, err = db.Exec("UPDATE servers SET key_enc = ? WHERE name = ?", enc, name)
	return err
}

type ServiceDeps struct {
	State                          *State
	Repository                     Repository
	KnownHosts                     KnownHostsDeps
	LegacyImport                   func() bool
	SaveOverride                   func() error
	PrunePolicyOverridesForServers func(*sql.Tx, []Server) error
	RenamePolicyOverridesServer    func(*sql.Tx, string, string) error
	RenamePolicyTargetServers      func(*sql.Tx, string, string) error
	RenameServerFacts              func(*sql.Tx, string, string) error
	DeleteServerFacts              func(*sql.Tx, string) error
}

type Service struct {
	deps ServiceDeps
}

func NewService(deps ServiceDeps) *Service {
	return &Service{deps: deps}
}

func (s *Service) SetSaveOverride(save func() error) {
	s.deps.SaveOverride = save
}

func (s *Service) SetLegacyImport(importLegacy func() bool) {
	s.deps.LegacyImport = importLegacy
}

func (s *Service) state() *State {
	return s.deps.State
}

func (s *Service) repo() Repository {
	return s.deps.Repository
}

func (s *Service) Load() error {
	loaded, err := s.repo().Load()
	if err != nil {
		return fmt.Errorf("load Server inventory: %w", err)
	}
	s.state().SetServers(loaded)
	if len(loaded) == 0 && s.deps.LegacyImport != nil {
		s.deps.LegacyImport()
	}
	return nil
}

func (s *Service) SaveWithTxHook(txHook TxHook) error {
	servers := s.state().Servers()
	combinedHook := txHook
	if s.deps.PrunePolicyOverridesForServers != nil {
		combinedHook = func(tx *sql.Tx) error {
			if txHook != nil {
				if err := txHook(tx); err != nil {
					return err
				}
			}
			if err := s.deps.PrunePolicyOverridesForServers(tx, servers); err != nil {
				return fmt.Errorf("prune policy overrides: %w", err)
			}
			return nil
		}
	}
	return s.repo().Save(servers, combinedHook)
}

func (s *Service) SaveOrRollbackLocked(prevServers []Server, prevStatusMap map[string]*ServerStatus, txHook TxHook) error {
	save := func() error {
		return s.SaveWithTxHook(txHook)
	}
	if txHook == nil && s.deps.SaveOverride != nil {
		save = s.deps.SaveOverride
	}
	if err := save(); err != nil {
		s.state().SetServers(prevServers)
		s.state().SetStatusMap(prevStatusMap)
		return err
	}
	return nil
}

func (s *Service) ListStatuses() []ServerStatus {
	return s.state().ListStatuses()
}

func (s *Service) Create(server Server) (Server, error) {
	server.Name = strings.TrimSpace(server.Name)
	server.Host = strings.TrimSpace(server.Host)
	server.User = strings.TrimSpace(server.User)
	if server.Name == "" || server.Host == "" || server.User == "" {
		return server, ErrRequiredFields
	}
	if !IsValidSSHUsername(server.User) {
		return server, ErrInvalidSSHUsername
	}
	server.Port = NormalizePort(server.Port)
	server.Tags = ParseTags(JoinTags(server.Tags))
	state := s.state()
	state.Lock()
	defer state.Unlock()
	prevServers := state.CloneServers()
	prevStatusMap := state.CloneStatusMap()
	if ServerNameExists(state.Servers(), server.Name, -1) {
		return server, ErrNameExists
	}
	if ServerHostExists(state.Servers(), server.Host, -1) {
		return server, ErrHostExists
	}
	state.SetServers(append(state.Servers(), server))
	state.StatusMap()[server.Name] = NewIdleStatus(server)
	if err := s.SaveOrRollbackLocked(prevServers, prevStatusMap, nil); err != nil {
		return server, err
	}
	return server, nil
}

func (s *Service) Update(name string, server Server) (Server, error) {
	server.Name = strings.TrimSpace(server.Name)
	server.Host = strings.TrimSpace(server.Host)
	server.User = strings.TrimSpace(server.User)
	if server.Name == "" || server.Host == "" || server.User == "" {
		return server, ErrRequiredFields
	}
	if !IsValidSSHUsername(server.User) {
		return server, ErrInvalidSSHUsername
	}
	state := s.state()
	state.Lock()
	defer state.Unlock()
	prevServers := state.CloneServers()
	prevStatusMap := state.CloneStatusMap()
	currentServers := state.Servers()
	for i, existing := range currentServers {
		if existing.Name != name {
			continue
		}
		if status := state.StatusMap()[name]; status != nil && state.statusInProgress(status.Status) {
			return server, ActionError{Status: status.Status}
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
		server.Port = NormalizePort(server.Port)
		if server.Tags == nil {
			server.Tags = existing.Tags
		}
		server.Tags = ParseTags(JoinTags(server.Tags))
		if ServerNameExists(currentServers, server.Name, i) {
			return server, ErrNameExists
		}
		if ServerHostExists(currentServers, server.Host, i) {
			return server, ErrHostExists
		}
		currentServers[i] = server
		state.SetServers(currentServers)
		renamedServer := server.Name != name
		if renamedServer {
			delete(state.StatusMap(), name)
			state.StatusMap()[server.Name] = NewIdleStatus(server)
		} else {
			UpdateStatusFromServer(state.StatusMap(), name, server)
		}
		var txHook TxHook
		if renamedServer {
			oldServerName := name
			newServerName := server.Name
			txHook = func(tx *sql.Tx) error {
				if s.deps.RenamePolicyOverridesServer != nil {
					if err := s.deps.RenamePolicyOverridesServer(tx, oldServerName, newServerName); err != nil {
						return err
					}
				}
				if s.deps.RenamePolicyTargetServers != nil {
					if err := s.deps.RenamePolicyTargetServers(tx, oldServerName, newServerName); err != nil {
						return err
					}
				}
				if s.deps.RenameServerFacts != nil {
					return s.deps.RenameServerFacts(tx, oldServerName, newServerName)
				}
				return nil
			}
		}
		if err := s.SaveOrRollbackLocked(prevServers, prevStatusMap, txHook); err != nil {
			return server, err
		}
		return server, nil
	}
	return server, ErrNotFound
}

func (s *Service) Delete(name string) error {
	state := s.state()
	state.Lock()
	defer state.Unlock()
	prevServers := state.CloneServers()
	prevStatusMap := state.CloneStatusMap()
	currentServers := state.Servers()
	for i, server := range currentServers {
		if server.Name != name {
			continue
		}
		if status := state.StatusMap()[name]; status != nil && state.statusInProgress(status.Status) {
			return ActionError{Status: status.Status}
		}
		_ = server
		state.SetServers(append(currentServers[:i], currentServers[i+1:]...))
		delete(state.StatusMap(), name)
		txHook := func(tx *sql.Tx) error {
			if s.deps.DeleteServerFacts == nil {
				return nil
			}
			return s.deps.DeleteServerFacts(tx, name)
		}
		if err := s.SaveOrRollbackLocked(prevServers, prevStatusMap, txHook); err != nil {
			return err
		}
		return nil
	}
	return ErrNotFound
}

func (s *Service) ClearPassword(name string) error {
	state := s.state()
	state.Lock()
	defer state.Unlock()
	prevServers := state.CloneServers()
	prevStatusMap := state.CloneStatusMap()
	currentServers := state.Servers()
	for i, server := range currentServers {
		if server.Name != name {
			continue
		}
		if blocked, status := state.ActionStatusInProgressLocked(name); blocked {
			return ActionError{Status: status}
		}
		currentServers[i].Pass = ""
		state.SetServers(currentServers)
		if err := s.SaveOrRollbackLocked(prevServers, prevStatusMap, nil); err != nil {
			return err
		}
		if status, ok := state.StatusMap()[name]; ok {
			status.HasPassword = false
		}
		return nil
	}
	return ErrNotFound
}

func (s *Service) CheckMutationAllowed(name string) error {
	state := s.state()
	state.Lock()
	defer state.Unlock()
	if _, found := state.FindByNameLocked(name); !found {
		return ErrNotFound
	}
	if blocked, status := state.ActionStatusInProgressLocked(name); blocked {
		return ActionError{Status: status}
	}
	return nil
}

func (s *Service) SetKey(name, key string) error {
	state := s.state()
	state.Lock()
	defer state.Unlock()
	currentServers := state.Servers()
	for i, server := range currentServers {
		if server.Name != name {
			continue
		}
		if blocked, status := state.ActionStatusInProgressLocked(name); blocked {
			return ActionError{Status: status}
		}
		if err := s.UpdateServerKey(name, key); err != nil {
			return err
		}
		currentServers[i].Key = key
		state.SetServers(currentServers)
		if status, ok := state.StatusMap()[name]; ok {
			status.HasKey = key != ""
		}
		return nil
	}
	return ErrNotFound
}

func (s *Service) ClearKey(name string) error {
	return s.SetKey(name, "")
}

func (s *Service) UpdateServerKey(name, key string) error {
	return s.repo().UpdateServerKey(name, key)
}

func (s *Service) ScanHostKey(host string, port int) (HostKeyScanResult, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return HostKeyScanResult{}, errors.New("host is required")
	}
	port = NormalizePort(port)
	key, err := s.deps.KnownHosts.scanHostKey(host, port)
	if err != nil {
		return HostKeyScanResult{}, err
	}
	line := BuildKnownHostsLine(host, port, key)
	alreadyTrusted := false
	if trusted, trustedErr := KnownHostLineExists(s.deps.KnownHosts, line); trustedErr == nil {
		alreadyTrusted = trusted
	} else {
		log.Printf("hostkey.scan trusted-check failed for %s:%d: %v", host, port, trustedErr)
	}
	return HostKeyScanResult{
		Host:              host,
		Port:              port,
		Algorithm:         key.Type(),
		FingerprintSHA256: ssh.FingerprintSHA256(key),
		KnownHostsLine:    line,
		AlreadyTrusted:    alreadyTrusted,
	}, nil
}

func (s *Service) TrustHostKey(host string, port int, expectedFingerprint string) (HostKeyTrustResult, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return HostKeyTrustResult{}, errors.New("host is required")
	}
	port = NormalizePort(port)
	fingerprint, line, alreadyTrusted, err := TrustHostKey(s.deps.KnownHosts, host, port, strings.TrimSpace(expectedFingerprint))
	if err != nil {
		return HostKeyTrustResult{}, err
	}
	message := "Host key trusted"
	if alreadyTrusted {
		message = "Host key already trusted"
	}
	return HostKeyTrustResult{
		Message:           message,
		Host:              host,
		Port:              port,
		FingerprintSHA256: fingerprint,
		KnownHostsLine:    line,
		AlreadyTrusted:    alreadyTrusted,
	}, nil
}

func (s *Service) ClearKnownHost(host string, port int) (HostKeyClearResult, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return HostKeyClearResult{}, errors.New("host is required")
	}
	port = NormalizePort(port)
	removed, err := RemoveKnownHostEntries(s.deps.KnownHosts, host, port)
	if err != nil {
		return HostKeyClearResult{}, err
	}
	message := "Known host entry not found"
	if removed > 0 {
		message = "Known host entry cleared"
	}
	return HostKeyClearResult{
		Message:        message,
		Host:           host,
		Port:           port,
		RemovedEntries: removed,
	}, nil
}
