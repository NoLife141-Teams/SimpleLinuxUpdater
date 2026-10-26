package policies

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/servers"
)

type schedule472Harness struct {
	repo      *SQLiteRepository
	db        *sql.DB
	service   *Service
	now       time.Time
	loc       *time.Location
	inventory []servers.Server
	requests  []ScheduledRunRequest
	status    string
}

func newSchedule472Harness(t *testing.T, raw string) *schedule472Harness {
	t.Helper()
	repo, db := newTestRepository(t)
	h := &schedule472Harness{repo: repo, db: db, now: must472Time(t, raw), loc: time.UTC, status: RunSucceeded, inventory: []servers.Server{{Name: "a", Tags: []string{"prod"}}, {Name: "b", Tags: []string{"prod"}}, {Name: "c", Tags: []string{"prod"}}}}
	repo.nowString = func() string { return h.now.UTC().Format(DefaultTimestampLayout) }
	h.restart()
	return h
}
func (h *schedule472Harness) restart() {
	h.service = NewService(ServiceDeps{Occurrences: h.repo, ListPolicies: h.repo.ListPolicies, LoadOverrides: h.repo.LoadAllOverrides, LoadGlobalBlackouts: h.repo.LoadGlobalBlackouts, ListRuns: h.repo.ListRuns, ListRolloutRuns: h.repo.ListRolloutRuns, ListRolloutOrigins: h.repo.ListRolloutOrigins, Now: func() time.Time { return h.now }, CurrentLocation: func() *time.Location { return h.loc }, SnapshotServers: func() []servers.Server { return h.inventory }, HandleScheduledRun: func(req ScheduledRunRequest) ScheduledRunResult {
		stage, wave := RunRolloutPosition(req.Policy, req.Server.Name)
		status := h.status
		if req.Outcome != "" {
			status = RunSkipped
		}
		run, inserted, err := h.repo.CreateRun(Run{PolicyID: req.Policy.ID, PolicyName: req.Policy.Name, PolicyRevision: req.Policy.Revision, RolloutStage: stage, Wave: wave, ServerName: req.Server.Name, ScheduledForUTC: req.ScheduledForUTC, Status: status, Reason: req.Outcome, FinishedAt: h.now.Format(DefaultTimestampLayout)})
		if inserted {
			h.requests = append(h.requests, req)
		}
		return ScheduledRunResult{Inserted: inserted, Handled: err == nil, RunID: run.ID, Status: run.Status, Err: err}
	}})
}
func must472Time(t *testing.T, raw string) time.Time {
	t.Helper()
	v, e := time.Parse(time.RFC3339, raw)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func (h *schedule472Harness) policy(t *testing.T, patch func(*Policy)) Policy {
	t.Helper()
	p := Policy{Name: "Schedule472", Enabled: true, TargetTag: "prod", ExecutionMode: ExecutionScanOnly, PackageScope: PackageScopeFull, CadenceKind: CadenceOnce, DateLocal: h.now.Add(time.Hour).Format("2006-01-02"), TimeLocal: h.now.Add(time.Hour).Format("15:04")}
	if patch != nil {
		patch(&p)
	}
	if e := h.service.NormalizePolicy(&p); e != nil {
		t.Fatal(e)
	}
	p, e := h.repo.CreatePolicy(p)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func (h *schedule472Harness) tick(t *testing.T, at time.Time) {
	t.Helper()
	h.now = at
	if e := h.service.ProcessDue(at); e != nil {
		t.Fatal(e)
	}
}
func (h *schedule472Harness) runs(t *testing.T) []Run {
	t.Helper()
	r, e := h.repo.ListRuns(200)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestMonthlyAndOnceCanonicalProjection472(t *testing.T) {
	for _, tc := range []struct {
		name, now, zone, date string
		day                   int
		last                  bool
		want                  string
	}{
		{name: "31 skips February", now: "2026-02-01T00:00:00Z", day: 31, want: "2026-03-31T03:00:00.000000000Z"},
		{name: "leap February", now: "2028-02-01T00:00:00Z", day: 29, want: "2028-02-29T03:00:00.000000000Z"},
		{name: "last February", now: "2026-02-01T00:00:00Z", last: true, want: "2026-02-28T03:00:00.000000000Z"},
		{name: "last leap February", now: "2028-02-01T00:00:00Z", last: true, want: "2028-02-29T03:00:00.000000000Z"},
		{name: "spring nonexistent skipped", now: "2026-03-01T00:00:00Z", zone: "America/New_York", day: 8, want: "2026-04-08T06:30:00.000000000Z"},
		{name: "fall earlier occurrence", now: "2026-10-31T00:00:00Z", zone: "America/New_York", date: "2026-11-01", want: "2026-11-01T05:30:00.000000000Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSchedule472Harness(t, tc.now)
			if tc.zone != "" {
				var e error
				h.loc, e = time.LoadLocation(tc.zone)
				if e != nil {
					t.Fatal(e)
				}
			}
			p := h.policy(t, func(p *Policy) {
				p.CadenceKind = CadenceMonthly
				p.MonthDay = tc.day
				p.MonthLastDay = tc.last
				p.TimeLocal = "03:00"
				if tc.zone != "" {
					p.TimeLocal = "02:30"
				}
				if tc.date != "" {
					p.CadenceKind = CadenceOnce
					p.DateLocal = tc.date
					p.TimeLocal = "01:30"
				}
			})
			preview, e := h.service.PreviewPolicy(p)
			if e != nil {
				t.Fatal(e)
			}
			if len(preview.UpcomingOccurrences) == 0 || preview.UpcomingOccurrences[0].ScheduledForUTC != tc.want {
				t.Fatalf("preview=%+v want %s", preview, tc.want)
			}
			projection, e := h.service.ProjectSchedule(ScheduleProjectionRequest{Now: h.now})
			if e != nil {
				t.Fatal(e)
			}
			if projection.Servers["a"].NextRun.ScheduledForUTC != tc.want {
				t.Fatalf("projection=%+v", projection)
			}
			at := must472Time(t, strings.Replace(tc.want, ".000000000", "", 1))
			h.tick(t, at)
			runs := h.runs(t)
			if len(runs) != 3 || runs[0].ScheduledForUTC != tc.want {
				t.Fatalf("runs=%+v", runs)
			}
			if tc.day == 31 {
				if !hasDiagnosticCode(preview.InformationalFacts, "month_day_missing") {
					t.Fatal("missing skipped month fact")
				}
				cal, e := h.service.Calendar(CalendarOptions{Start: must472Time(t, tc.now), Days: 28})
				if e != nil {
					t.Fatal(e)
				}
				if !reflect.DeepEqual(cal.Policies[0].Days[27].BlockedReasons, []string{"month_day_missing"}) {
					t.Fatalf("calendar=%+v", cal.Policies[0].Days[27])
				}
			}
		})
	}
}

func TestOnceConsumptionSurvivesRestartTimezoneAndEdits472(t *testing.T) {
	h := newSchedule472Harness(t, "2026-10-25T02:00:00Z")
	p := h.policy(t, nil)
	origin := h.now.Add(time.Hour)
	h.status = RunWaitingApproval
	h.tick(t, origin)
	if len(h.requests) != 3 {
		t.Fatal(h.requests)
	}
	h.restart()
	h.loc = time.FixedZone("-01:00", -3600)
	h.tick(t, origin.Add(time.Hour))
	p, e := h.repo.GetPolicy(p.ID)
	if e != nil || p.OnceConsumedAt == "" {
		t.Fatalf("policy=%+v err=%v", p, e)
	}
	for _, cadence := range []string{CadenceDaily, CadenceMonthly, CadenceOnce} {
		p.CadenceKind = cadence
		p.TimeLocal = "05:00"
		p.DateLocal = "2026-10-26"
		p.MonthDay = 26
		if e := h.service.NormalizePolicy(&p); e != nil {
			t.Fatal(e)
		}
		p, e = h.repo.UpdatePolicy(p.ID, p)
		if e != nil {
			t.Fatal(e)
		}
		h.tick(t, must472Time(t, "2026-10-26T06:00:00Z"))
		preview, e := h.service.PreviewPolicy(p)
		if e != nil {
			t.Fatal(e)
		}
		if len(preview.UpcomingOccurrences) != 0 || !hasDiagnosticCode(preview.InformationalFacts, "once_consumed") {
			t.Fatalf("consumed %s preview=%+v", cadence, preview)
		}
	}
	if len(h.requests) != 3 {
		t.Fatalf("replayed: %+v", h.requests)
	}
	if runs := h.runs(t); len(runs) != 3 || runs[0].Status != RunWaitingApproval {
		t.Fatal(runs)
	}
}

func TestOnceMissedTickRecoveryConsumesAndNeverLaunches472(t *testing.T) {
	h := newSchedule472Harness(t, "2026-10-25T02:00:00Z")
	p := h.policy(t, func(p *Policy) { p.RolloutMode = RolloutCanaryWaves; p.WaveSize = 1; p.WaveDelayMinutes = 5 })
	origin := h.now.Add(time.Hour)
	h.now = origin.Add(30 * time.Minute)
	store := SchedulerWatermarkStore{Load: func() (time.Time, bool, error) { return origin.Add(-time.Minute), true, nil }, Save: func(time.Time) error { return nil }}
	if e := h.service.ProcessDueWithRecovery(h.now, store); e != nil {
		t.Fatal(e)
	}
	h.restart()
	h.loc = time.FixedZone("-01:00", -3600)
	h.tick(t, origin.Add(time.Hour))
	for _, req := range h.requests {
		if req.Outcome == "" {
			t.Fatalf("missed work launched: %+v", req)
		}
	}
	if len(h.requests) != 3 {
		t.Fatalf("requests=%+v", h.requests)
	}
	saved, e := h.repo.GetPolicy(p.ID)
	if e != nil || saved.OnceConsumedAt == "" {
		t.Fatalf("not consumed: %+v %v", saved, e)
	}
}

func TestExplicitCanaryUnavailableNeverSubstitutes472(t *testing.T) {
	for _, reason := range []string{"missing", "renamed", "disabled", "excluded", "override"} {
		t.Run(reason, func(t *testing.T) {
			h := newSchedule472Harness(t, "2026-10-25T02:00:00Z")
			p := h.policy(t, func(p *Policy) {
				p.RolloutMode = RolloutCanaryWaves
				p.CanaryMode = CanaryExplicit
				p.CanaryServers = []string{"b"}
				p.WaveSize = 1
				p.WaveDelayMinutes = 1
				if reason == "excluded" {
					p.ExcludeTags = []string{"hold"}
				}
			})
			switch reason {
			case "missing":
				h.inventory = append(h.inventory[:1], h.inventory[2:]...)
			case "renamed":
				h.inventory[1].Name = "renamed-b"
			case "disabled":
				h.inventory[1].Disabled = true
			case "excluded":
				h.inventory[1].Tags = append(h.inventory[1].Tags, "hold")
			case "override":
				if _, e := h.repo.SetOverride(p.ID, "b", true); e != nil {
					t.Fatal(e)
				}
			}
			preview, e := h.service.PreviewPolicy(p)
			if e != nil {
				t.Fatal(e)
			}
			if !hasDiagnosticCode(preview.OperationalWarnings, RunReasonCanaryUnavailable) || preview.UpcomingOccurrences[0].AdmissionOutcome != RunReasonCanaryUnavailable {
				t.Fatal(preview)
			}
			origin := h.now.Add(time.Hour)
			h.tick(t, origin)
			h.tick(t, origin.Add(10*time.Minute))
			if len(h.requests) == 0 {
				t.Fatal("no investigable skip")
			}
			for _, req := range h.requests {
				if req.Outcome == "" {
					t.Fatalf("substitution launched: %+v", req)
				}
			}
		})
	}
}

func TestAcceptedRolloutFreezesRevisionMembersAndLiveGuards472(t *testing.T) {
	for _, guard := range []string{"edit", "deleted", "disabled", "override", "blackout", "failure", "skipped", "postcheck"} {
		t.Run(guard, func(t *testing.T) {
			h := newSchedule472Harness(t, "2026-10-25T02:00:00Z")
			p := h.policy(t, func(p *Policy) {
				p.RolloutMode = RolloutCanaryWaves
				p.CanaryMode = CanaryExplicit
				p.CanaryServers = []string{"c"}
				p.WaveSize = 1
				p.WaveDelayMinutes = 5
			})
			origin := h.now.Add(time.Hour)
			h.tick(t, origin)
			if len(h.requests) != 1 || h.requests[0].Server.Name != "c" {
				t.Fatal(h.requests)
			}
			h.inventory = append(h.inventory, servers.Server{Name: "new", Tags: []string{"prod"}})
			if guard == "deleted" {
				h.inventory = h.inventory[1:]
			}
			if guard == "disabled" {
				h.inventory[0].Disabled = true
			}
			if guard == "override" {
				if _, e := h.repo.SetOverride(p.ID, "a", true); e != nil {
					t.Fatal(e)
				}
			}
			if guard == "blackout" {
				if _, e := h.repo.SaveGlobalBlackouts([]BlackoutWindow{{Weekdays: []string{"sun"}, StartTime: "03:00", EndTime: "04:00"}}); e != nil {
					t.Fatal(e)
				}
			}
			if guard == "failure" || guard == "skipped" || guard == "postcheck" {
				r := h.runs(t)[0]
				status := RunFailed
				if guard == "skipped" {
					status = RunSkipped
				}
				reason := "postcheck_failed"
				if e := h.repo.UpdateRun(r.ID, RunUpdate{Status: &status, Reason: &reason}); e != nil {
					t.Fatal(e)
				}
			}
			p.Name = "changed"
			p.TargetTag = "other"
			p.RolloutMode = RolloutImmediate
			p.ExecutionMode = ExecutionAutoApply
			p.TimeLocal = "06:00"
			if _, e := h.repo.UpdatePolicy(p.ID, p); e != nil {
				t.Fatal(e)
			}
			h.restart()
			h.tick(t, origin.Add(5*time.Minute))
			h.tick(t, origin.Add(10*time.Minute))
			for _, req := range h.requests {
				if req.Server.Name == "new" || req.Policy.Name != "Schedule472" || req.Policy.Revision != 1 || req.Policy.ExecutionMode != ExecutionScanOnly {
					t.Fatalf("changed accepted rollout: %+v", req)
				}
			}
			if len(h.requests) != 3 {
				t.Fatalf("requests=%+v", h.requests)
			}
			for _, req := range h.requests[1:] {
				if guard != "edit" && req.Outcome == "" {
					t.Fatalf("guard %s bypassed: %+v", guard, req)
				}
			}
			runs := h.runs(t)
			for _, r := range runs {
				if r.PolicyRevision != 1 || r.RolloutStage == "" {
					t.Fatalf("missing accepted metadata: %+v", r)
				}
			}
		})
	}
}

func TestMigratedActiveRolloutFreezesOnFirstObservation472(t *testing.T) {
	h := newSchedule472Harness(t, "2026-10-25T02:00:00Z")
	p := h.policy(t, func(p *Policy) {
		p.CadenceKind = CadenceDaily
		p.RolloutMode = RolloutCanaryWaves
		p.WaveSize = 1
		p.WaveDelayMinutes = 5
	})
	origin := h.now.Add(time.Hour)
	if _, _, e := h.repo.CreateRun(Run{PolicyID: p.ID, ServerName: "a", ScheduledForUTC: origin.Format(DefaultTimestampLayout), Status: RunSucceeded}); e != nil {
		t.Fatal(e)
	}
	h.tick(t, origin.Add(time.Minute))
	open, e := h.repo.ListOpenOccurrences()
	if e != nil || len(open) != 1 {
		t.Fatalf("migration manifest=%+v %v", open, e)
	}
	h.inventory = append(h.inventory, servers.Server{Name: "new", Tags: []string{"prod"}})
	p.Name = "edited"
	p.TargetTag = "changed"
	p.TimeLocal = "06:00"
	if _, e := h.repo.UpdatePolicy(p.ID, p); e != nil {
		t.Fatal(e)
	}
	h.restart()
	h.tick(t, origin.Add(5*time.Minute))
	h.tick(t, origin.Add(10*time.Minute))
	if len(h.requests) != 2 {
		t.Fatalf("requests=%+v", h.requests)
	}
	for _, req := range h.requests {
		if req.Server.Name == "new" || req.Policy.Revision != 1 {
			t.Fatal(req)
		}
	}
}

func TestOccurrenceAdmissionFailureDoesNotDispatch472(t *testing.T) {
	h := newSchedule472Harness(t, "2026-10-25T02:00:00Z")
	h.policy(t, nil)
	if _, e := h.db.Exec(`DROP TABLE update_policy_occurrences`); e != nil {
		t.Fatal(e)
	}
	if e := h.service.ProcessDue(h.now.Add(time.Hour)); e == nil {
		t.Fatal("expected persistence error")
	}
	if len(h.requests) != 0 {
		t.Fatal(h.requests)
	}
}

func TestSchedule472Validation(t *testing.T) {
	h := newSchedule472Harness(t, "2026-10-25T02:00:00Z")
	for _, patch := range []func(*Policy){func(p *Policy) { p.DateLocal = "2026-02-30" }, func(p *Policy) { p.CadenceKind = CadenceMonthly; p.MonthDay = 32 }, func(p *Policy) { p.CadenceKind = CadenceMonthly; p.MonthDay = 0 }, func(p *Policy) { p.CanaryMode = CanaryExplicit }, func(p *Policy) {
		p.RolloutMode = RolloutCanaryWaves
		p.CanaryMode = CanaryExplicit
		p.CanaryServers = nil
	}} {
		p := Policy{Name: "invalid", Enabled: true, TargetTag: "prod", PackageScope: PackageScopeFull, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceOnce, DateLocal: "2026-10-25", TimeLocal: "03:00"}
		patch(&p)
		if e := h.service.NormalizePolicy(&p); e == nil {
			t.Fatalf("accepted invalid=%+v", p)
		}
	}
	if _, e := h.repo.AcceptOccurrence(Occurrence{}); e == nil {
		t.Fatal("empty admission accepted")
	}
}

func TestNewCadencesKeepBlackoutsConflictsAndPriority472(t *testing.T) {
	for _, cadence := range []string{CadenceOnce, CadenceMonthly} {
		t.Run(cadence, func(t *testing.T) {
			h := newSchedule472Harness(t, "2026-10-25T02:00:00Z")
			draft := h.policy(t, func(p *Policy) {
				p.Name = "draft"
				p.CadenceKind = cadence
				p.MonthDay = 25
				p.ExecutionMode = ExecutionScanOnly
			})
			winner := h.policy(t, func(p *Policy) {
				p.Name = "winner"
				p.CadenceKind = cadence
				p.MonthDay = 25
				p.ExecutionMode = ExecutionApprovalRequired
			})
			preview, e := h.service.PreviewPolicy(draft)
			if e != nil {
				t.Fatal(e)
			}
			if len(preview.ScheduleConflicts) != 1 || !preview.ScheduleConflicts[0].OccurrenceWindows[0].Effective {
				t.Fatal(preview)
			}
			origin := h.now.Add(time.Hour)
			h.tick(t, origin)
			for _, req := range h.requests {
				if req.Policy.ID == draft.ID && req.Outcome != RunReasonSuperseded {
					t.Fatalf("priority bypassed: %+v", req)
				}
			}
			if len(h.requests) != 6 {
				t.Fatal(h.requests)
			}
			if _, e := h.repo.SaveGlobalBlackouts([]BlackoutWindow{{Weekdays: []string{"sun"}, StartTime: "02:30", EndTime: "04:00"}}); e != nil {
				t.Fatal(e)
			}
			h.now = origin.AddDate(0, 0, 7)
			blocked := h.policy(t, func(p *Policy) {
				p.Name = "blocked"
				p.CadenceKind = cadence
				p.MonthDay = h.now.Day()
				p.TimeLocal = "03:00"
				p.DateLocal = h.now.Format("2006-01-02")
			})
			h.now = h.now.Add(-time.Minute)
			preview, e = h.service.PreviewPolicy(blocked)
			if e != nil {
				t.Fatal(e)
			}
			if len(preview.UpcomingOccurrences) == 0 || preview.UpcomingOccurrences[0].AdmissionOutcome != PreviewAdmissionBlockedNoRun {
				t.Fatal(preview)
			}
			h.tick(t, h.now.Add(time.Minute))
			for _, req := range h.requests {
				if req.Policy.ID == blocked.ID && req.Outcome != RunReasonBlackout {
					t.Fatalf("blackout bypassed: %+v", req)
				}
			}
			_ = winner
		})
	}
}

func TestOccurrencePartialPersistenceRecoveryCannotLaunchLateCanary472(t *testing.T) {
	h := newSchedule472Harness(t, "2026-10-25T02:00:00Z")
	p := h.policy(t, func(p *Policy) {
		p.RolloutMode = RolloutCanaryWaves
		p.CanaryMode = CanaryExplicit
		p.CanaryServers = []string{"a", "b"}
		p.WaveSize = 1
		p.WaveDelayMinutes = 1
	})
	origin := h.now.Add(time.Hour)
	o := newOccurrence(h.service, p, origin, h.inventory, nil)
	if _, e := h.repo.AcceptOccurrence(o); e != nil {
		t.Fatal(e)
	}
	// Simulate a crash after admission and one persisted canary, before its peer.
	if _, _, e := h.repo.CreateRun(Run{PolicyID: p.ID, ServerName: "a", ScheduledForUTC: o.ScheduledForUTC, Status: RunSucceeded}); e != nil {
		t.Fatal(e)
	}
	h.restart()
	h.tick(t, origin.Add(5*time.Minute))
	h.tick(t, origin.Add(6*time.Minute))
	if len(h.requests) != 2 {
		t.Fatal(h.requests)
	}
	for _, req := range h.requests {
		if req.Outcome == "" {
			t.Fatalf("late canary or wave launched: %+v", req)
		}
	}
}

func TestFutureExplicitCanaryRenameFollowsIdentityButNotAcceptedMembers472(t *testing.T) {
	h := newSchedule472Harness(t, "2026-10-25T02:00:00Z")
	p := h.policy(t, func(p *Policy) {
		p.RolloutMode = RolloutCanaryWaves
		p.CanaryMode = CanaryExplicit
		p.CanaryServers = []string{"b"}
		p.TargetServers = []string{"b"}
	})
	origin := h.now.Add(time.Hour)
	h.tick(t, origin)
	tx, e := h.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	if e := h.repo.RenameTargetServersTx(tx, "b", "renamed-b"); e != nil {
		t.Fatal(e)
	}
	if e := tx.Commit(); e != nil {
		t.Fatal(e)
	}
	saved, e := h.repo.GetPolicy(p.ID)
	if e != nil || !reflect.DeepEqual(saved.CanaryServers, []string{"renamed-b"}) || saved.Revision != 2 {
		t.Fatalf("renamed=%+v %v", saved, e)
	}
	open, e := h.repo.ListOpenOccurrences()
	if e != nil || len(open) != 1 || !reflect.DeepEqual(open[0].Policy.CanaryServers, []string{"b"}) {
		t.Fatalf("rewrote accepted names=%+v %v", open, e)
	}
}

func TestSchemaMigrationKeepsDailyWeeklyAndAutomaticCanaries472(t *testing.T) {
	repo, db := newTestRepository(t)
	// Recreate the policy table exactly without the added schedule columns.
	if _, e := db.Exec(`DROP TABLE update_policies;
 CREATE TABLE update_policies (id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,target_tag TEXT NOT NULL,include_tags_json TEXT NOT NULL DEFAULT '[]',exclude_tags_json TEXT NOT NULL DEFAULT '[]',target_servers_json TEXT NOT NULL DEFAULT '[]',package_scope TEXT NOT NULL,upgrade_mode TEXT NOT NULL DEFAULT 'standard',execution_mode TEXT NOT NULL,cadence_kind TEXT NOT NULL,time_local TEXT NOT NULL,weekdays_json TEXT NOT NULL DEFAULT '[]',approval_timeout_minutes INTEGER NOT NULL DEFAULT 720,rollout_mode TEXT NOT NULL DEFAULT 'immediate',canary_count INTEGER NOT NULL DEFAULT 0,wave_size INTEGER NOT NULL DEFAULT 0,wave_delay_minutes INTEGER NOT NULL DEFAULT 0,policy_blackouts_json TEXT NOT NULL DEFAULT '[]',created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
 INSERT INTO update_policies(name,target_tag,package_scope,execution_mode,cadence_kind,time_local,weekdays_json,rollout_mode,canary_count,wave_size,wave_delay_minutes,created_at,updated_at) VALUES ('daily','prod','security','scan_only','daily','03:00','[]','canary_waves',1,1,5,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),('weekly','prod','security','scan_only','weekly','03:00','["mon"]','canary_waves',1,1,5,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');`); e != nil {
		t.Fatal(e)
	}
	if e := EnsureSchema(db); e != nil {
		t.Fatal(e)
	}
	if e := EnsureSchema(db); e != nil {
		t.Fatal(e)
	}
	policies, e := repo.ListPolicies()
	if e != nil || len(policies) != 2 {
		t.Fatalf("policies=%+v %v", policies, e)
	}
	service := NewService(ServiceDeps{})
	for _, p := range policies {
		if p.CanaryMode != CanaryAutomatic || p.Revision != 1 || p.OnceConsumedAt != "" {
			t.Fatal(p)
		}
		if !service.PolicyDueAt(p, time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)) {
			t.Fatal(p)
		}
		batches := BuildRolloutBatches(p, []string{"c", "b", "a"})
		if !reflect.DeepEqual(batches[0].Servers, []string{"a"}) || len(batches) != 3 {
			t.Fatal(batches)
		}
	}
}

func TestOnceNoMatchingServersStillHasDurableInvestigableConsumption472(t *testing.T) {
	h := newSchedule472Harness(t, "2026-10-25T02:00:00Z")
	p := h.policy(t, nil)
	h.inventory = nil
	h.tick(t, h.now.Add(time.Hour))
	saved, e := h.repo.GetPolicy(p.ID)
	if e != nil || saved.OnceConsumedAt == "" || saved.OnceAdmissionReason != RunReasonNoMatch {
		t.Fatalf("no-match consumption=%+v %v", saved, e)
	}
	preview, e := h.service.PreviewPolicy(saved)
	if e != nil || !hasDiagnosticCode(preview.InformationalFacts, "once_admission_reason") {
		t.Fatalf("preview=%+v %v", preview, e)
	}
}
