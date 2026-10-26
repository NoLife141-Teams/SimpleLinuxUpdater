// Package dashboardfixture supplies disposable, deterministic scaling fixtures.
// It is used by tests and measurement tools only; it never starts runners.
package dashboardfixture

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/servers"
)

const Seed = 475

var Now = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

type Manifest struct {
	Version                 int    `json:"version"`
	Seed                    int    `json:"seed"`
	Clock                   string `json:"clock"`
	Servers                 int    `json:"servers"`
	Active                  int    `json:"active"`
	Approvals               int    `json:"approvals"`
	Terminal                int    `json:"terminal"`
	PackageHosts            int    `json:"package_hosts"`
	PackagesPerHost         int    `json:"packages_per_host"`
	PreviewBytesPerHost     int    `json:"preview_bytes_per_host"`
	CommandsPerHost         int    `json:"commands_per_host"`
	UpdatesPerHost          int    `json:"updates_per_host"`
	DetailedLogBytesPerHost int    `json:"detailed_log_bytes_per_host"`
	Policies                int    `json:"policies"`
	TrustedHosts            int    `json:"trusted_hosts"`
	SHA256                  string `json:"sha256"`
}

type Dataset struct {
	Manifest Manifest                         `json:"manifest"`
	Servers  []servers.Server                 `json:"servers"`
	Statuses map[string]*servers.ServerStatus `json:"statuses"`
}

// Generate uses index arithmetic (no global PRNG) and a fixed UTC clock. All
// endpoints use reserved .invalid names and an explicitly synthetic password.
// Neither these fixtures nor their callers may start host maintenance runners.
func Generate(n int) Dataset {
	d := Dataset{Manifest: Manifest{Version: 2, Seed: Seed, Clock: Now.Format(time.RFC3339), Servers: n, PackagesPerHost: 20, PreviewBytesPerHost: 4096, CommandsPerHost: 10, UpdatesPerHost: 4, DetailedLogBytesPerHost: 32768, Policies: 3}, Statuses: make(map[string]*servers.ServerStatus, n)}
	for i := 0; i < n; i++ {
		if i%5 != 4 {
			d.Manifest.TrustedHosts++
		}
		name := fmt.Sprintf("synthetic-%04d", i)
		server := servers.Server{Name: name, Host: name + ".invalid", Port: 22, User: "synthetic", Pass: "synthetic-only", Tags: []string{fmt.Sprintf("group-%d", (i+Seed)%5), "synthetic"}}
		s := &servers.ServerStatus{Name: name, Host: server.Host, Port: 22, User: server.User, Tags: server.Tags, Status: "done", JobID: "job-" + name, Logs: strings.Repeat("Synthetic maintenance output.\n", 142)[:4096]}
		switch i % 10 {
		case 0:
			s.Status = "updating"
			d.Manifest.Active++
		case 1, 2:
			s.Status = "pending_approval"
			s.ApprovalGeneration = 1
			d.Manifest.Approvals++
		default:
			d.Manifest.Terminal++
		}
		if i%2 == 0 || i%10 == 1 {
			d.Manifest.PackageHosts++
			for j := 0; j < 20; j++ {
				p := fmt.Sprintf("synthetic-pkg-%02d", j)
				s.Upgradable = append(s.Upgradable, p)
				s.PendingUpdates = append(s.PendingUpdates, servers.PendingUpdate{Package: p, CurrentVersion: "1.0", CandidateVersion: "1.1", Security: j%4 == 0, Raw: p + "/stable 1.1 amd64 [upgradable from: 1.0]", CVEState: "complete", CVEs: []string{}})
			}
			s.UpgradePlan.StandardPackageCount = 20
			s.UpgradePlan.StandardSecurityCount = 5
			s.UpgradePlan.TotalSecurityCount = 5
		}
		d.Servers = append(d.Servers, server)
		d.Statuses[name] = s
	}
	// Include the complete data recipe (including history and log dimensions) in
	// the digest. DB row IDs, auth randomness and filesystem paths are excluded.
	raw, _ := json.Marshal(d)
	hash := sha256.Sum256(raw)
	d.Manifest.SHA256 = hex.EncodeToString(hash[:])
	return d
}

// SeedDB requires the real application's schema. Rows are inserted in one
// transaction; no app scheduler, worker, inventory command or SSH is invoked.
func (d Dataset) SeedDB(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for i, s := range d.Servers {
		if _, err = tx.Exec(`INSERT INTO servers(name,host,port,user,pass_enc,key_enc,tags,disabled) VALUES(?,?,?,?,?,?,?,0)`, s.Name, s.Host, s.Port, s.User, s.Pass, "", strings.Join(s.Tags, ",")); err != nil {
			return err
		}
		at := Now.Add(-time.Hour).Format(time.RFC3339)
		if _, err = tx.Exec(`INSERT INTO server_facts(server_name,collected_at,os_pretty_name,disk_status,disk_free_kb,disk_total_kb,apt_status,reboot_required) VALUES(?,?,'Synthetic Linux','ok',10000000,20000000,'ok',0)`, s.Name, at); err != nil {
			return err
		}
		st := d.Statuses[s.Name]
		status, phase := jobs.StatusSucceeded, jobs.PhaseComplete
		if st.Status == "updating" {
			status, phase = jobs.StatusRunning, jobs.PhaseAptUpgrade
		}
		if st.Status == "pending_approval" {
			status, phase = jobs.StatusWaitingApproval, jobs.PhaseApprovalWait
		}
		if _, err = tx.Exec(`INSERT INTO jobs(id,kind,server_name,actor,status,phase,summary,logs_text,created_at,updated_at,log_next_sequence,log_source_bytes) VALUES(?,'update',?,'synthetic',?,?,'Synthetic job',?,?,?,2,32768)`, st.JobID, s.Name, status, phase, st.Logs, at, at); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO job_log_chunks(job_id,sequence,stream,data,created_at) VALUES(?,1,'stdout',?,?)`, st.JobID, []byte(strings.Repeat("D", d.Manifest.DetailedLogBytesPerHost)), at); err != nil {
			return err
		}
		for j := 0; j < d.Manifest.CommandsPerHost+d.Manifest.UpdatesPerHost; j++ {
			action := "server.update"
			meta := `{}`
			status := "success"
			if j >= d.Manifest.CommandsPerHost {
				action = "update.complete"
				meta = `{"duration_ms":1200,"packages":20}`
				if (i+j)%4 == 0 {
					status = "failure"
				}
			}
			at := Now.Add(-time.Duration(j+1) * time.Hour).Format(time.RFC3339)
			if _, err = tx.Exec(`INSERT INTO audit_events(created_at,actor,action,target_type,target_name,status,message,meta_json) VALUES(?,'synthetic',?,'server',?,?,'Synthetic history',?)`, at, action, s.Name, status, meta); err != nil {
				return err
			}
		}
	}
	for i := 0; i < d.Manifest.Policies; i++ {
		at := Now.Add(-24 * time.Hour).Format(time.RFC3339)
		if _, err = tx.Exec(`INSERT INTO update_policies(name,target_tag,include_tags_json,exclude_tags_json,target_servers_json,package_scope,execution_mode,cadence_kind,time_local,created_at,updated_at) VALUES(?,?,'[]','[]','[]','full','scan_only','daily','23:00',?,?)`, fmt.Sprintf("Synthetic policy %d", i), fmt.Sprintf("group-%d", i), at, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}
