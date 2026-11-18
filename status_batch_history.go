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
	s.activeMu.RLock()
	page, err := readStatusBatchHistory(c.Request.Context(), db, principal.Account.ID, before, s.active)
	s.activeMu.RUnlock()
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Maintenance batch history cursor not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Could not read maintenance batch history"})
		return
	}
	interpretation := s.deps.ApplicationTime.Current()
	for i := range page.Batches {
		batch := &page.Batches[i]
		batch.CreatedAtDisplay, _ = interpretation.Format(batch.CreatedAt, "")
	}
	c.JSON(http.StatusOK, page)
}

func readStatusBatchHistory(ctx context.Context, db *sql.DB, account int64, before string, active map[string]struct{}) (statusBatchHistoryPage, error) {
	page := statusBatchHistoryPage{Batches: make([]statusBatchSummary, 0, statusBatchHistorySize+1)}
	pageQuery := `SELECT id, action, created_at FROM status_action_batches WHERE account_id = ?`
	args := []any{account}
	if before != "" {
		var created string
		err := db.QueryRowContext(ctx, `SELECT created_at FROM status_action_batches WHERE account_id = ? AND id = ?`, account, before).Scan(&created)
		if err != nil {
			return page, err
		}
		pageQuery += ` AND (created_at, id) < (?, ?)`
		args = append(args, created, before)
	}
	pageQuery += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, statusBatchHistorySize+1)
	rows, err := db.QueryContext(ctx, `WITH page AS (`+pageQuery+`)
		SELECT p.id, p.action, p.created_at, COUNT(h.name),
			COUNT(*) FILTER (WHERE h.state = 'queued'),
			COUNT(*) FILTER (WHERE h.state = 'admitting'),
			COUNT(*) FILTER (WHERE h.state = 'accepted' AND j.status IN ('queued', 'running', 'waiting_approval')),
			COUNT(*) FILTER (WHERE h.state = 'uncertain' OR (h.state = 'accepted' AND j.id IS NULL))
		FROM page p
		LEFT JOIN status_action_batch_hosts h ON h.batch_id = p.id
		LEFT JOIN jobs j ON j.id = h.job_id
		GROUP BY p.id, p.action, p.created_at
		ORDER BY p.created_at DESC, p.id DESC`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var batch statusBatchSummary
		var total, queued, admitting int
		if err := rows.Scan(&batch.ID, &batch.Action, &batch.CreatedAt, &total, &queued, &admitting,
			&batch.Pending, &batch.NeedsVerification); err != nil {
			return page, err
		}
		if _, running := active[batch.ID]; running {
			batch.Pending += queued + admitting
		} else {
			batch.NeedsVerification += admitting
		}
		batch.Terminal = total - batch.Pending - batch.NeedsVerification
		page.Batches = append(page.Batches, batch)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Batches) > statusBatchHistorySize {
		page.Batches = page.Batches[:statusBatchHistorySize]
		page.NextCursor = page.Batches[len(page.Batches)-1].ID
	}
	return page, nil
}
