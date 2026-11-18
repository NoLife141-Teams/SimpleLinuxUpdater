package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

const statusBatchHistorySize = 20

type statusBatchSummary struct {
	ID                string `json:"id"`
	Action            string `json:"action"`
	CreatedAt         string `json:"created_at"`
	CreatedAtDisplay  string `json:"created_at_display,omitempty"`
	Pending           int    `json:"pending"`
	Terminal          int    `json:"terminal"`
	NeedsVerification int    `json:"needs_verification"`
}

type statusBatchHistoryPage struct {
	Batches    []statusBatchSummary `json:"batches"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

type statusBatchHistoryRecord struct {
	batch  statusBatch
	active bool
}

func (s *statusBatchService) history(c *gin.Context) {
	setNoStoreHeaders(c)
	principal, ok := principalForContext(c)
	if !ok {
		c.Status(http.StatusForbidden)
		return
	}
	before := c.Query("before")
	if before != "" && !validBatchID(before) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid maintenance batch history cursor"})
		return
	}
	db := s.deps.DB()
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Maintenance batch storage is unavailable"})
		return
	}
	ctx := c.Request.Context()
	records, err := s.readBatchHistory(ctx, db, principal.Account.ID, before)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Maintenance batch history cursor not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Could not read maintenance batch history"})
		return
	}
	page := statusBatchHistoryPage{Batches: make([]statusBatchSummary, 0, min(len(records), statusBatchHistorySize))}
	if len(records) > statusBatchHistorySize {
		records = records[:statusBatchHistorySize]
		page.NextCursor = records[len(records)-1].batch.ID
	}
	interpretation := s.deps.ApplicationTime.Current()
	for _, record := range records {
		batch := &record.batch
		s.projectBatch(ctx, db, batch, record.active)
		display, _ := interpretation.Format(batch.CreatedAt, "")
		summary := statusBatchSummary{ID: batch.ID, Action: batch.Action, CreatedAt: batch.CreatedAt, CreatedAtDisplay: display}
		for _, host := range batch.Hosts {
			switch host.State {
			case "queued", "admitting", "accepted", "running", "waiting_approval":
				summary.Pending++
			case "completed", "failed", "refused", "interrupted":
				summary.Terminal++
			default:
				summary.NeedsVerification++
			}
		}
		page.Batches = append(page.Batches, summary)
	}
	c.JSON(http.StatusOK, page)
}

func (s *statusBatchService) readBatchHistory(ctx context.Context, db *sql.DB, account int64, before string) ([]statusBatchHistoryRecord, error) {
	// Read the current page and its active markers together. Project after
	// closing the rows so audit reconciliation can use the database connection.
	s.activeMu.RLock()
	defer s.activeMu.RUnlock()
	var records []statusBatchHistoryRecord
	pageQuery := `SELECT id, action, created_at, actor FROM status_action_batches WHERE account_id = ?`
	args := []any{account}
	if before != "" {
		var created string
		err := db.QueryRowContext(ctx, `SELECT created_at FROM status_action_batches WHERE account_id = ? AND id = ?`, account, before).Scan(&created)
		if err != nil {
			return nil, err
		}
		pageQuery += ` AND (created_at, id) < (?, ?)`
		args = append(args, created, before)
	}
	pageQuery += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, statusBatchHistorySize+1)
	rows, err := db.QueryContext(ctx, `WITH page AS (`+pageQuery+`)
		SELECT p.id, p.action, p.created_at, p.actor, COALESCE(h.name, ''), COALESCE(h.state, ''),
			COALESCE(h.job_id, ''), COALESCE(j.status, '')
		FROM page p
		LEFT JOIN status_action_batch_hosts h ON h.batch_id = p.id
		LEFT JOIN jobs j ON j.id = h.job_id
		ORDER BY p.created_at DESC, p.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var batch statusBatch
		var host statusBatchHost
		if err := rows.Scan(&batch.ID, &batch.Action, &batch.CreatedAt, &batch.Actor,
			&host.Name, &host.State, &host.JobID, &host.JobStatus); err != nil {
			return nil, err
		}
		if len(records) == 0 || records[len(records)-1].batch.ID != batch.ID {
			_, active := s.active[batch.ID]
			records = append(records, statusBatchHistoryRecord{batch: batch, active: active})
		}
		if host.Name != "" {
			current := &records[len(records)-1].batch
			current.Hosts = append(current.Hosts, host)
		}
	}
	return records, rows.Err()
}
