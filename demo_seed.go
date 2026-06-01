package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

func seedVariantCDemoIfRequested(deps AppDeps) {
	if strings.TrimSpace(os.Getenv("DEBIAN_UPDATER_DEMO_SEED")) != "variant-c" {
		return
	}
	if !demoSeedResetEnabled(os.Getenv("DEBIAN_UPDATER_DEMO_RESET")) {
		log.Printf("variant c demo seed requested but DEBIAN_UPDATER_DEMO_RESET is not enabled; skipping destructive demo reset")
		return
	}
	if err := seedVariantCDemoRuntime(deps.withDefaults()); err != nil {
		log.Printf("failed to seed variant c demo runtime: %v", err)
		return
	}
	log.Printf("seeded variant c demo runtime")
}

func demoSeedResetEnabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "reset", "variant-c":
		return true
	default:
		return false
	}
}

func seedVariantCDemoRuntime(deps AppDeps) error {
	now := time.Now().UTC().Truncate(time.Second)
	demoServers := []Server{
		{Name: "edge-cache-03", Host: "edge-cache-03.example.test", Port: 22, User: "root", Pass: "demo-password", Tags: []string{"edge", "cache"}},
		{Name: "lab-node-05", Host: "lab-node-05.example.test", Port: 22, User: "root", Pass: "demo-password", Tags: []string{"lab"}},
		{Name: "prod-db-02", Host: "prod-db-02.example.test", Port: 22, User: "root", Pass: "demo-password", Tags: []string{"prod", "db"}},
		{Name: "prod-web-01", Host: "prod-web-01.example.test", Port: 22, User: "root", Pass: "demo-password", Tags: []string{"prod", "web"}},
		{Name: "worker-04", Host: "worker-04.example.test", Port: 22, User: "root", Pass: "demo-password", Key: "demo-key", Tags: []string{"batch"}},
	}
	demoStatuses := map[string]*ServerStatus{
		"edge-cache-03": newDemoStatus(demoServers[0], "pending_approval", "Waiting for security-only approval", []PendingUpdate{
			demoUpdate("openssl", true, "CVE-2026-4101"),
			demoUpdate("linux-image-generic", true),
			demoUpdate("curl", false),
		}),
		"lab-node-05": newDemoStatus(demoServers[1], "error", "Post-check failed after package update", []PendingUpdate{
			demoUpdate("python3-apt", false),
		}),
		"prod-db-02": newDemoStatus(demoServers[2], "updating", "Running apt update and package discovery", []PendingUpdate{
			demoUpdate("postgresql-16", true, "CVE-2026-2230"),
			demoUpdate("libpq5", true),
		}),
		"prod-web-01": newDemoStatus(demoServers[3], "pending_approval", "Waiting for approval: 4 packages, 3 CVE", []PendingUpdate{
			demoUpdate("nginx", true, "CVE-2026-1001", "CVE-2026-1002"),
			demoUpdate("openssl", true, "CVE-2026-4101"),
			demoUpdate("systemd", false),
			demoUpdate("tzdata", false),
		}),
		"worker-04": newDemoStatus(demoServers[4], "done", "Maintenance completed successfully", nil),
	}

	if deps.ServerState != nil {
		deps.ServerState.Lock()
		deps.ServerState.SetServers(demoServers)
		deps.ServerState.SetStatusMap(demoStatuses)
		deps.ServerState.Unlock()
	} else {
		mu.Lock()
		servers = demoServers
		statusMap = demoStatuses
		mu.Unlock()
	}

	if deps.ServerInventoryService != nil {
		if err := deps.ServerInventoryService.SaveWithTxHook(nil); err != nil {
			return fmt.Errorf("save demo servers: %w", err)
		}
	} else if err := saveServers(); err != nil {
		return fmt.Errorf("save demo servers: %w", err)
	}
	return seedVariantCDemoDatabase(deps.DB(), now)
}

func newDemoStatus(server Server, status, logs string, pending []PendingUpdate) *ServerStatus {
	upgradable := make([]string, 0, len(pending))
	for _, update := range pending {
		upgradable = append(upgradable, update.Package)
	}
	return &ServerStatus{
		Name:           server.Name,
		Host:           server.Host,
		Port:           normalizePort(server.Port),
		User:           server.User,
		Status:         status,
		Logs:           logs,
		Upgradable:     upgradable,
		PendingUpdates: clonePendingUpdates(pending),
		HasPassword:    server.Pass != "",
		HasKey:         server.Key != "",
		Tags:           append([]string(nil), server.Tags...),
	}
}

func demoUpdate(pkg string, security bool, cves ...string) PendingUpdate {
	source := "ubuntu"
	if security {
		source = "ubuntu-security"
	}
	return PendingUpdate{
		Package:          pkg,
		CurrentVersion:   "1.0.0",
		CandidateVersion: "1.0.1",
		Source:           source,
		Security:         security,
		CVEs:             append([]string(nil), cves...),
		CVEState:         "ready",
		Raw:              "Inst " + pkg,
	}
}

func seedVariantCDemoDatabase(db *sql.DB, now time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, stmt := range []string{
		"DELETE FROM jobs",
		"DELETE FROM audit_events",
		"DELETE FROM server_facts",
		"DELETE FROM update_policy_runs",
		"DELETE FROM update_policy_overrides",
		"DELETE FROM update_policies",
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}

	if err := insertDemoFacts(tx, now); err != nil {
		return err
	}
	if err := insertDemoJobs(tx, now); err != nil {
		return err
	}
	if err := insertDemoPolicy(tx, now); err != nil {
		return err
	}
	if err := insertDemoAudit(tx, now); err != nil {
		return err
	}
	return tx.Commit()
}

func insertDemoFacts(tx *sql.Tx, now time.Time) error {
	rows := []struct {
		name     string
		age      time.Duration
		reboot   any
		os       string
		apt      string
		diskFree int
		diskAll  int
	}{
		{"edge-cache-03", 72 * time.Hour, 0, "Ubuntu 24.04 LTS", "pending", 16 * 1024 * 1024, 64 * 1024 * 1024},
		{"lab-node-05", 48 * time.Hour, nil, "Debian 12", "warning", 6 * 1024 * 1024, 48 * 1024 * 1024},
		{"prod-db-02", 20 * time.Minute, 0, "Ubuntu 22.04 LTS", "updating", 42 * 1024 * 1024, 128 * 1024 * 1024},
		{"prod-web-01", 12 * time.Minute, 1, "Ubuntu 24.04 LTS", "pending", 28 * 1024 * 1024, 80 * 1024 * 1024},
		{"worker-04", 8 * time.Minute, 0, "Debian 12", "ok", 55 * 1024 * 1024, 96 * 1024 * 1024},
	}
	for _, row := range rows {
		if _, err := tx.Exec(`INSERT INTO server_facts
			(server_name, collected_at, os_pretty_name, uptime_seconds, disk_status, disk_free_kb, disk_total_kb, disk_details, apt_status, apt_details, reboot_required, raw_json)
			VALUES (?, ?, ?, ?, 'ok', ?, ?, '', ?, '', ?, '{}')`,
			row.name, formatJobTimestamp(now.Add(-row.age)), row.os, int64((7*24*time.Hour)/time.Second), row.diskFree, row.diskAll, row.apt, row.reboot); err != nil {
			return err
		}
	}
	return nil
}

func insertDemoJobs(tx *sql.Tx, now time.Time) error {
	type job struct {
		id, server, status, phase, summary, logs string
		started, finished                        time.Time
	}
	rows := []job{
		{"demo-edge-approval", "edge-cache-03", jobStatusWaitingApproval, jobPhaseApprovalWait, "Waiting for approval", "Waiting for security-only approval", now.Add(-35 * time.Minute), time.Time{}},
		{"demo-lab-failed", "lab-node-05", jobStatusFailed, jobPhasePostchecks, "Post-check failed after package update", "Post-check failed after package update", now.Add(-95 * time.Minute), now.Add(-82 * time.Minute)},
		{"demo-db-running", "prod-db-02", jobStatusRunning, jobPhaseAptUpdate, "Running apt update and package discovery", "Running apt update and package discovery", now.Add(-9 * time.Minute), time.Time{}},
		{"demo-web-approval", "prod-web-01", jobStatusWaitingApproval, jobPhaseApprovalWait, "Waiting for approval", "Waiting for approval: 4 packages, 3 CVE", now.Add(-22 * time.Minute), time.Time{}},
		{"demo-worker-done", "worker-04", jobStatusSucceeded, jobPhaseComplete, "Maintenance completed successfully", "Maintenance completed successfully", now.Add(-2 * time.Hour), now.Add(-110 * time.Minute)},
	}
	for _, row := range rows {
		finished := ""
		if !row.finished.IsZero() {
			finished = formatJobTimestamp(row.finished)
		}
		if _, err := tx.Exec(`INSERT INTO jobs
			(id, kind, server_name, actor, status, phase, summary, logs_text, created_at, updated_at, started_at, finished_at)
			VALUES (?, ?, ?, 'demo', ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.id, jobKindUpdate, row.server, row.status, row.phase, row.summary, row.logs,
			formatJobTimestamp(row.started.Add(-1*time.Minute)), formatJobTimestamp(now), formatJobTimestamp(row.started), finished); err != nil {
			return err
		}
	}
	return nil
}

func insertDemoPolicy(tx *sql.Tx, now time.Time) error {
	created := formatJobTimestamp(now.Add(-24 * time.Hour))
	if _, err := tx.Exec(`INSERT INTO update_policies
		(id, name, enabled, target_tag, include_tags_json, exclude_tags_json, target_servers_json, package_scope, execution_mode, cadence_kind, time_local, weekdays_json, approval_timeout_minutes, policy_blackouts_json, created_at, updated_at)
		VALUES (1, 'Nightly security', 1, 'batch', '[]', '[]', '["worker-04"]', 'security', 'approval_required', 'daily', '02:15', '[]', 720, '[]', ?, ?)`,
		created, formatJobTimestamp(now)); err != nil {
		return err
	}
	scheduled := time.Date(now.Year(), now.Month(), now.Day()+1, 2, 15, 0, 0, time.UTC)
	_, err := tx.Exec(`INSERT INTO update_policy_runs
		(policy_id, policy_name, server_name, scheduled_for_utc, execution_mode, package_scope, status, reason, summary, job_id, result_json, created_at, updated_at)
		VALUES (1, 'Nightly security', 'worker-04', ?, 'approval_required', 'security', 'scheduled', '', 'Nightly security window', '', '{}', ?, ?)`,
		formatJobTimestamp(scheduled), formatJobTimestamp(now), formatJobTimestamp(now))
	return err
}

func insertDemoAudit(tx *sql.Tx, now time.Time) error {
	rows := []struct {
		age    time.Duration
		action string
		target string
		status string
		msg    string
	}{
		{3 * time.Minute, "auth.login", "admin", "success", "demo login"},
		{24 * time.Minute, "update.scan", "prod-web-01", "success", "pending approvals discovered"},
		{82 * time.Minute, "update.run", "lab-node-05", "failure", "post-check failed"},
	}
	for _, row := range rows {
		if _, err := tx.Exec(`INSERT INTO audit_events
			(created_at, actor, action, target_type, target_name, status, message, meta_json, request_id, client_ip)
			VALUES (?, 'demo', ?, 'server', ?, ?, ?, '{}', '', '127.0.0.1')`,
			formatJobTimestamp(now.Add(-row.age)), row.action, row.target, row.status, row.msg); err != nil {
			return err
		}
	}
	return nil
}
