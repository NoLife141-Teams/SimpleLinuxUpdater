package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	serverpkg "debian-updater/internal/servers"
	"github.com/gin-gonic/gin"
)

const defaultJSONRequestBytes = 1 << 20

// Preflight runs after permissions and same-origin checks. Only bounded command
// documents are buffered; multipart archives and SSH keys keep their streaming
// readers and specialized limits. Handlers retain field validation, optional
// empty bodies and compatible unknown fields.
func requestBodyBudgetMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.FullPath() == "" || c.Request.Body == nil || !strings.HasPrefix(c.FullPath(), "/api/") {
			c.Next()
			return
		}
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			c.Next()
			return
		}
		path := c.FullPath()
		switch path {
		case "/api/backup/restore", "/api/backup/verify":
			c.Next()
			return
		}
		if c.Request.Method == http.MethodPost && (path == "/api/servers/:name/key" || path == "/api/keys/global") {
			c.Next()
			return
		}
		limit := int64(defaultJSONRequestBytes)
		switch {
		case strings.HasPrefix(path, "/api/auth/"):
			limit = authMaxRequestBytes
		case strings.HasPrefix(path, "/api/servers/onboarding"):
			limit = 128 * 1024
		case path == "/api/postcheck-profiles":
			limit = 256 * 1024
		case path == "/api/package-deferrals":
			limit = packageDeferralMaxRequestBytes
		case path == "/api/backup/automation":
			limit = 4096
		case path == "/api/backup/export":
			limit = backupMaxExportRequestBytes
		case path == "/api/servers/import/preview":
			limit = serverpkg.MaxImportBytes
		case path == "/api/servers/import":
			limit = serverpkg.MaxImportSelectionBytes
		}
		raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, limit))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request payload too large"})
			} else {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
			}
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		// Native auth forms and CSV imports retain their existing decoders.
		contentType := strings.ToLower(strings.TrimSpace(c.ContentType()))
		form := (path == "/api/auth/setup" || path == "/api/auth/login") && (contentType == "application/x-www-form-urlencoded" || contentType == "multipart/form-data")
		csv := path == "/api/servers/import/preview" && c.Query("format") == "csv"
		jsonRaw := raw
		if path == "/api/servers/import/preview" {
			// The inventory file format already permits a UTF-8 BOM. Preserve
			// those original bytes for its strict parser and size accounting.
			jsonRaw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
		}
		if len(bytes.TrimSpace(jsonRaw)) != 0 && !form && !csv {
			decoder := json.NewDecoder(bytes.NewReader(jsonRaw))
			var document json.RawMessage
			if err := decoder.Decode(&document); err != nil {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
				return
			}
			if err := decoder.Decode(&document); !errors.Is(err, io.EOF) {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "request must contain exactly one JSON document"})
				return
			}
		}
		c.Next()
	}
}
