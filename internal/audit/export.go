package audit

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const ExportMaxRows = 10000
const ExportMaxBytes int64 = 32 << 20
const ExportBudget = 15 * time.Second

var ErrExportLimit = errors.New("audit export exceeds 10000 events or 32 MiB; narrow the search")

// ExportFile is a private, bounded spool. Close removes it, even after a failed download.
type ExportFile struct {
	*os.File
	Count int
}

func (f *ExportFile) Close() error {
	name := f.Name()
	err := f.File.Close()
	removeErr := os.Remove(name)
	if err != nil {
		return err
	}
	return removeErr
}

type ExportManifest struct {
	Format  string     `json:"format"`
	Version int        `json:"version"`
	Filters ListFilter `json:"filters"`
	Count   int        `json:"count"`
	From    string     `json:"exported_from"`
	To      string     `json:"exported_to"`
	Order   string     `json:"order"`
}
type boundedExportWriter struct {
	ctx       context.Context
	out       io.Writer
	remaining int64
}

func (w *boundedExportWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.remaining {
		return 0, ErrExportLimit
	}
	n, err := w.out.Write(p)
	w.remaining -= int64(n)
	return n, err
}

// PrepareExport completes one SQLite read snapshot before releasing any bytes to
// HTTP. Memory is one event plus fixed encoder buffers, independent of row count.
// The deadline covers scanning, sanitation, encoding and local spooling. The
// caller closes the spool and owns its separate, bounded transfer deadline.
func (s *Service) PrepareExport(ctx context.Context, filter ListFilter, format string) (*ExportFile, error) {
	repo, ok := s.repo.(*SQLiteRepository)
	if !ok {
		return nil, errors.New("audit export requires SQLite snapshot repository")
	}
	return repo.prepareExport(ctx, filter, format, ExportMaxRows, ExportMaxBytes)
}
func (r *SQLiteRepository) prepareExport(ctx context.Context, filter ListFilter, format string, maxRows int, maxBytes int64) (result *ExportFile, err error) {
	if format != "json" && format != "csv" {
		return nil, errors.New("invalid export format")
	}
	ctx, cancel := context.WithTimeout(ctx, ExportBudget)
	defer cancel()
	file, err := os.CreateTemp("", "slu-audit-export-*")
	if err != nil {
		return nil, err
	}
	result = &ExportFile{File: file}
	defer func() {
		if err != nil {
			_ = result.Close()
			result = nil
		}
	}()
	// Own the connection explicitly so cancellation waits for database/sql's
	// asynchronous transaction rollback before returning the maintenance lease.
	conn, err := r.db().Conn(ctx)
	if err != nil {
		return result, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	filter.Page = 0
	filter.PageSize = 0
	manifest := ExportManifest{Format: "simplelinuxupdater-audit", Version: 1, Filters: filter, Order: "id DESC"}
	order := " ORDER BY id DESC"
	if filter.Order == ListOrderCreatedAt {
		order = " ORDER BY created_at DESC, id DESC"
		manifest.Order = "created_at DESC, id DESC"
	}
	where, args := auditWhereClause(filter)
	rows, err := tx.QueryContext(ctx, `SELECT id,created_at,actor,action,target_type,target_name,status,message,meta_json,request_id,client_ip FROM audit_events`+where+order, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	bounded := &boundedExportWriter{ctx: ctx, out: file, remaining: maxBytes}
	buffer := bufio.NewWriterSize(bounded, 32*1024)
	encoder := json.NewEncoder(buffer)
	csvWriter := csv.NewWriter(buffer)
	csvWriter.UseCRLF = true
	if format == "json" {
		_, err = io.WriteString(buffer, `{"items":[`)
	} else {
		err = csvWriter.Write([]string{"record_type", "manifest_json", "id", "created_at", "actor", "action", "target_type", "target_name", "status", "message", "meta_json", "request_id", "client_ip"})
	}
	if err != nil {
		return result, err
	}
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		var evt Event
		err = rows.Scan(&evt.ID, &evt.CreatedAt, &evt.Actor, &evt.Action, &evt.TargetType, &evt.TargetName, &evt.Status, &evt.Message, &evt.MetaJSON, &evt.RequestID, &evt.ClientIP)
		if err != nil {
			return result, err
		}
		if filter.FailureCause != "" && FailureCauseFromMetaJSON(evt.MetaJSON) != filter.FailureCause {
			continue
		}
		if manifest.Count >= maxRows {
			return result, ErrExportLimit
		}
		// Stored legacy metadata is sanitized again; automation receives exact public
		// values, without spreadsheet neutralization. UseNumber preserves large IDs.
		var meta map[string]any
		decoder := json.NewDecoder(strings.NewReader(evt.MetaJSON))
		decoder.UseNumber()
		if decoder.Decode(&meta) != nil {
			meta = nil
		}
		evt.MetaJSON = SanitizeMeta(meta)
		if manifest.Count == 0 || evt.CreatedAt < manifest.From {
			manifest.From = evt.CreatedAt
		}
		if evt.CreatedAt > manifest.To {
			manifest.To = evt.CreatedAt
		}
		if format == "json" {
			if manifest.Count > 0 {
				_, err = io.WriteString(buffer, ",")
				if err != nil {
					return result, err
				}
			}
			err = encoder.Encode(evt)
		} else {
			cells := []string{"event", "", strconv.FormatInt(evt.ID, 10), evt.CreatedAt, evt.Actor, evt.Action, evt.TargetType, evt.TargetName, evt.Status, evt.Message, evt.MetaJSON, evt.RequestID, evt.ClientIP}
			for i := range cells {
				cells[i] = spreadsheetCell(cells[i])
			}
			err = csvWriter.Write(cells)
		}
		if err != nil {
			return result, err
		}
		manifest.Count++
	}
	if err = rows.Err(); err != nil {
		return result, err
	}
	if err = rows.Close(); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	if format == "json" {
		_, err = io.WriteString(buffer, `],"manifest":`)
		if err == nil {
			err = encoder.Encode(manifest)
		}
		if err == nil {
			_, err = io.WriteString(buffer, "}\n")
		}
	} else {
		var raw []byte
		raw, err = json.Marshal(manifest)
		if err == nil {
			err = csvWriter.Write([]string{"manifest", string(raw), "", "", "", "", "", "", "", "", "", "", ""})
		}
		csvWriter.Flush()
		if err == nil {
			err = csvWriter.Error()
		}
	}
	if err != nil {
		return result, err
	}
	if err = buffer.Flush(); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	_, err = file.Seek(0, io.SeekStart)
	result.Count = manifest.Count
	return result, err
}

func spreadsheetCell(value string) string {
	leading := strings.TrimLeftFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '\ufeff' })
	if leading != "" && strings.ContainsRune("=+-@", rune(leading[0])) {
		return "'" + value
	}
	// Tabs and CR/LF can also trigger spreadsheet interpretation on their own.
	if strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") {
		return "'" + value
	}
	return value
}
