package main

import (
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	auditpkg "debian-updater/internal/audit"
	authpkg "debian-updater/internal/auth"
	serverpkg "debian-updater/internal/servers"
	"github.com/gin-gonic/gin"
)

func TestIdentityAuditMetaPreservesFieldsAcrossAllocationHintBoundary(t *testing.T) {
	for _, bearer := range []bool{false, true} {
		for _, count := range []int{0, 63, 64, 65, 4096} {
			t.Run(fmt.Sprintf("bearer=%t/fields=%d", bearer, count), func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Set("auth_principal", authenticatedPrincipal{Account: authpkg.Account{ID: 7}, Token: authpkg.PersonalToken{ID: "0123456789abcdef0123456789abcdef"}, Bearer: bearer})
				var meta map[string]any
				if count > 0 {
					meta = make(map[string]any)
					for i := range count {
						meta[fmt.Sprintf("field-%d", i)] = i
					}
				}
				before := auditpkg.CloneMeta(meta)
				out := identityAuditMeta(c, meta)
				want := count + 1
				if bearer {
					want++
				}
				if len(out) != want || out["account_id"] != int64(7) || (bearer && out["personal_token_id"] != "0123456789abcdef0123456789abcdef") {
					t.Fatal("request audit identity or metadata fields lost")
				}
				if count > 0 && !reflect.DeepEqual(meta, before) {
					t.Fatal("request audit enrichment mutated caller")
				}
				for key, value := range meta {
					if out[key] != value {
						t.Fatalf("request audit lost %s", key)
					}
				}
			})
		}
	}
}

func TestInventoryImportAuditCopiesLargeFieldSetsWithoutLosingTargets(t *testing.T) {
	names := make([]string, 100)
	for i := range names {
		names[i] = fmt.Sprintf("%03d-%s", i, strings.Repeat("n", 75))
	}
	meta := map[string]any{"created_names": names, "batch_id": "controlled-batch", "part": -1, "parts": -1}
	for i := range 65 {
		meta[fmt.Sprintf("field-%d", i)] = i
	}
	before := auditpkg.CloneMeta(meta)
	records := inventoryImportAuditRecords(serverpkg.CommandAudit{Action: "server.import", Meta: meta})
	if len(records) < 2 || !reflect.DeepEqual(meta, before) {
		t.Fatal("chunking failed or mutated caller metadata")
	}
	var recovered []string
	for i, record := range records {
		if len(auditpkg.SanitizeMeta(record.Meta)) > auditpkg.MetaMaxLen || record.Meta["part"] != i+1 || record.Meta["parts"] != len(records) {
			t.Fatal("chunk bounds or numbering lost")
		}
		for field := range 65 {
			key := fmt.Sprintf("field-%d", field)
			if record.Meta[key] != field {
				t.Fatalf("chunk lost %s", key)
			}
		}
		recovered = append(recovered, record.Meta["created_names"].([]string)...)
	}
	if !reflect.DeepEqual(recovered, names) {
		t.Fatal("chunked audit dropped or reordered target names")
	}
}
