package servers

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const MaxBulkTargets = 1000

// BulkDraft deliberately excludes connection, credential and trust fields.
type BulkDraft struct {
	Names      []string `json:"names"`
	AddTags    []string `json:"add_tags"`
	RemoveTags []string `json:"remove_tags"`
	Disabled   *bool    `json:"disabled,omitempty"`
}
type BulkValues struct {
	Tags     []string `json:"tags"`
	Disabled bool     `json:"disabled"`
}
type BulkRow struct {
	Name           string          `json:"name"`
	Before         BulkValues      `json:"before"`
	After          BulkValues      `json:"after"`
	Errors         []string        `json:"errors"`
	BeforeSchedule json.RawMessage `json:"before_schedule"`
	AfterSchedule  json.RawMessage `json:"after_schedule"`
}
type BulkPreview struct {
	BatchID    string    `json:"batch_id"`
	Rows       []BulkRow `json:"rows"`
	CanConfirm bool      `json:"can_confirm"`
	Notice     string    `json:"notice"`
}
type BulkResult struct {
	Outcome CommandOutcome `json:"outcome"`
	Error   string         `json:"error,omitempty"`
	Preview BulkPreview    `json:"preview"`
	Audit   CommandAudit   `json:"-"`
}

// The composition supplies canonical Policy Schedule Projection and its durable
// generation. CheckRevision runs inside the inventory write transaction.
type BulkDeps struct {
	Epoch         func() string
	Revision      func() (int64, error)
	CheckRevision func(*sql.Tx, int64) error
	Project       func([]Server) (map[string]json.RawMessage, error)
}
type bulkTicket struct {
	draft       BulkDraft
	fingerprint string
	epoch       string
	revision    int64
	expires     time.Time
}
type BulkCommands struct {
	commands *CommandService
	deps     BulkDeps
	mu       sync.Mutex
	tickets  map[string]bulkTicket
}

func NewBulkCommands(c *CommandService, deps BulkDeps) *BulkCommands {
	return &BulkCommands{commands: c, deps: deps, tickets: map[string]bulkTicket{}}
}
func DecodeBulkDraft(r io.Reader) (BulkDraft, error) {
	var d BulkDraft
	raw, err := io.ReadAll(io.LimitReader(r, MaxImportSelectionBytes+1))
	if err != nil || len(raw) > MaxImportSelectionBytes {
		return d, errors.New("bulk inventory draft is too large")
	}
	if _, err := strictObject(raw, map[string]bool{"names": true, "add_tags": true, "remove_tags": true, "disabled": true}); err != nil {
		return d, errors.New("invalid bulk inventory draft; only unique names, add_tags, remove_tags and disabled fields are supported")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&d); err != nil {
		return d, errors.New("invalid bulk inventory draft values")
	}
	return d, nil
}

// DecodeBulkConfirmation permits only the opaque identity of a reviewed draft.
func DecodeBulkConfirmation(r io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 4097))
	if err != nil || len(raw) > 4096 {
		return "", errors.New("invalid bulk confirmation")
	}
	if _, err := strictObject(raw, map[string]bool{"batch_id": true}); err != nil {
		return "", errors.New("only a reviewed batch_id is accepted")
	}
	var request struct {
		BatchID string `json:"batch_id"`
	}
	if err := json.Unmarshal(raw, &request); err != nil || len(request.BatchID) != 32 {
		return "", errors.New("a reviewed batch_id is required")
	}
	if _, err := hex.DecodeString(request.BatchID); err != nil {
		return "", errors.New("a reviewed batch_id is required")
	}
	return request.BatchID, nil
}

func normalizeBulk(d BulkDraft) (BulkDraft, error) {
	if len(d.Names) == 0 || len(d.Names) > MaxBulkTargets {
		return d, errors.New("select between 1 and 1000 explicit servers")
	}
	seen := map[string]bool{}
	for _, n := range d.Names {
		if n == "" || seen[n] {
			return d, errors.New("server names must be explicit and unique")
		}
		seen[n] = true
	}
	d.Names = append([]string(nil), d.Names...)
	if d.Disabled != nil {
		value := *d.Disabled
		d.Disabled = &value
	}
	d.AddTags = ParseTags(JoinTags(d.AddTags))
	d.RemoveTags = ParseTags(JoinTags(d.RemoveTags))
	if len(d.AddTags) > 64 || len(d.RemoveTags) > 64 || len(JoinTags(d.AddTags)) > 4096 || len(JoinTags(d.RemoveTags)) > 4096 {
		return d, errors.New("use at most 64 tags and 4096 tag characters per operation")
	}
	if len(d.AddTags) == 0 && len(d.RemoveTags) == 0 && d.Disabled == nil {
		return d, errors.New("choose a tag or availability change")
	}
	for _, a := range d.AddTags {
		for _, r := range d.RemoveTags {
			if strings.EqualFold(a, r) {
				return d, errors.New("a tag cannot be both added and removed")
			}
		}
	}
	return d, nil
}
func bulkFingerprint(list []Server) string {
	// Credentials participate in conflict detection but never leave this digest.
	type revision struct {
		Server Server
		Pass   string
		Key    string
	}
	rows := make([]revision, len(list))
	for i, s := range list {
		rows[i] = revision{s, s.Pass, s.Key}
	}
	raw, _ := json.Marshal(rows)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (b *BulkCommands) previewLocked(d BulkDraft) (BulkPreview, bulkTicket, error) {
	state := b.commands.inventory.state()
	p := BulkPreview{Rows: []BulkRow{}, CanConfirm: true, Notice: "This command changes inventory only and starts no SSH or maintenance. Enabling servers resumes normal scheduling and automatic refreshes; configured policies may run. Existing accepted rollout manifests remain unchanged."}
	revision, err := b.deps.Revision()
	if err != nil {
		return p, bulkTicket{}, err
	}
	current := state.CloneServers()
	next := state.CloneServers()
	positions := map[string]int{}
	for i, s := range current {
		positions[s.Name] = i
	}
	for _, name := range d.Names {
		row := BulkRow{Name: name, Errors: []string{}}
		i, ok := positions[name]
		if !ok {
			row.Errors = append(row.Errors, "Server no longer exists")
		} else {
			old := current[i]
			candidate := old
			tags := []string{}
			for _, tag := range old.Tags {
				remove := false
				for _, r := range d.RemoveTags {
					if strings.EqualFold(tag, r) {
						remove = true
					}
				}
				if !remove {
					tags = append(tags, tag)
				}
			}
			candidate.Tags = ParseTags(JoinTags(append(tags, d.AddTags...)))
			if len(candidate.Tags) > 64 || len(JoinTags(candidate.Tags)) > 4096 {
				row.Errors = append(row.Errors, "Result exceeds 64 tags or 4096 tag characters")
			}
			if d.Disabled != nil {
				candidate.Disabled = *d.Disabled
			}
			row.Before = BulkValues{old.Tags, old.Disabled}
			row.After = BulkValues{candidate.Tags, candidate.Disabled}
			next[i] = candidate
			if blocked, status := state.ActionStatusInProgressLocked(name); blocked {
				row.Errors = append(row.Errors, "Server action in progress: "+status)
			}
		}
		if len(row.Errors) > 0 {
			p.CanConfirm = false
		}
		p.Rows = append(p.Rows, row)
	}
	before, err := b.deps.Project(current)
	if err != nil {
		return p, bulkTicket{}, err
	}
	after, err := b.deps.Project(next)
	if err != nil {
		return p, bulkTicket{}, err
	}
	afterRevision, err := b.deps.Revision()
	if err != nil {
		return p, bulkTicket{}, err
	}
	if afterRevision != revision {
		return p, bulkTicket{}, errors.New("policies changed during preview; request a new preview")
	}
	for i := range p.Rows {
		p.Rows[i].BeforeSchedule = before[p.Rows[i].Name]
		p.Rows[i].AfterSchedule = after[p.Rows[i].Name]
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return p, bulkTicket{}, err
	}
	p.BatchID = hex.EncodeToString(id[:])
	epoch := ""
	if b.deps.Epoch != nil {
		epoch = b.deps.Epoch()
	}
	return p, bulkTicket{draft: d, fingerprint: bulkFingerprint(current), epoch: epoch, revision: revision, expires: time.Now().Add(15 * time.Minute)}, nil
}
func (b *BulkCommands) Preview(d BulkDraft) (BulkPreview, error) {
	d, err := normalizeBulk(d)
	if err != nil {
		return BulkPreview{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.commands.inventory.state()
	state.Lock()
	defer state.Unlock()
	for id, t := range b.tickets {
		if time.Now().After(t.expires) {
			delete(b.tickets, id)
		}
	}
	if len(b.tickets) >= 128 {
		return BulkPreview{}, errors.New("too many active previews; retry after previews expire")
	}
	p, t, err := b.previewLocked(d)
	if err == nil {
		b.tickets[p.BatchID] = t
	}
	return p, err
}

var ErrBulkConflict = errors.New("inventory or policy revision changed")

func (b *BulkCommands) Commit(id string) BulkResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := BulkResult{Outcome: CommandOutcomeConflict, Error: "Preview expired, consumed or unknown. Refresh inventory and request a new preview."}
	ticket, ok := b.tickets[id]
	delete(b.tickets, id)
	if !ok || time.Now().After(ticket.expires) || (b.deps.Epoch != nil && ticket.epoch != b.deps.Epoch()) {
		return result
	}
	state := b.commands.inventory.state()
	state.Lock()
	defer state.Unlock()
	fresh, _, err := b.previewLocked(ticket.draft)
	if err != nil {
		result.Error = "Cannot revalidate preview; refresh and request a new preview"
		return result
	}
	result.Preview = fresh
	rev, err := b.deps.Revision()
	if err != nil || rev != ticket.revision || bulkFingerprint(state.Servers()) != ticket.fingerprint || !fresh.CanConfirm {
		result.Error = "Inventory, action state or policies changed. No servers changed; request and review a new preview."
		return result
	}
	next := state.CloneServers()
	rows := map[string]BulkRow{}
	for _, row := range fresh.Rows {
		rows[row.Name] = row
	}
	for i, s := range next {
		if row, ok := rows[s.Name]; ok {
			next[i].Tags = row.After.Tags
			next[i].Disabled = row.After.Disabled
		}
	}
	err = b.commands.inventory.repo().Save(next, func(tx *sql.Tx) error { return b.deps.CheckRevision(tx, ticket.revision) })
	if err != nil {
		result.Outcome = CommandOutcomeFailed
		if errors.Is(err, ErrBulkConflict) {
			result.Outcome = CommandOutcomeConflict
		}
		result.Error = "Inventory could not be committed. No servers changed; refresh and request a new preview."
		return result
	}
	state.SetServers(next)
	for _, s := range next {
		if _, ok := rows[s.Name]; ok {
			UpdateStatusFromServer(state.StatusMap(), s.Name, s)
		}
	}
	result.Outcome = CommandOutcomeSuccess
	result.Error = ""
	result.Preview.BatchID = id
	facts := make([]map[string]any, 0, len(fresh.Rows))
	for _, row := range fresh.Rows {
		facts = append(facts, map[string]any{"name": row.Name, "before": row.Before, "after": row.After})
	}
	result.Audit = CommandAudit{Action: "server.bulk", TargetType: "inventory", TargetName: id, Status: "success", Message: fmt.Sprintf("Bulk inventory updated: %d servers", len(facts)), Meta: map[string]any{"batch_id": id, "selected_count": len(facts), "changes": facts}}
	return result
}
