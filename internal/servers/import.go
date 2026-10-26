package servers

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxImportBytes = 2 * 1024 * 1024
const MaxImportRows = 1000

// A normalized selection adds row IDs, field names and JSON escaping to an
// upload. Allow six bytes per original byte (the maximum JSON string escape)
// plus bounded per-row metadata, while keeping the file limit at 2 MiB.
const maxImportSelectionBytes = 6*MaxImportBytes + 512*MaxImportRows

// ImportEntry deliberately has no credentials, availability or trust fields.
type ImportEntry struct {
	Row  int      `json:"row"`
	Name string   `json:"name"`
	Host string   `json:"host"`
	Port int      `json:"port"`
	User string   `json:"user"`
	Tags []string `json:"tags"`
}

type ImportRow struct {
	ImportEntry
	Errors []string `json:"errors"`
}

type ImportPreview struct {
	BatchID string      `json:"batch_id"`
	Rows    []ImportRow `json:"rows"`
}

type ImportSelection struct {
	BatchID string        `json:"batch_id"`
	Rows    []ImportEntry `json:"rows"`
}

type ImportResult struct {
	Outcome CommandOutcome `json:"outcome"`
	Error   string         `json:"error,omitempty"`
	Preview ImportPreview  `json:"preview"`
	Created []string       `json:"created"`
	Audit   CommandAudit   `json:"-"`
}

var errImportFormat = errors.New("invalid inventory format. Use only name, host, port, user and tags; credentials and other fields are not supported")

// strictObject rejects duplicate keys as well as unknown fields, including
// secrets. Its errors never contain input keys, values, or decoder excerpts.
func strictObject(raw []byte, allowed map[string]bool) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errImportFormat
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || !allowed[key] {
			return nil, errImportFormat
		}
		if _, exists := fields[key]; exists {
			return nil, errImportFormat
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, errImportFormat
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, errImportFormat
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errImportFormat
	}
	return fields, nil
}

var importFields = map[string]bool{"name": true, "host": true, "port": true, "user": true, "tags": true}

func parseImportTags(raw []byte) ([]string, error) {
	// encoding/json accepts null into a string as its zero value. Preserve the
	// closed string-array schema instead of silently dropping those tags.
	var values []*string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, errImportFormat
	}
	tags := make([]string, len(values))
	for index, value := range values {
		if value == nil {
			return nil, errImportFormat
		}
		tags[index] = *value
	}
	return tags, nil
}

func parseImportObject(raw []byte, row int, withRow bool) (ImportRow, error) {
	allowed := importFields
	if withRow {
		allowed = map[string]bool{"name": true, "host": true, "port": true, "user": true, "tags": true, "row": true}
	}
	fields, err := strictObject(raw, allowed)
	if err != nil {
		return ImportRow{}, err
	}
	result := ImportRow{ImportEntry: ImportEntry{Row: row}, Errors: []string{}}
	for _, field := range []struct {
		name string
		to   any
	}{{"name", &result.Name}, {"host", &result.Host}, {"port", &result.Port}, {"user", &result.User}} {
		value, exists := fields[field.name]
		if exists && (bytes.Equal(value, []byte("null")) || json.Unmarshal(value, field.to) != nil) {
			result.Errors = append(result.Errors, "Invalid "+field.name+" type")
		}
	}
	if value, exists := fields["tags"]; exists {
		result.Tags, err = parseImportTags(value)
		if err != nil {
			result.Errors = append(result.Errors, "Invalid tags type")
		}
	}
	if withRow {
		if json.Unmarshal(fields["row"], &result.Row) != nil || result.Row < 1 || result.Row > MaxImportRows {
			return ImportRow{}, errImportFormat
		}
	}
	return result, nil
}

// ParseInventoryImport bounds both bytes and records before returning any data.
// A format/schema error rejects the complete file without echoing its contents.
func ParseInventoryImport(reader io.Reader, format string) ([]ImportRow, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, MaxImportBytes+1))
	if err != nil || len(raw) > MaxImportBytes {
		return nil, errors.New("inventory files must be at most 2 MiB")
	}
	if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		return nil, errors.New("inventory files must contain UTF-8 text")
	}
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	rows := []ImportRow{}
	switch format {
	case "json":
		decoder := json.NewDecoder(bytes.NewReader(raw))
		token, err := decoder.Token()
		if err != nil || token != json.Delim('[') {
			return nil, errImportFormat
		}
		for decoder.More() {
			if len(rows) == MaxImportRows {
				return nil, errors.New("inventory files must contain at most 1000 rows")
			}
			var value json.RawMessage
			if decoder.Decode(&value) != nil {
				return nil, errImportFormat
			}
			row, err := parseImportObject(value, len(rows)+1, false)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, errImportFormat
		}
		if _, err := decoder.Token(); err != io.EOF {
			return nil, errImportFormat
		}
	case "csv":
		reader := csv.NewReader(bytes.NewReader(raw))
		headers, err := reader.Read()
		if err != nil || len(headers) != len(importFields) {
			return nil, errImportFormat
		}
		seen := map[string]bool{}
		for _, header := range headers {
			if !importFields[header] || seen[header] {
				return nil, errImportFormat
			}
			seen[header] = true
		}
		for {
			values, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, errImportFormat
			}
			if len(rows) == MaxImportRows {
				return nil, errors.New("inventory files must contain at most 1000 rows")
			}
			fields := map[string]any{}
			rowErrors := []string{}
			for index, header := range headers {
				value := values[index]
				switch header {
				case "port":
					port := 0
					if strings.TrimSpace(value) != "" {
						var err error
						port, err = strconv.Atoi(strings.TrimSpace(value))
						if err != nil {
							rowErrors = append(rowErrors, "Invalid port type")
						}
					}
					fields[header] = port
				case "tags":
					tags := []string{}
					if strings.TrimSpace(value) != "" {
						var err error
						tags, err = parseImportTags([]byte(value))
						if err != nil {
							rowErrors = append(rowErrors, "Tags must be a JSON array of strings")
						}
					}
					fields[header] = tags
				default:
					fields[header] = value
				}
			}
			encoded, _ := json.Marshal(fields)
			row, err := parseImportObject(encoded, len(rows)+1, false)
			if err != nil {
				return nil, err
			}
			row.Errors = append(row.Errors, rowErrors...)
			rows = append(rows, row)
		}
	default:
		return nil, errors.New("choose a .csv or .json inventory file")
	}
	if len(rows) == 0 {
		return nil, errors.New("inventory file contains no server rows")
	}
	return rows, nil
}

// DecodeImportSelection uses the same closed schema as upload parsing. Client
// supplied rows are always treated as untrusted and validated again at commit.
func DecodeImportSelection(reader io.Reader) (ImportSelection, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, maxImportSelectionBytes+1))
	if err != nil || len(raw) > maxImportSelectionBytes || !utf8.Valid(raw) {
		return ImportSelection{}, errImportFormat
	}
	fields, err := strictObject(raw, map[string]bool{"batch_id": true, "rows": true})
	if err != nil {
		return ImportSelection{}, err
	}
	var selection ImportSelection
	if json.Unmarshal(fields["batch_id"], &selection.BatchID) != nil || len(selection.BatchID) != 32 {
		return selection, errImportFormat
	}
	if _, err := hex.DecodeString(selection.BatchID); err != nil {
		return selection, errImportFormat
	}
	var entries []json.RawMessage
	if json.Unmarshal(fields["rows"], &entries) != nil || len(entries) == 0 || len(entries) > MaxImportRows {
		return selection, errImportFormat
	}
	seen := map[int]bool{}
	for _, raw := range entries {
		row, err := parseImportObject(raw, 0, true)
		if err != nil || len(row.Errors) != 0 || seen[row.Row] {
			return selection, errImportFormat
		}
		seen[row.Row] = true
		selection.Rows = append(selection.Rows, row.ImportEntry)
	}
	return selection, nil
}

func validateImportRows(rows []ImportRow, inventory []Server) []ImportRow {
	validated := make([]ImportRow, len(rows))
	seenNames := map[string]bool{}
	seenEndpoints := map[ServerEndpoint]bool{}
	for index, raw := range rows {
		row := raw
		row.Errors = append([]string{}, raw.Errors...)
		row.Name, row.Host, row.User = strings.TrimSpace(row.Name), CanonicalServerHost(row.Host), strings.TrimSpace(row.User)
		row.Tags = ParseTags(JoinTags(row.Tags))
		if row.Tags == nil {
			row.Tags = []string{}
		}
		if row.Name == "" || len(row.Name) > 128 || strings.IndexFunc(row.Name, unicode.IsControl) >= 0 {
			row.Errors = append(row.Errors, "Name must contain 1–128 characters without control characters")
		}
		if !validOnboardingHost(row.Host) || len(row.Host) > 253 {
			row.Errors = append(row.Errors, "Host must be a DNS name or IP address without a URL or port")
		}
		if !IsValidSSHUsername(row.User) {
			row.Errors = append(row.Errors, "Invalid SSH user")
		}
		if row.Port < 0 || row.Port > 65535 {
			row.Errors = append(row.Errors, "Port must be between 1 and 65535, or omitted for 22")
		}
		row.Port = NormalizePort(row.Port)
		if len(row.Tags) > 64 || len(JoinTags(row.Tags)) > 4096 {
			row.Errors = append(row.Errors, "Use at most 64 tags and 4096 tag characters")
		}
		name := NormalizeServerName(row.Name)
		endpoint := NormalizeServerEndpoint(row.Host, row.Port)
		if ServerNameExists(inventory, row.Name, -1) {
			row.Errors = append(row.Errors, "Name already exists in inventory")
		}
		if ServerEndpointExists(inventory, row.Host, row.Port, -1) {
			row.Errors = append(row.Errors, "Endpoint already exists in inventory")
		}
		if seenNames[name] {
			row.Errors = append(row.Errors, "Duplicate name in file; keep the first valid row")
		}
		if seenEndpoints[endpoint] {
			row.Errors = append(row.Errors, "Duplicate endpoint in file; keep the first valid row")
		}
		if len(row.Errors) == 0 {
			seenNames[name], seenEndpoints[endpoint] = true, true
		}
		validated[index] = row
	}
	return validated
}

func (c *CommandService) PreviewImport(rows []ImportRow) (ImportPreview, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return ImportPreview{}, errors.New("cannot prepare inventory preview; retry shortly")
	}
	state := c.inventory.state()
	state.Lock()
	defer state.Unlock()
	return ImportPreview{BatchID: hex.EncodeToString(id[:]), Rows: validateImportRows(rows, state.Servers())}, nil
}

func (c *CommandService) CommitImport(selection ImportSelection) ImportResult {
	result := ImportResult{Outcome: CommandOutcomeInvalid, Created: []string{}, Preview: ImportPreview{BatchID: selection.BatchID, Rows: []ImportRow{}}}
	result.Audit = CommandAudit{Action: "server.import", TargetType: "inventory", TargetName: selection.BatchID, Status: "failure", Message: "Inventory import rejected", Meta: map[string]any{"batch_id": selection.BatchID, "selected_count": len(selection.Rows), "created_count": 0}}
	if len(selection.Rows) == 0 || len(selection.Rows) > MaxImportRows {
		result.Error = "Select between 1 and 1000 valid rows"
		return result
	}
	state := c.inventory.state()
	state.Lock()
	defer state.Unlock()
	rows := make([]ImportRow, len(selection.Rows))
	for index, entry := range selection.Rows {
		rows[index] = ImportRow{ImportEntry: entry}
	}
	result.Preview.Rows = validateImportRows(rows, state.Servers())
	for _, row := range result.Preview.Rows {
		if len(row.Errors) != 0 {
			result.Outcome, result.Error = CommandOutcomeConflict, "Inventory changed or selected rows are invalid. Review the refreshed preview and confirm again; no servers were imported"
			return result
		}
	}
	next := state.CloneServers()
	for _, row := range result.Preview.Rows {
		next = append(next, Server{Name: row.Name, Host: row.Host, Port: row.Port, User: row.User, Tags: row.Tags, Disabled: true})
	}
	// Persist the candidate inventory and its existing transaction hooks before
	// publishing any servers or statuses. All mutations share this state lock.
	var hook TxHook
	if prune := c.inventory.deps.PrunePolicyOverridesForServers; prune != nil {
		hook = func(tx *sql.Tx) error { return prune(tx, next) }
	}
	if err := c.inventory.repo().Save(next, hook); err != nil {
		result.Outcome, result.Error = CommandOutcomeFailed, "Inventory could not be saved. No servers were imported; review and retry"
		result.Audit.Message = "Inventory import persistence failed"
		return result
	}
	state.SetServers(next)
	for _, server := range next[len(next)-len(selection.Rows):] {
		state.StatusMap()[server.Name] = NewIdleStatus(server)
		result.Created = append(result.Created, server.Name)
	}
	result.Outcome = CommandOutcomeSuccess
	result.Audit.Status, result.Audit.Message = "success", "Inventory imported as disabled servers"
	result.Audit.Meta["created_count"], result.Audit.Meta["created_names"] = len(result.Created), result.Created
	return result
}
