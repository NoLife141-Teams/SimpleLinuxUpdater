package audit

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func exportTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;"); err != nil {
		t.Fatal(err)
	}
	if err = EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}
func seedExportEvent(t *testing.T, r *SQLiteRepository, evt Event) {
	t.Helper()
	if err := r.Write(evt); err != nil {
		t.Fatal(err)
	}
}

type exportedAudit struct {
	Items    []Event        `json:"items"`
	Manifest ExportManifest `json:"manifest"`
}

func TestAuditExportSnapshotEntireFilteredResultWithConcurrentWrites(t *testing.T) {
	db := exportTestDB(t)
	repo := NewSQLiteRepository(func() *sql.DB { return db })
	svc := NewService(ServiceOptions{Repository: repo})
	for i := 0; i < 800; i++ {
		action := "update.complete"
		if i%3 == 0 {
			action = "server.create"
		}
		seedExportEvent(t, repo, Event{CreatedAt: fmt.Sprintf("2026-10-27T12:%02d:%02dZ", i/60, i%60), Actor: "account:1", Action: action, TargetType: "server", TargetName: "fleet", Status: "failure", MetaJSON: `{"last_error_class":"network"}`})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var writes sync.WaitGroup
	writes.Add(1)
	started := make(chan struct{})
	go func() {
		defer writes.Done()
		close(started)
		for i := 0; i < 100; i++ {
			if ctx.Err() != nil {
				return
			}
			err := repo.Write(Event{CreatedAt: "2026-10-27T13:00:00Z", Actor: "account:1", Action: "update.complete", TargetType: "server", TargetName: "fleet", Status: "failure", MetaJSON: `{"last_error_class":"network"}`})
			if err != nil {
				t.Errorf("concurrent write: %v", err)
				return
			}
		}
	}()
	<-started
	filter := ListFilter{Actor: "account:1", Action: "update.complete", TargetType: "server", TargetName: "fleet", Status: "failure", FailureCause: "error_class:network", From: "2026-10-27T00:00:00Z", To: "2026-10-28T00:00:00Z", Page: 3, PageSize: 10}
	file, err := svc.PrepareExport(ctx, filter, "json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var got exportedAudit
	if err = json.NewDecoder(file).Decode(&got); err != nil {
		t.Fatal(err)
	}
	cancel()
	writes.Wait()
	if len(got.Items) <= 200 || got.Manifest.Count != len(got.Items) {
		t.Fatalf("full result missing: count=%d", got.Manifest.Count)
	}
	watermark := got.Items[0].ID
	expected := []int64{}
	rows, err := db.Query("SELECT id FROM audit_events WHERE id<=? AND action='update.complete' ORDER BY id DESC", watermark)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, id)
	}
	actual := []int64{}
	for _, event := range got.Items {
		actual = append(actual, event.ID)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatal("snapshot has duplicates, missing matches or unstable order")
	}
	filter.Page = 0
	filter.PageSize = 0
	if !reflect.DeepEqual(got.Manifest.Filters, filter) || got.Manifest.From != "2026-10-27T12:00:01Z" || got.Manifest.To != got.Items[0].CreatedAt {
		t.Fatalf("wrong manifest: %+v", got.Manifest)
	}
	// A created_at ordering request uses ID as a stable tie breaker.
	file2, err := svc.PrepareExport(context.Background(), ListFilter{Order: ListOrderCreatedAt}, "json")
	if err != nil {
		t.Fatal(err)
	}
	defer file2.Close()
	if err = json.NewDecoder(file2).Decode(&got); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(got.Items); i++ {
		a, b := got.Items[i-1], got.Items[i]
		if a.CreatedAt < b.CreatedAt || (a.CreatedAt == b.CreatedAt && a.ID < b.ID) {
			t.Fatal("unstable timestamp order")
		}
	}
}
func TestAuditExportJSONFidelityCSVFormulaProtectionAndRedaction(t *testing.T) {
	db := exportTestDB(t)
	repo := NewSQLiteRepository(func() *sql.DB { return db })
	svc := NewService(ServiceOptions{Repository: repo})
	values := []string{"comma,quote\" Unicode é雪\r\nnext", "=1+1", "+SUM(A1)", "-1+2", "@SUM(A1)", " \t=1", "\ufeff@value", "\ttext", "\rtext"}
	for _, value := range values {
		seedExportEvent(t, repo, Event{CreatedAt: "2026-10-27T12:00:00Z", Actor: value, Action: value, TargetType: value, TargetName: value, Status: value, Message: value, RequestID: value, ClientIP: value, MetaJSON: `{"public":"雪","large":9007199254740993,"nested":{"password":"never-export-me","ok":true}}`})
	}
	file, err := svc.PrepareExport(context.Background(), ListFilter{}, "json")
	if err != nil {
		t.Fatal(err)
	}
	var got exportedAudit
	if err = json.NewDecoder(file).Decode(&got); err != nil {
		t.Fatal(err)
	}
	file.Close()
	for i, event := range got.Items {
		if event.Message != values[len(values)-1-i] || strings.Contains(event.MetaJSON, "never-export-me") || !strings.Contains(event.MetaJSON, "9007199254740993") {
			t.Fatal("JSON changed public values or leaked metadata")
		}
	}
	file, err = svc.PrepareExport(context.Background(), ListFilter{}, "csv")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\r\n") {
		t.Fatal("CSV lacks CRLF")
	}
	records, err := csv.NewReader(strings.NewReader(string(raw))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != len(values)+2 {
		t.Fatal("CSV row count")
	}
	for i, row := range records[1 : len(records)-1] {
		value := values[len(values)-1-i]
		want := value
		if i < len(values)-1 {
			want = "'" + want
		}
		want = strings.ReplaceAll(want, "\r\n", "\n")
		want = strings.ReplaceAll(want, "\r", "")
		if row[9] != want {
			t.Fatalf("CSV value %q want %q", row[9], want)
		}
	}
	var manifest ExportManifest
	if err = json.Unmarshal([]byte(records[len(records)-1][1]), &manifest); err != nil || manifest.Count != len(values) || manifest.Version != 1 {
		t.Fatal("CSV manifest absent")
	}
}
func TestAuditExportEmptyLimitsDeadlineCancellationCleanup(t *testing.T) {
	db := exportTestDB(t)
	repo := NewSQLiteRepository(func() *sql.DB { return db })
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	for _, format := range []string{"json", "csv"} {
		file, err := repo.prepareExport(context.Background(), ListFilter{}, format, 3, 10000)
		if err != nil {
			t.Fatal(err)
		}
		stat, _ := file.Stat()
		if stat.Mode().Perm() != 0600 {
			t.Fatal("spool is not private")
		}
		name := file.Name()
		if file.Count != 0 {
			t.Fatal("empty search not empty")
		}
		if err = file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(name); !os.IsNotExist(err) {
			t.Fatal("spool retained")
		}
	}
	for i := 0; i < 4; i++ {
		seedExportEvent(t, repo, Event{MetaJSON: "{}"})
	}
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		rows  int
		bytes int64
		want  error
	}{
		{"rows", context.Background(), 3, 10000, ErrExportLimit},
		{"bytes", context.Background(), 5, 20, ErrExportLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := repo.prepareExport(tc.ctx, ListFilter{}, "json", tc.rows, tc.bytes)
			if file != nil || !errors.Is(err, tc.want) {
				t.Fatalf("got file=%v err=%v", file, err)
			}
		})
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if file, err := repo.prepareExport(cancelled, ListFilter{}, "json", 10, 10000); file != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if file, err := repo.prepareExport(expired, ListFilter{}, "json", 10, 10000); file != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("budget = %v", err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Fatal("failed exports left temporary files")
	}
	if db.Stats().InUse != 0 {
		t.Fatal("cursor/transaction leaked")
	}
}
func TestAuditExportBoundedWriterRejectsBeforeWritingOverLimit(t *testing.T) {
	var out strings.Builder
	w := boundedExportWriter{ctx: context.Background(), out: &out, remaining: 4}
	if _, err := w.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("5")); !errors.Is(err, ErrExportLimit) || out.String() != "1234" {
		t.Fatal("unbounded export write")
	}
}

func TestAuditExportCancellationDuringScanClosesSnapshotAndSpool(t *testing.T) {
	db := exportTestDB(t)
	repo := NewSQLiteRepository(func() *sql.DB { return db })
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		_, err = tx.Exec(`INSERT INTO audit_events(created_at,actor,action,target_type,target_name,status,message,meta_json,request_id,client_ip) VALUES('2026-10-27T00:00:00Z','a','a','server','a','failure',?,'{}','','')`, strings.Repeat("x", 2000))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		file, err := repo.prepareExport(ctx, ListFilter{}, "json", ExportMaxRows, ExportMaxBytes)
		if file != nil {
			file.Close()
		}
		finished <- err
	}()
	deadline := time.Now().Add(time.Second)
	for db.Stats().InUse == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if db.Stats().InUse == 0 {
		t.Fatal("export never opened its snapshot")
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("scan cancellation=%v", err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 || db.Stats().InUse != 0 {
		t.Fatal("mid-scan cancellation retained cursor, transaction or spool")
	}
}

func TestAuditExportLargeResultKeepsHeapBounded(t *testing.T) {
	db := exportTestDB(t)
	repo := NewSQLiteRepository(func() *sql.DB { return db })
	_, err := db.Exec(`WITH RECURSIVE seq(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM seq WHERE x<10000) INSERT INTO audit_events(created_at,actor,action,target_type,target_name,status,message,meta_json,request_id,client_ip) SELECT '2026-10-27T00:00:00Z','a','a','server','a','success',?,'{}','','' FROM seq`, strings.Repeat("x", 2500))
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	done := make(chan struct{})
	peakResult := make(chan uint64, 1)
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		peak := before.HeapAlloc
		for {
			select {
			case <-ticker.C:
				var sample runtime.MemStats
				runtime.ReadMemStats(&sample)
				if sample.HeapAlloc > peak {
					peak = sample.HeapAlloc
				}
			case <-done:
				peakResult <- peak
				return
			}
		}
	}()
	file, err := repo.prepareExport(context.Background(), ListFilter{}, "json", ExportMaxRows, ExportMaxBytes)
	close(done)
	peak := <-peakResult
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if file.Count != 10000 || stat.Size() < 25_000_000 || stat.Size() > ExportMaxBytes {
		t.Fatalf("unexpected spool count/size: %d/%d", file.Count, stat.Size())
	}
	growth := uint64(0)
	if peak > before.HeapAlloc {
		growth = peak - before.HeapAlloc
	}
	t.Logf("10000 events; spool=%d bytes; sampled peak Go heap growth=%d bytes", stat.Size(), growth)
	if growth > 32<<20 {
		t.Fatalf("result-sized memory retained: sampled heap growth %d", growth)
	}
}
