package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStatusBatchHistoryPagesRemainStableAndAccountScoped(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	seedBrowserBatchHistory(t, app.Deps)
	read := func(rec *httptest.ResponseRecorder) statusBatchHistoryPage {
		t.Helper()
		var page statusBatchHistoryPage
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &page) != nil {
			t.Fatalf("read history: %d %s", rec.Code, rec.Body.String())
		}
		return page
	}
	const path = "/api/status/batches"
	first := read(identityRequest(app, "GET", path, cookie, "", nil))
	if len(first.Batches) != statusBatchHistorySize || first.NextCursor != fmt.Sprintf("%032x", 2) {
		t.Fatalf("wrong initial page: %+v", first)
	}
	for i, batch := range first.Batches {
		if batch.ID != fmt.Sprintf("%032x", 21-i) || batch.Terminal != 1 || batch.CreatedAtDisplay == "" {
			t.Fatalf("unstable first page at position %d: %+v", i, batch)
		}
	}
	db := app.Deps.DB()
	seedStatusBatchRetryHistory(t, db, strings.Repeat("f", 32), "", "update", "refused", statusBatchTarget{Name: "new", ServerIdentity: "new"})
	second := read(identityRequest(app, "GET", path+"?before="+first.NextCursor, cookie, "", nil))
	if len(second.Batches) != 1 || second.NextCursor != "" {
		t.Fatalf("unexpected older page after insertion: %+v", second)
	}
	if old := second.Batches[0]; old.ID != fmt.Sprintf("%032x", 1) || old.Pending != 1 || old.Terminal != 1 || old.NeedsVerification != 1 {
		t.Fatalf("wrong outcome summary for older batch: %+v", old)
	}
	if _, err := app.Deps.AuthService.CreateAccount("other", testPasswordStrong, "operator"); err != nil {
		t.Fatal(err)
	}
	other := identityLogin(t, app, "other")
	if empty := read(identityRequest(app, "GET", path, other, "", nil)); len(empty.Batches) != 0 {
		t.Fatalf("history shared another account's batches: %+v", empty)
	}
	for _, target := range []string{path + "?before=" + first.NextCursor, path + "/" + first.NextCursor} {
		if rec := identityRequest(app, "GET", target, other, "", nil); rec.Code != http.StatusNotFound {
			t.Fatalf("foreign cursor or detail was accepted (GET %s, HTTP %d)", target, rec.Code)
		}
	}
	if rec := identityRequest(app, "GET", path+"?before=bad", cookie, "", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor was accepted: %d", rec.Code)
	}
}
