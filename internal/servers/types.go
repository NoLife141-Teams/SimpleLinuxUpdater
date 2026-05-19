package servers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
)

var (
	ErrRequiredFields      = errors.New("name, host, and user are required")
	ErrInvalidSSHUsername  = errors.New("invalid ssh username")
	ErrNameExists          = errors.New("server name already exists")
	ErrHostExists          = errors.New("server host already exists")
	ErrNotFound            = errors.New("server not found")
	ErrActionInProgress    = errors.New("action already in progress")
	ErrFingerprintMismatch = errors.New("host key fingerprint mismatch")
)

type Server struct {
	Name string   `json:"name"`
	Host string   `json:"host"`
	Port int      `json:"port"`
	User string   `json:"user"`
	Pass string   `json:"pass"`
	Key  string   `json:"-"`
	Tags []string `json:"tags"`
}

func (s Server) MarshalJSON() ([]byte, error) {
	type serverResponse struct {
		Name string   `json:"name"`
		Host string   `json:"host"`
		Port int      `json:"port"`
		User string   `json:"user"`
		Tags []string `json:"tags"`
	}
	return json.Marshal(serverResponse{
		Name: s.Name,
		Host: s.Host,
		Port: s.Port,
		User: s.User,
		Tags: append([]string(nil), s.Tags...),
	})
}

type ServerStatus struct {
	Name           string          `json:"name"`
	Host           string          `json:"host"`
	Port           int             `json:"port"`
	User           string          `json:"user"`
	Status         string          `json:"status"`
	ApprovalScope  string          `json:"-"`
	Logs           string          `json:"logs"`
	Upgradable     []string        `json:"upgradable"`
	PendingUpdates []PendingUpdate `json:"pending_updates"`
	HasPassword    bool            `json:"has_password"`
	HasKey         bool            `json:"has_key"`
	Tags           []string        `json:"tags"`
}

type PendingUpdate struct {
	Package          string   `json:"package"`
	CurrentVersion   string   `json:"current_version,omitempty"`
	CandidateVersion string   `json:"candidate_version,omitempty"`
	Source           string   `json:"source,omitempty"`
	Security         bool     `json:"security"`
	CVEs             []string `json:"cves"`
	CVEState         string   `json:"cve_state"`
	Raw              string   `json:"raw"`
}

type HostKeyScanResult struct {
	Host              string
	Port              int
	Algorithm         string
	FingerprintSHA256 string
	KnownHostsLine    string
	AlreadyTrusted    bool
}

type HostKeyTrustResult struct {
	Message           string
	Host              string
	Port              int
	FingerprintSHA256 string
	KnownHostsLine    string
	AlreadyTrusted    bool
}

type HostKeyClearResult struct {
	Message        string
	Host           string
	Port           int
	RemovedEntries int
}

type ActionError struct {
	Status string
}

func (e ActionError) Error() string {
	return ErrActionInProgress.Error()
}

func (e ActionError) Unwrap() error {
	return ErrActionInProgress
}

type State struct {
	mu               *sync.Mutex
	servers          *[]Server
	statusMap        *map[string]*ServerStatus
	statusInProgress func(string) bool
}

func NewState(mu *sync.Mutex, servers *[]Server, statusMap *map[string]*ServerStatus, statusInProgress func(string) bool) *State {
	if statusInProgress == nil {
		statusInProgress = defaultStatusInProgress
	}
	return &State{
		mu:               mu,
		servers:          servers,
		statusMap:        statusMap,
		statusInProgress: statusInProgress,
	}
}

func defaultStatusInProgress(status string) bool {
	return status == "updating" ||
		status == "pending_approval" ||
		status == "approved" ||
		status == "upgrading" ||
		status == "autoremove" ||
		status == "sudoers" ||
		status == "facts_refresh"
}

func (s *State) Lock() {
	s.mu.Lock()
}

func (s *State) Unlock() {
	s.mu.Unlock()
}

func (s *State) Servers() []Server {
	return *s.servers
}

func (s *State) SetServers(next []Server) {
	*s.servers = next
}

func (s *State) StatusMap() map[string]*ServerStatus {
	return *s.statusMap
}

func (s *State) SetStatusMap(next map[string]*ServerStatus) {
	*s.statusMap = next
}

func (s *State) CloneServers() []Server {
	return CloneServers(*s.servers)
}

func (s *State) CloneStatusMap() map[string]*ServerStatus {
	return CloneStatusMap(*s.statusMap)
}

func (s *State) ListStatuses() []ServerStatus {
	s.Lock()
	defer s.Unlock()
	servers := *s.servers
	statusMap := *s.statusMap
	statuses := make([]ServerStatus, 0, len(servers))
	for _, server := range servers {
		status := statusMap[server.Name]
		if status == nil {
			continue
		}
		status.Host = server.Host
		status.Port = NormalizePort(server.Port)
		status.User = server.User
		status.HasPassword = server.Pass != ""
		status.HasKey = server.Key != ""
		status.Tags = server.Tags
		copyStatus := CloneServerStatus(status)
		statuses = append(statuses, *copyStatus)
	}
	return statuses
}

func (s *State) CurrentStatusSnapshot(name string) *ServerStatus {
	s.Lock()
	defer s.Unlock()
	return CloneServerStatus((*s.statusMap)[name])
}

func (s *State) RestoreStatusSnapshot(name string, snapshot *ServerStatus) {
	s.Lock()
	defer s.Unlock()
	if snapshot == nil {
		delete(*s.statusMap, name)
		return
	}
	(*s.statusMap)[name] = CloneServerStatus(snapshot)
}

func (s *State) CurrentStatusLogs(name string) string {
	snapshot := s.CurrentStatusSnapshot(name)
	if snapshot == nil {
		return ""
	}
	return snapshot.Logs
}

func (s *State) ActiveActionNames() []string {
	s.Lock()
	defer s.Unlock()
	names := make([]string, 0)
	for name, status := range *s.statusMap {
		if status == nil || !s.statusInProgress(status.Status) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *State) FindByNameLocked(name string) (Server, bool) {
	for _, server := range *s.servers {
		if server.Name == name {
			return server, true
		}
	}
	return Server{}, false
}

func (s *State) ActionStatusInProgressLocked(name string) (bool, string) {
	status := (*s.statusMap)[name]
	if status == nil {
		return false, ""
	}
	return s.statusInProgress(status.Status), status.Status
}

func (s *State) ActionStatusInProgress(name string) (bool, string) {
	s.Lock()
	defer s.Unlock()
	return s.ActionStatusInProgressLocked(name)
}

func (s *State) BeginAction(name, newStatus string) (Server, error) {
	s.Lock()
	defer s.Unlock()
	status, exists := (*s.statusMap)[name]
	if !exists || status == nil {
		return Server{}, sql.ErrNoRows
	}
	if s.statusInProgress(status.Status) {
		return Server{}, ErrActionInProgress
	}
	server, found := s.FindByNameLocked(name)
	if !found {
		return Server{}, sql.ErrNoRows
	}
	status.Status = newStatus
	if strings.TrimSpace(status.Logs) == "" {
		status.Logs = "Starting Linux Updater..."
	}
	return server, nil
}

func (s *State) BeginTransientAction(name, newStatus string) (Server, *ServerStatus, error) {
	s.Lock()
	defer s.Unlock()
	status, exists := (*s.statusMap)[name]
	if !exists || status == nil {
		return Server{}, nil, sql.ErrNoRows
	}
	if s.statusInProgress(status.Status) {
		return Server{}, nil, ErrActionInProgress
	}
	server, found := s.FindByNameLocked(name)
	if !found {
		return Server{}, nil, sql.ErrNoRows
	}
	snapshot := CloneServerStatus(status)
	status.Status = newStatus
	return server, snapshot, nil
}

func (s *State) ApprovePendingUpdate(name, scope string) (exists bool, approved bool) {
	normalizedScope := NormalizeApprovalScope(scope)
	s.Lock()
	defer s.Unlock()
	status, exists := (*s.statusMap)[name]
	if !exists || status == nil {
		return exists, false
	}
	if status.Status != "pending_approval" {
		return exists, false
	}
	status.ApprovalScope = normalizedScope
	status.Status = "approved"
	return exists, true
}

func (s *State) CancelPendingUpdate(name string) (exists bool, cancelled bool) {
	s.Lock()
	defer s.Unlock()
	status, exists := (*s.statusMap)[name]
	if !exists || status == nil {
		return exists, false
	}
	if status.Status != "pending_approval" {
		return exists, false
	}
	status.Status = "cancelled"
	status.ApprovalScope = ""
	status.Logs = ""
	status.Upgradable = nil
	status.PendingUpdates = nil
	return exists, true
}

func CloneServers(src []Server) []Server {
	if src == nil {
		return nil
	}
	dst := make([]Server, len(src))
	for i, server := range src {
		server.Tags = append([]string(nil), server.Tags...)
		dst[i] = server
	}
	return dst
}

func CloneStatusMap(src map[string]*ServerStatus) map[string]*ServerStatus {
	dst := make(map[string]*ServerStatus, len(src))
	for name, status := range src {
		dst[name] = CloneServerStatus(status)
	}
	return dst
}

func ClonePendingUpdates(src []PendingUpdate) []PendingUpdate {
	if src == nil {
		return nil
	}
	dst := make([]PendingUpdate, len(src))
	for i, update := range src {
		dst[i] = update
		dst[i].CVEs = append([]string(nil), update.CVEs...)
	}
	return dst
}

func CloneServerStatus(status *ServerStatus) *ServerStatus {
	if status == nil {
		return nil
	}
	copyStatus := *status
	copyStatus.Upgradable = append([]string(nil), status.Upgradable...)
	copyStatus.PendingUpdates = ClonePendingUpdates(status.PendingUpdates)
	copyStatus.Tags = append([]string(nil), status.Tags...)
	return &copyStatus
}

func NewIdleStatus(server Server) *ServerStatus {
	return &ServerStatus{
		Name:           server.Name,
		Host:           server.Host,
		Port:           NormalizePort(server.Port),
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

func UpdateStatusFromServer(statusMap map[string]*ServerStatus, name string, server Server) {
	if statusMap[name] == nil {
		statusMap[name] = &ServerStatus{
			Name:           server.Name,
			Status:         "idle",
			Upgradable:     []string{},
			PendingUpdates: []PendingUpdate{},
		}
	}
	statusMap[name].Host = server.Host
	statusMap[name].Port = NormalizePort(server.Port)
	statusMap[name].User = server.User
	statusMap[name].HasPassword = server.Pass != ""
	statusMap[name].HasKey = server.Key != ""
	statusMap[name].Tags = server.Tags
}
