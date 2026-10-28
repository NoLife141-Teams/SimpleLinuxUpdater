package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"

	policypkg "debian-updater/internal/policies"
	serverpkg "debian-updater/internal/servers"
	"github.com/gin-gonic/gin"
)

func registerServerInventoryBulkRoutes(r *gin.Engine, deps AppDeps, commands *serverpkg.CommandService) {
	repository := policypkg.NewSQLiteRepository(policypkg.SQLiteRepositoryDeps{DB: deps.DB})
	bulk := serverpkg.NewBulkCommands(commands, serverpkg.BulkDeps{
		Epoch:    func() string { return fmt.Sprintf("%p", deps.DB()) },
		Revision: repository.LoadSchedulerStateRevision,
		CheckRevision: func(tx *sql.Tx, expected int64) error {
			var actual int64
			if err := tx.QueryRow("SELECT revision FROM " + policypkg.SchedulerStateRevisionTable + " WHERE id=1").Scan(&actual); err != nil {
				return err
			}
			if actual != expected {
				return serverpkg.ErrBulkConflict
			}
			return nil
		},
		Project: func(list []Server) (map[string]json.RawMessage, error) {
			if len(list) == 0 {
				return map[string]json.RawMessage{}, nil
			}
			projection, err := deps.PolicyService.ProjectSchedule(PolicyScheduleProjectionRequest{Now: deps.Now(), Servers: list})
			if err != nil {
				return nil, err
			}
			result := map[string]json.RawMessage{}
			for name, item := range projection.Servers {
				raw, err := json.Marshal(item)
				if err != nil {
					return nil, err
				}
				result[name] = raw
			}
			return result, nil
		},
	})
	r.POST("/api/servers/bulk/preview", func(c *gin.Context) {
		draft, err := serverpkg.DecodeBulkDraft(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		preview, err := bulk.Preview(draft)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, preview)
	})
	r.POST("/api/servers/bulk", func(c *gin.Context) {
		batchID, err := serverpkg.DecodeBulkConfirmation(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result := bulk.Commit(batchID)
		status := http.StatusOK
		if result.Outcome != serverpkg.CommandOutcomeSuccess {
			status = http.StatusConflict
			if result.Outcome == serverpkg.CommandOutcomeFailed {
				status = http.StatusInternalServerError
			}
		} else {
			for _, event := range inventoryImportAuditRecords(result.Audit) {
				audit(c, event.Action, event.TargetType, event.TargetName, event.Status, event.Message, event.Meta)
			}
			deps.NotifyDashboardEvent("servers-bulk-updated")
		}
		c.JSON(status, result)
	})
}
