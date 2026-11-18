package notifications

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

const deliveryHistoryPageSize = 20

var ErrInvalidDeliveryHistoryQuery = errors.New("invalid delivery history filter or cursor")

type DeliveryHistoryReader interface {
	DeliveryHistory(context.Context, DeliveryHistoryQuery) (DeliveryHistoryPage, error)
}

type DeliveryHistoryQuery struct {
	Destination string
	State       string
	JobID       string
	Before      string
}

type DeliveryHistoryItem struct {
	Destination        string `json:"destination"`
	State              string `json:"state"`
	EventType          string `json:"event_type"`
	JobID              string `json:"job_id,omitempty"`
	ApprovalGeneration uint64 `json:"approval_generation,omitempty"`
	Attempts           int    `json:"attempts"`
	CreatedAt          string `json:"created_at"`
	AttemptedAt        string `json:"attempted_at,omitempty"`
	CompletedAt        string `json:"completed_at,omitempty"`
	NextAttemptAt      string `json:"next_attempt_at,omitempty"`
	Reason             string `json:"reason,omitempty"`
}

type DeliveryHistoryPage struct {
	Items            []DeliveryHistoryItem `json:"items"`
	NextCursor       string                `json:"next_cursor,omitempty"`
	RetentionSeconds int64                 `json:"retention_seconds"`
}

func validHistoryID(value string, size int) bool {
	if len(value) != size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validHistoryJobID(value string) bool {
	if validHistoryID(value, 16) {
		return true
	}
	timestamp, ok := strings.CutPrefix(value, "job-")
	if !ok || len(timestamp) > 19 {
		return false
	}
	number, err := strconv.ParseUint(timestamp, 10, 63)
	return err == nil && number > 0 && strconv.FormatUint(number, 10) == timestamp
}

func historyTimestamp(value string) string {
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return ""
	}
	return value
}

func historyCursor(value string) (createdAt, id string, ok bool) {
	if len(value) > 160 {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", "", false
	}
	createdAt, id, ok = strings.Cut(string(raw), "\n")
	return createdAt, id, ok && historyTimestamp(createdAt) != "" && validHistoryID(id, 32)
}

func historyReason(state, message string, httpStatus int) string {
	switch state {
	case outboxStateSkipped:
		if message == "destination configuration changed after notification admission" {
			return "Destination configuration changed after admission."
		}
		return "Delivery was no longer eligible."
	case outboxStateFailed, outboxStateRetrying:
		if httpStatus >= 100 && httpStatus <= 599 {
			return fmt.Sprintf("Destination returned HTTP %d.", httpStatus)
		}
		switch message {
		case "Webhook delivery was canceled.":
			return "Delivery was canceled."
		case "Webhook delivery timed out.":
			return "Delivery timed out."
		default:
			return "Delivery failed."
		}
	default:
		return ""
	}
}

func (s *Service) DeliveryHistory(ctx context.Context, filter DeliveryHistoryQuery) (DeliveryHistoryPage, error) {
	var args []any
	const jobID = `CASE WHEN json_valid(meta_json) THEN json_extract(meta_json, '$.job_id') END`
	const generation = `CASE WHEN json_valid(meta_json) THEN json_extract(meta_json, '$.approval_generation') END`
	where := `WHERE destination IN ('webhook', 'discord', 'telegram')
		AND state IN ('pending', 'claimed', 'retrying', 'succeeded', 'failed', 'skipped')`
	if filter.Destination != "" {
		if !slices.Contains([]string{DestinationWebhook, DestinationDiscord, DestinationTelegram}, filter.Destination) {
			return DeliveryHistoryPage{}, ErrInvalidDeliveryHistoryQuery
		}
		where += " AND destination = ?"
		args = append(args, filter.Destination)
	}
	if filter.State != "" {
		if !slices.Contains([]string{outboxStatePending, outboxStateClaimed, outboxStateRetrying, outboxStateSucceeded, outboxStateFailed, outboxStateSkipped}, filter.State) {
			return DeliveryHistoryPage{}, ErrInvalidDeliveryHistoryQuery
		}
		where += " AND state = ?"
		args = append(args, filter.State)
	}
	if filter.JobID != "" {
		if !validHistoryJobID(filter.JobID) {
			return DeliveryHistoryPage{}, ErrInvalidDeliveryHistoryQuery
		}
		where += " AND " + jobID + " = ?"
		args = append(args, filter.JobID)
	}
	if filter.Before != "" {
		createdAt, id, ok := historyCursor(filter.Before)
		if !ok {
			return DeliveryHistoryPage{}, ErrInvalidDeliveryHistoryQuery
		}
		where += " AND (rtrim(created_at, 'Z'), id) < (rtrim(?, 'Z'), ?)"
		args = append(args, createdAt, id)
	}

	db := s.database()
	if db == nil {
		return DeliveryHistoryPage{}, errors.New("notification outbox database is unavailable")
	}
	rows, err := db.QueryContext(ctx, `SELECT id, destination, state, event_type, attempts,
		created_at, attempted_at, completed_at, next_attempt_at, status_code, error,
		COALESCE(`+jobID+`, ''), CAST(COALESCE(`+generation+`, 0) AS INTEGER)
		FROM notification_outbox `+where+` ORDER BY rtrim(created_at, 'Z') DESC, id DESC LIMIT ?`, append(args, deliveryHistoryPageSize+1)...)
	if err != nil {
		return DeliveryHistoryPage{}, err
	}
	defer rows.Close()
	page := DeliveryHistoryPage{
		Items:            make([]DeliveryHistoryItem, 0, deliveryHistoryPageSize),
		RetentionSeconds: int64(s.deps.Retention / time.Second),
	}
	var nextCursor string
	for rows.Next() {
		if len(page.Items) == deliveryHistoryPageSize {
			page.NextCursor = nextCursor
			break
		}
		var item DeliveryHistoryItem
		var id, rawReason string
		var approvalGeneration int64
		var httpStatus int
		if err := rows.Scan(&id, &item.Destination, &item.State, &item.EventType, &item.Attempts,
			&item.CreatedAt, &item.AttemptedAt, &item.CompletedAt, &item.NextAttemptAt, &httpStatus, &rawReason,
			&item.JobID, &approvalGeneration); err != nil {
			return DeliveryHistoryPage{}, err
		}
		nextCursor = ""
		if validHistoryID(id, 32) && historyTimestamp(item.CreatedAt) != "" {
			nextCursor = base64.RawURLEncoding.EncodeToString([]byte(item.CreatedAt + "\n" + id))
		}
		item.CreatedAt = historyTimestamp(item.CreatedAt)
		item.AttemptedAt = historyTimestamp(item.AttemptedAt)
		item.CompletedAt = historyTimestamp(item.CompletedAt)
		if item.State == outboxStateRetrying {
			item.NextAttemptAt = historyTimestamp(item.NextAttemptAt)
		} else {
			item.NextAttemptAt = ""
		}
		if !slices.Contains(supportedEvents, item.EventType) && item.EventType != EventReminder && item.EventType != EventTest {
			item.EventType = "notification"
		}
		if !validHistoryJobID(item.JobID) {
			item.JobID = ""
		} else if approvalGeneration > 0 && (item.EventType == EventApprovalPublished || item.EventType == EventApprovalExpired) {
			item.ApprovalGeneration = uint64(approvalGeneration)
		}
		item.Reason = historyReason(item.State, rawReason, httpStatus)
		page.Items = append(page.Items, item)
	}
	return page, rows.Err()
}
