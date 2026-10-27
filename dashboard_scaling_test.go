package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	apptimepkg "debian-updater/internal/apptime"
	authpkg "debian-updater/internal/auth"
	"debian-updater/internal/events"
	observabilitypkg "debian-updater/internal/observability"
	serverpkg "debian-updater/internal/servers"
	"debian-updater/internal/testutil/dashboardfixture"
	"debian-updater/internal/vulnerabilities"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh"
)

type scalingApp struct {
	deps    AppDeps
	handler http.Handler
	data    dashboardfixture.Dataset
	meter   *dashboardfixture.SQLMeter
	cookie  *http.Cookie
	viewer  *http.Cookie
	token   string
}

// This is a test binary composition, never a production runtime flag. In
// particular, main(), the scheduler and host-facts refresh worker never run.
func newScalingApp(t testing.TB, n int) *scalingApp {
	return newScalingAppMode(t, n, true)
}

func newScalingAppMode(t testing.TB, n int, metered bool) *scalingApp {
	return newScalingRuntime(t, n, metered, true)
}
func newScalingRuntime(t testing.TB, n int, metered, bootstrap bool) *scalingApp {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	path := filepath.Join(dir, "scaling.db")
	known := filepath.Join(dir, "known_hosts")
	t.Setenv("DEBIAN_UPDATER_KNOWN_HOSTS", known)
	t.Setenv("DEBIAN_UPDATER_DB_PATH", path)
	db, meter, err := dashboardfixture.Open(path)
	if !metered && err == nil {
		_ = db.Close()
		db, err = sql.Open("sqlite", path)
		if err == nil {
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)
		}
		meter = nil
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL", "PRAGMA busy_timeout=5000"} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err = ensureSchema(db); err != nil {
		t.Fatal(err)
	}
	d := dashboardfixture.Generate(n)
	key, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(make([]byte, 32)).Public())
	if err != nil {
		t.Fatal(err)
	}
	var hosts strings.Builder
	for i, s := range d.Servers {
		if i%5 == 4 {
			continue
		}
		fmt.Fprintf(&hosts, "%s %s", s.Host, ssh.MarshalAuthorizedKey(key))
	}
	if err = os.WriteFile(known, []byte(hosts.String()), 0600); err != nil {
		t.Fatal(err)
	}
	state := newServerState()
	inventory := serverpkg.NewService(serverpkg.ServiceDeps{State: state, Repository: serverpkg.SQLiteRepository{DB: func() *sql.DB { return db }}, KnownHosts: appKnownHostsDeps(func() string { return path })})
	// No-op setters keep this independent from process-global runtime pointers.
	deps := AppDeps{DB: func() *sql.DB { return db }, DBPath: func() string { return path }, ServerState: state, ServerInventoryService: inventory, Now: func() time.Time { return dashboardfixture.Now }, ApplicationTime: apptimepkg.New(apptimepkg.Deps{Store: apptimepkg.NewMemoryStore("UTC")}), SetCurrentJobManager: func(*JobManager) {}, SetSessionManager: func(*scs.SessionManager) {}, StartJobRunner: func(string, func(), ...func()) { t.Error("scaling fixture attempted runner dispatch") }, StartScheduledRunReconciliation: func(int64, string) { t.Error("scaling fixture attempted reconciliation") }, LoginRateLimiter: NewAuthRateLimiter(authRateLimitWindow, authLoginRateLimitMaxAttempts), SetupRateLimiter: NewAuthRateLimiter(authRateLimitWindow, authSetupRateLimitMaxAttempts), PasswordChangeRateLimiter: NewAuthRateLimiter(authRateLimitWindow, authPasswordChangeMaxAttempts), MetricsRateLimiter: NewAuthRateLimiter(metricsRateLimitWindow, metricsRateLimitMaxAttempts), TrustedProxies: func() []string { return nil }}.withDefaults()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := closeNotificationDelivery(ctx, deps.NotificationService); err != nil {
			t.Error(err)
		}
	})
	for _, lim := range []*AuthRateLimiter{deps.LoginRateLimiter, deps.PasswordChangeRateLimiter, deps.SetupRateLimiter, deps.MetricsRateLimiter} {
		t.Cleanup(lim.Stop)
	}
	original := getDialSSHConnection()
	var dials atomic.Int64
	setDialSSHConnection(func(Server, *ssh.ClientConfig) (sshConnection, error) {
		dials.Add(1)
		return nil, fmt.Errorf("scaling fixture forbids SSH")
	})
	t.Cleanup(func() {
		setDialSSHConnection(original)
		if dials.Load() != 0 {
			t.Errorf("scaling fixture attempted %d SSH dials", dials.Load())
		}
	})
	router, err := setupRouterWithDeps(deps)
	if err != nil {
		t.Fatal(err)
	}
	a := &scalingApp{deps: deps, handler: deps.CurrentSessionManager().LoadAndSave(router), data: d, meter: meter}
	if cleanup, ok := deps.CurrentSessionManager().Store.(interface{ StopCleanup() }); ok {
		t.Cleanup(cleanup.StopCleanup)
	}
	if !bootstrap {
		return a
	}
	// Seed after initialization so runtime recovery cannot alter active fixtures.
	if err = d.SeedDB(db); err != nil {
		t.Fatal(err)
	}
	state.Lock()
	state.SetServers(d.Servers)
	state.SetStatusMap(d.Statuses)
	state.Unlock()
	login := func(endpoint, user string) *http.Cookie {
		req := httptest.NewRequest("POST", endpoint, strings.NewReader(fmt.Sprintf(`{"username":%q,"password":"SyntheticPass475!"}`, user)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://example.com")
		rec := httptest.NewRecorder()
		a.handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("scaling login: %d %s", rec.Code, rec.Body.String())
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == authpkg.DefaultSessionCookieName {
				return c
			}
		}
		t.Fatal("no scaling cookie")
		return nil
	}
	a.cookie = login("/api/auth/setup", "scaling-admin")
	if _, err = deps.AuthService.CreateAccount("scaling-viewer", "SyntheticPass475!", "reader"); err != nil {
		t.Fatal(err)
	}
	a.viewer = login("/api/auth/login", "scaling-viewer")
	admin, err := deps.AuthService.AccountByName("scaling-admin")
	if err != nil {
		t.Fatal(err)
	}
	_, a.token, err = deps.AuthService.CreatePersonalToken(admin.ID, "scaling read", []string{"operational:read"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (a *scalingApp) get(t testing.TB, path, credential string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	switch credential {
	case "viewer":
		req.AddCookie(a.viewer)
	case "pat":
		req.Header.Set("Authorization", "Bearer "+a.token)
	default:
		req.AddCookie(a.cookie)
	}
	// Separate fixture clients use their own IP, within the real 120/min limit.
	req.RemoteAddr = fmt.Sprintf("192.0.2.%d:475", a.data.Manifest.Servers/10+1)
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET %s (%s): %d %s", path, credential, rec.Code, rec.Body.String())
	}
	return rec
}

type scalingMeasurement struct {
	Name           string  `json:"name"`
	N              int     `json:"n"`
	P50MS          float64 `json:"p50_ms"`
	P95MS          float64 `json:"p95_ms"`
	Bytes          int     `json:"json_bytes,omitempty"`
	SQL            *int    `json:"sql_statements"`
	Allocs         uint64  `json:"allocs_per_op"`
	AllocatedBytes uint64  `json:"allocated_bytes_per_op"`
}

func measureScaling(t testing.TB, name string, n int, meter *dashboardfixture.SQLMeter, fn func() int) scalingMeasurement {
	t.Helper()
	fn()
	times := make([]float64, n)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if meter != nil {
		meter.Reset()
	}
	size := 0
	for i := range n {
		start := time.Now()
		size = fn()
		times[i] = float64(time.Since(start).Nanoseconds()) / 1e6
	}
	runtime.ReadMemStats(&after)
	var queries *int
	if meter != nil {
		count := meter.Count() / n
		queries = &count
	}
	sort.Float64s(times)
	return scalingMeasurement{Name: name, N: n, P50MS: times[(n-1)/2], P95MS: times[int(float64(n)*.95+.999)-1], Bytes: size, SQL: queries, Allocs: (after.Mallocs - before.Mallocs) / uint64(n), AllocatedBytes: (after.TotalAlloc - before.TotalAlloc) / uint64(n)}
}

func TestDashboardScalingBudgets(t *testing.T) {
	for _, size := range []int{10, 100, 1000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			a := newScalingApp(t, size)
			for _, tc := range []struct {
				path, credential                    string
				bytesPerHost, sqlMax, allocsPerHost int
			}{{"/api/servers", "admin", 9000, 4, 60}, {"/api/servers", "viewer", 4600, 4, 60}, {"/api/servers", "pat", 4600, 3, 60}, {"/api/dashboard/summary?include_command_history=false", "admin", 5200, 19, 400}, {"/api/dashboard/summary", "admin", 6800, 21, 550}} {
				a.meter.Reset()
				rec := a.get(t, tc.path, tc.credential)
				queries := a.meter.Count()
				if strings.HasPrefix(tc.path, "/api/dashboard/summary") {
					tc.sqlMax = 17 + 2*((size+499)/500)
					if strings.Contains(tc.path, "false") {
						tc.sqlMax = 17 + (size+499)/500
					}
				}
				if len(rec.Body.Bytes()) > tc.bytesPerHost*size+1024 {
					t.Errorf("payload budget %s: %d", tc.path, rec.Body.Len())
				}
				if queries > tc.sqlMax {
					t.Errorf("SQL budget %s: %d (%v)", tc.path, queries, a.meter.Snapshot())
				}
				allocs := testing.AllocsPerRun(2, func() { a.get(t, tc.path, tc.credential) })
				if allocs > float64(tc.allocsPerHost*size+2000) {
					t.Errorf("allocation budget %s: %.0f", tc.path, allocs)
				}
				if strings.Contains(rec.Body.String(), strings.Repeat("D", 100)) {
					t.Error("list contains detailed job log chunk")
				}
				if tc.path == "/api/servers" {
					var statuses []ServerStatus
					if err := json.Unmarshal(rec.Body.Bytes(), &statuses); err != nil {
						t.Fatal(err)
					}
					if len(statuses) != size {
						t.Fatal("truncated inventory")
					}
					for _, s := range statuses {
						if tc.credential != "admin" && s.Logs != "" {
							t.Fatal("logs permission leaked")
						}
						if len(s.Logs) > 4096 {
							t.Fatal("preview exceeded fixture bound")
						}
					}
					counts := map[string]int{}
					for _, s := range statuses {
						counts[s.Status]++
					}
					if counts["updating"] != a.data.Manifest.Active || counts["pending_approval"] != a.data.Manifest.Approvals || counts["done"] != a.data.Manifest.Terminal {
						t.Fatalf("inventory status distribution differs from canonical fixture: %v", counts)
					}
				} else {
					var summary dashboardSummaryResponse
					if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
						t.Fatal(err)
					}
					if len(summary.Servers) != size {
						t.Fatal("truncated dashboard")
					}
					if got := int(summary.Fleet["pending_approval"].(float64)); got != size/5 {
						t.Fatalf("fleet approvals %d", got)
					}
					if int(summary.Fleet["pending_packages"].(float64)) != size*12 {
						t.Fatal("global package count drifted")
					}
					if int(summary.Fleet["in_progress"].(float64)) != a.data.Manifest.Active || int(summary.Fleet["done"].(float64)) != a.data.Manifest.Terminal {
						t.Fatalf("inventory/dashboard state distribution differs: manifest=%+v fleet=%+v", a.data.Manifest, summary.Fleet)
					}
					for _, s := range summary.Servers {
						if s.Name == "synthetic-0001" && !s.Actions["approve_all"].Enabled {
							t.Fatal("eligible approval disabled")
						}
						if s.Name == "synthetic-0000" && s.Actions["update"].Enabled {
							t.Fatal("active server became update eligible")
						}
						want := 8
						if strings.Contains(tc.path, "false") {
							want = 0
						}
						if len(s.CommandHistory) != want {
							t.Fatalf("command detail count %d want %d", len(s.CommandHistory), want)
						}
					}
				}
				t.Logf("size=%d %s %s bytes=%d SQL=%d allocations=%.0f", size, tc.path, tc.credential, rec.Body.Len(), queries, allocs)
			}
		})
	}
}

// Opt-in measurements keep shared CI independent of absolute timing.
func TestDashboardScalingMeasurements(t *testing.T) {
	out := os.Getenv("SLU_SCALING_OUTPUT")
	if out == "" {
		t.Skip("set SLU_SCALING_OUTPUT for repeatable measurement")
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{10, 100, 1000} {
		for _, metered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/metered=%t", size, metered), func(t *testing.T) {
				a := newScalingAppMode(t, size, metered)
				results := []scalingMeasurement{}
				for _, tc := range []struct{ name, path, credential string }{{"auth-cookie", "/api/auth/status", "admin"}, {"inventory-cookie", "/api/servers", "admin"}, {"inventory-viewer", "/api/servers", "viewer"}, {"inventory-pat-read", "/api/servers", "pat"}, {"dashboard-cookie", "/api/dashboard/summary?include_command_history=false", "admin"}, {"dashboard-legacy", "/api/dashboard/summary", "admin"}, {"dashboard-pat-read", "/api/dashboard/summary?include_command_history=false", "pat"}} {
					results = append(results, measureScaling(t, tc.name, 15, a.meter, func() int { return a.get(t, tc.path, tc.credential).Body.Len() }))
				}
				results = append(results, measureScaling(t, "inventory-service", 15, a.meter, func() int { a.deps.ServerInventoryService.ListStatuses(); return 0 }))
				var summary dashboardSummaryResponse
				results = append(results, measureScaling(t, "dashboard-service", 15, a.meter, func() int {
					var err error
					summary, err = a.deps.ObservabilityService.BuildDashboardSummaryContextWithOptions(context.Background(), "24h", dashboardfixture.Now, observabilitypkg.DashboardSummaryOptions{OmitCommandHistory: true})
					if err != nil {
						t.Fatal(err)
					}
					return 0
				}))
				results = append(results, measureScaling(t, "dashboard-json", 15, a.meter, func() int {
					raw, err := json.Marshal(summary)
					if err != nil {
						t.Fatal(err)
					}
					return len(raw)
				}))
				runtime.GC()
				var mem runtime.MemStats
				runtime.ReadMemStats(&mem)
				report := map[string]any{"manifest": a.data.Manifest, "measurements": results, "heap_alloc_after_gc": mem.HeapAlloc, "go": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0), "goroutines": runtime.NumGoroutine(), "warmup": 1, "sql_instrumented": metered}
				writeScalingJSON(t, filepath.Join(out, fmt.Sprintf("go-%d-metered-%t.json", size, metered)), report)
			})
		}
	}
}
func writeScalingJSON(t testing.TB, path string, v any) {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkDashboardScaling(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			a := newScalingAppMode(b, n, false)
			for _, endpoint := range []struct{ name, path string }{{"InventoryHTTP", "/api/servers"}, {"DashboardHTTP", "/api/dashboard/summary?include_command_history=false"}} {
				b.Run(endpoint.name, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						a.get(b, endpoint.path, "admin")
					}
				})
			}
			b.Run("InventoryService", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					a.deps.ServerInventoryService.ListStatuses()
				}
			})
			b.Run("DashboardService", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					if _, err := a.deps.ObservabilityService.BuildDashboardSummaryContextWithOptions(context.Background(), "24h", dashboardfixture.Now, observabilitypkg.DashboardSummaryOptions{OmitCommandHistory: true}); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// Only the test binary exposes a disposable loopback fixture for Chromium. The
// stop request joins Serve and all cleanup hooks. No production endpoint exists.
func TestDashboardScalingServe(t *testing.T) {
	dir := os.Getenv("SLU_SCALING_SERVE_DIR")
	if dir == "" {
		t.Skip("browser measurement helper")
	}
	n, err := strconv.Atoi(os.Getenv("SLU_SCALING_SIZE"))
	if err != nil || n < 10 || n > 1000 {
		t.Fatal("invalid scaling size")
	}
	a := newScalingApp(t, n)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{}, 1)
	var streamMu sync.Mutex
	streams := map[int]context.CancelFunc{}
	streamID := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__scaling/disconnect" {
			streamMu.Lock()
			for _, cancel := range streams {
				cancel()
			}
			streamMu.Unlock()
			w.WriteHeader(204)
			return
		}
		if r.URL.Path == "/__scaling/stop" {
			stop <- struct{}{}
			w.WriteHeader(204)
			return
		}
		if r.URL.Path == "/__scaling/event" {
			a.deps.DashboardEventBroker.Publish("synthetic.refresh")
			w.WriteHeader(204)
			return
		}
		if r.URL.Path == "/api/dashboard/events" {
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			streamMu.Lock()
			streamID++
			id := streamID
			streams[id] = cancel
			streamMu.Unlock()
			defer func() { streamMu.Lock(); delete(streams, id); streamMu.Unlock() }()
			r = r.WithContext(ctx)
		}
		a.handler.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	writeScalingJSON(t, filepath.Join(dir, "server.json"), map[string]any{"url": "http://" + listener.Addr().String(), "cookie": a.cookie, "manifest": a.data.Manifest})
	select {
	case <-stop:
	case <-time.After(5 * time.Minute):
		t.Error("scaling browser did not stop fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
		t.Error(err)
	}
	if err := <-done; err != http.ErrServerClosed {
		t.Error(err)
	}
}

func TestDashboardScalingSSE(t *testing.T) {
	out := os.Getenv("SLU_SCALING_OUTPUT")
	if out == "" {
		t.Skip("set SLU_SCALING_OUTPUT for SSE cadence measurement")
	}
	for _, size := range []int{10, 100, 1000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			a := newScalingApp(t, size)
			srv := httptest.NewServer(a.handler)
			defer srv.Close()
			reports := []map[string]any{}
			for _, clients := range []int{1, 5} {
				times := []float64{}
				published, received := 0, 0
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				a.meter.Reset()
				g0 := runtime.NumGoroutine()
				for cycle := 0; cycle < 3; cycle++ {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					type stream struct {
						response *http.Response
						scanner  *bufio.Scanner
					}
					streams := []stream{}
					for range clients {
						req, _ := http.NewRequest("GET", srv.URL+"/api/dashboard/events", nil)
						req = req.WithContext(ctx)
						req.AddCookie(a.cookie)
						res, err := srv.Client().Do(req)
						if err != nil {
							t.Fatal(err)
						}
						if res.StatusCode != 200 {
							t.Fatal(res.Status)
						}
						scanner := bufio.NewScanner(res.Body)
						streams = append(streams, stream{res, scanner})
						readScalingEvent(t, scanner)
					}
					for event := 0; event < 10; event++ {
						time.Sleep(200 * time.Millisecond)
						start := time.Now()
						a.deps.DashboardEventBroker.PublishEvent(events.Event{Reason: "job.log", ServerName: "synthetic-0000", JobID: "job-synthetic-0000", Sequence: int64(event + 1), Stream: "stdout", Data: strings.Repeat("s", 256)})
						published++
						for _, s := range streams {
							readScalingEvent(t, s.scanner)
							received++
						}
						times = append(times, float64(time.Since(start).Nanoseconds())/1e6)
					}
					for _, s := range streams {
						_ = s.response.Body.Close()
					}
					cancel()
				}
				srv.CloseClientConnections()
				runtime.GC()
				runtime.ReadMemStats(&after)
				sort.Float64s(times)
				if received != published*clients {
					t.Fatal("SSE dropped events")
				}
				reports = append(reports, map[string]any{"clients": clients, "cycles": 3, "published": published, "received": received, "events_per_second": 5, "data_bytes_per_event": 256, "n": len(times), "fanout_p50_ms": times[14], "fanout_p95_ms": times[28], "allocated_bytes": after.TotalAlloc - before.TotalAlloc, "retained_heap_delta": int64(after.HeapAlloc) - int64(before.HeapAlloc), "goroutines_before": g0, "goroutines_after": runtime.NumGoroutine(), "sql_statements": a.meter.Count(), "errors": 0, "timeouts": 0})
			}
			writeScalingJSON(t, filepath.Join(out, fmt.Sprintf("sse-%d.json", size)), map[string]any{"manifest": a.data.Manifest, "measurements": reports})
		})
	}
}
func readScalingEvent(t *testing.T, s *bufio.Scanner) {
	t.Helper()
	found := false
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(line, "data:") {
			found = true
		}
		if line == "" && found {
			return
		}
	}
	t.Fatalf("SSE ended before event: %v", s.Err())
}

// Run the historical browser suite against the real router with empty owned
// persistence, normal setup/auth and no process startup workers or SSH transport.
func TestDashboardScalingE2EServe(t *testing.T) {
	addr := os.Getenv("SLU_SCALING_E2E_LISTEN")
	if addr == "" {
		t.Skip("isolated full-browser validation helper")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		t.Fatal("E2E helper requires explicit IPv4 loopback")
	}
	a := newScalingRuntime(t, 0, false, false)
	if phase := os.Getenv("SLU_E2E_VULNERABILITY_PHASE"); phase != "" {
		switch phase {
		case "inventory", "metadata", "evaluation", "persistence":
		default:
			t.Fatal("invalid isolated scan transport phase")
		}
		a.deps.VulnerabilityScans.deps.UpdateService = controlledInstalledScanService(phase, nil, nil, nil)
		if phase == "persistence" {
			repository := vulnerabilities.Repository{DB: a.deps.DB}
			a.deps.VulnerabilityScans.saveResult = func(ctx context.Context, name string, scan vulnerabilities.Scan, guard func(func() error) error) (vulnerabilities.Observation, error) {
				<-ctx.Done()
				return repository.SaveGuarded(ctx, name, scan, guard)
			}
		}
	}
	originalScan := scanHostKeyFunc
	scanHostKeyFunc = func(string, int) (ssh.PublicKey, error) {
		if os.Getenv("SLU_E2E_VULNERABILITY_PHASE") != "" {
			return ssh.NewPublicKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public())
		}
		return nil, fmt.Errorf("SSH host key scanning is unavailable in the isolated browser fixture")
	}
	t.Cleanup(func() { scanHostKeyFunc = originalScan })
	// Fault injection exists only in this disposable test binary. The normal
	// production router and its authentication/command handlers remain intact.
	productionHandler := a.handler
	a.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__test/expire-reauthentication" {
			productionHandler.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		cookie, err := r.Cookie(authpkg.DefaultSessionCookieName)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sm := a.deps.CurrentSessionManager()
		ctx, err := sm.Load(r.Context(), cookie.Value)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		sm.Put(ctx, "auth_reauthenticated_at", time.Now().Add(-6*time.Minute).Unix())
		if _, _, err := sm.Commit(ctx); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if ready := os.Getenv("SLU_SCALING_E2E_READY"); ready != "" {
		writeScalingJSON(t, ready, map[string]any{"pid": os.Getpid(), "listen": listener.Addr().String()})
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	srv := &http.Server{Handler: a.handler, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	<-ctx.Done()
	if os.Getenv("SLU_E2E_VULNERABILITY_PHASE") != "" {
		scans := a.deps.VulnerabilityScans
		scans.mu.Lock()
		for _, op := range scans.active {
			op.cancel()
		}
		scans.mu.Unlock()
		waitForUpdateRunners()
	}
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
	}
	if err := <-done; err != http.ErrServerClosed {
		t.Error(err)
	}
}
