package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"
	"debian-updater/internal/observability"
	"debian-updater/internal/remediation"
	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"
	"debian-updater/internal/vulnerabilities"
	"github.com/gin-gonic/gin"
)

type remediationLifecycle struct {
	deps AppDeps
	mu   sync.Mutex
	once sync.Once
	wg   sync.WaitGroup
}

func newRemediationLifecycle(deps AppDeps) *remediationLifecycle {
	return &remediationLifecycle{deps: deps}
}
func (s *remediationLifecycle) repo() remediation.Repository {
	return remediation.Repository{DB: s.deps.DB}
}
func remediationHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Credentials participate in identity without being persisted, returned, or logged.
func remediationTarget(server Server) string {
	return remediationHash(struct {
		Host, User string
		Port       int
	}{server.Host, server.User, server.Port})
}

func remediationConfiguration(server Server) string {
	return remediationHash(struct {
		Server        Server
		Password, Key string
	}{server, server.Pass, server.Key})
}
func (s *remediationLifecycle) request(server Server, actor, ip, jobID string, policy RetryPolicy) UpdateRunRequest {
	var latest remediation.Plan
	configuration := remediationConfiguration(server)
	return UpdateRunRequest{Server: server, Actor: actor, ClientIP: ip, JobID: jobID, Policy: policy, ForceManual: true,
		PublishPlan: func(status *ServerStatus, preparation updatespkg.RemediationPreparation) error {
			inventory := preparation.Inventory
			preparation.Inventory = nil
			for i := range preparation.Prechecks {
				preparation.Prechecks[i].Output = ""
			}
			if latest.ID != "" && latest.Generation == status.ApprovalGeneration {
				status.RemediationPlanID = latest.ID
				status.RemediationDigest = latest.Digest
				return nil
			}
			ctx := context.Background()
			repo := s.repo()
			key, err := repo.ServerKey(ctx, server.Name)
			if err != nil {
				return err
			}
			before, err := (vulnerabilities.Repository{DB: s.deps.DB}).Load(ctx, server.Name)
			if err != nil {
				return err
			}
			snapshot := serverpkg.CloneServerStatus(status)
			snapshot.Logs = ""
			snapshot.RemediationPlanID = ""
			snapshot.RemediationDigest = ""
			snapshot.Host = ""
			snapshot.User = ""
			snapshot.HasKey = false
			snapshot.HasPassword = false
			// Pending CVE enrichment is not part of the APT transaction; complete proof lives in Before.
			for i := range snapshot.PendingUpdates {
				snapshot.PendingUpdates[i].CVEFindings = nil
				snapshot.PendingUpdates[i].CVEs = nil
			}
			transaction := buildRemediationTransaction(server.Name, snapshot, before, s.deps.Now())
			transaction.Preparation = preparation
			raw, err := json.Marshal(transaction)
			if err != nil {
				return err
			}
			inventoryJSON, err := json.Marshal(inventory)
			if err != nil {
				return err
			}
			p := remediation.Plan{TargetIdentity: remediationTarget(server), Inventory: inventoryJSON, ID: remediation.ID(), ServerKey: key, Server: server.Name, JobID: jobID, Generation: status.ApprovalGeneration, Configuration: configuration, CreatedAt: s.deps.Now().UTC().Format(time.RFC3339Nano), Actor: actor, Snapshot: raw, Before: before, State: "prepared"}
			if before.Accepted != nil {
				p.BeforeScanID = before.Accepted.ID
				p.BeforeAt = before.Accepted.FinishedAt
			}
			p.Digest = remediationHash(struct {
				ID, Configuration string
				Transaction       json.RawMessage
				Before            vulnerabilities.Observation
				Inventory         json.RawMessage
			}{p.ID, configuration, raw, before, inventoryJSON})
			if err = repo.Insert(ctx, p); err != nil {
				return err
			}
			latest = p
			status.RemediationPlanID = p.ID
			status.RemediationDigest = p.Digest
			return nil
		}, ValidatePlan: func() error {
			current, found := s.deps.ServerState.FindByName(server.Name)
			if !found || configuration != remediationConfiguration(current) {
				return errors.New("Server configuration drift")
			}
			if latest.ID == "" {
				return errors.New("missing immutable plan")
			}
			at, err := time.Parse(time.RFC3339Nano, latest.CreatedAt)
			if err != nil || s.deps.Now().Before(at) || s.deps.Now().Sub(at) > time.Hour {
				return errors.New("plan is stale")
			}
			return nil
		}, Finished: func(meta map[string]any) {
			if latest.ID == "" {
				return
			}
			meta["remediation_plan_id"] = latest.ID
			meta["remediation_digest"] = latest.Digest
			meta["before_scan_id"] = latest.BeforeScanID
			latest.Completion = meta
			latest.State = "queued"
			latest.Message = "Maintenance ended; independent CVE verification is queued."
			if err := s.repo().Save(context.Background(), latest, "prepared"); err != nil {
				log.Printf("Persist remediation completion: %v", err)
			}
		}}
}
func (s *remediationLifecycle) Start(ctx context.Context) {
	s.once.Do(func() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			timer := time.NewTicker(5 * time.Second)
			defer timer.Stop()
			for {
				if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
					log.Printf("Reconcile remediation verification: %v", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
				}
			}
		}()
	})
}
func (s *remediationLifecycle) Wait() { s.wg.Wait() }

// Reconcile retries scan admission only. No path in this worker starts maintenance.
func (s *remediationLifecycle) Reconcile(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lease, decision := s.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkScheduled)
	if !decision.Allowed {
		return nil
	}
	defer lease.Close()
	plans, err := s.repo().Pending(ctx)
	if err != nil {
		return err
	}
	for _, p := range plans {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name, err := s.repo().ServerName(ctx, p.ServerKey)
		if err != nil {
			continue
		}
		current, exists := s.deps.ServerState.FindByName(name)
		if !exists {
			continue
		}
		if p.TargetIdentity == "" || p.TargetIdentity != remediationTarget(current) {
			if p.State == "queued" && p.Message != "Server target changed; verification remains queued for the original target" {
				p.Message = "Server target changed; verification remains queued for the original target"
				if err = s.repo().Save(ctx, p, "queued"); err != nil {
					return err
				}
			}
			continue
		}
		job, err := s.deps.CurrentJobManager().GetJob(p.JobID)
		if err != nil {
			continue
		}
		if p.State == "prepared" {
			// A terminal job can still have a live owner persisting completion/audit.
			// Recovery must not steal its prepared -> queued transition.
			if status := s.deps.ServerState.CurrentStatusSnapshot(name); status != nil && status.ActionRunning {
				continue
			}
			if !remediationJobTerminal(job.Status) {
				continue
			}
			p.State = "queued"
			p.Message = "Recovered verification request; maintenance/health result may be unknown."
			if err = s.repo().Save(ctx, p, "prepared"); err != nil {
				return err
			}
		}
		if p.State == "queued" {
			// Admission must occur after FinishActionRunner and the enclosing shared lease drain.
			id, run, admitErr := s.deps.VulnerabilityScans.admitCorrelatedScan(ctx, name, p.Actor, "", p.JobID, p.ID)
			if admitErr != nil {
				if p.Message != admitErr.Error() {
					p.Message = admitErr.Error()
					if err = s.repo().Save(ctx, p, "queued"); err != nil {
						return err
					}
				}
				continue
			}
			p.State = "scanning"
			p.ScanID = id
			p.VerificationScans = append(p.VerificationScans, id)
			p.VerificationServer = name
			p.Message = "Independent installed-package verification in progress"
			if err = s.repo().Save(ctx, p, "queued"); err != nil {
				_ = s.deps.VulnerabilityScans.Cancel(name, id)
				run()
				return err
			}
			s.deps.StartJobRunner(id, run, func() { _ = s.deps.VulnerabilityScans.Cancel(name, id); run() })
		}
		if p.State == "scanning" {
			scanJob, err := s.deps.CurrentJobManager().GetJob(p.ScanID)
			if err != nil || !remediationJobTerminal(scanJob.Status) {
				continue
			}
			var scan vulnerabilities.Scan
			if json.Unmarshal([]byte(scanJob.MetaJSON), &scan) != nil || scan.ID != p.ScanID || scan.ID == p.BeforeScanID || scan.ServerName != p.VerificationServer || scanJob.ServerName != name || scanJob.ParentJobID != p.JobID || scanJob.Status != "succeeded" || (scan.State != "complete" && scan.State != "partial") {
				p.State = "verification_failed"
				p.Message = "Verification did not complete. Retry verification without repeating the update."
			} else {
				comparison := remediation.Compare(p.Before, scan)
				p.Comparison = &comparison
				p.State = "verified"
				if comparison.State == "partial" || comparison.Counts["not_reverified"] > 0 {
					p.State = "partially_verified"
				}
				p.Message = "Package proof comparison complete; review residual exposure and health separately."
			}
			if err = s.repo().Save(ctx, p, "scanning"); err != nil {
				return err
			}
			meta := map[string]any{"remediation_plan_id": p.ID, "update_job_id": p.JobID, "before_scan_id": p.BeforeScanID, "after_scan_id": p.ScanID}
			if p.Comparison != nil {
				meta["comparison_state"] = p.Comparison.State
				meta["comparison_counts"] = p.Comparison.Counts
				meta["fully_corrected_cves"] = p.Comparison.FullyCorrectedCVEs
			}
			s.deps.UpdateService.EnsureDeps().AuditWithActor(p.Actor, "", "remediation.verify", "server", name, p.State, p.Message, meta)
		}
	}
	return nil
}
func remediationJobTerminal(state string) bool {
	return state == "succeeded" || state == "failed" || state == "cancelled" || state == "interrupted"
}
func handlePrepareRemediation(c *gin.Context, deps AppDeps) {
	lifecycle := newServerActionLifecycle(deps, func(action, targetType, targetName, status, message string, meta map[string]any) {
		auditWithService(deps.AuditService, c, action, targetType, targetName, status, message, meta)
	})
	result := lifecycle.startAction(c.Param("name"), actorFromContext(c), c.ClientIP(), "", serverActionStartSpec{status: "updating", packageMutation: true, jobKind: jobKindUpdate, auditAction: "remediation.prepare", startFailure: "Failed to prepare remediation", createFailure: "Failed to create preparation job", successMessage: "Fresh package discovery started; explicit approval is required", runWithJob: func(service *UpdateService, server Server, actor, ip string, policy RetryPolicy, id, _ string) {
		service.RunUpdateJob(deps.Remediation.request(server, actor, ip, id, policy))
		_ = deps.Remediation.Reconcile(currentApplicationMaintenanceContext())
	}})
	c.JSON(result.statusCode, result.body)
}
func remediationOwnedPlan(c *gin.Context, deps AppDeps) (remediation.Plan, bool) {
	p, err := deps.Remediation.repo().Load(c.Request.Context(), c.Param("plan_id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Remediation plan not found"})
		return p, false
	}
	name, err := deps.Remediation.repo().ServerName(c.Request.Context(), p.ServerKey)
	if err != nil || name != c.Param("name") {
		c.JSON(http.StatusNotFound, gin.H{"error": "Remediation plan is not attached to this Server"})
		return p, false
	}
	return p, true
}
func handleRemediationHistory(c *gin.Context, deps AppDeps) {
	if _, found := deps.ServerState.FindByName(c.Param("name")); !found {
		c.JSON(404, gin.H{"error": "Server not found"})
		return
	}
	var key string
	err := deps.DB().QueryRowContext(c.Request.Context(), `SELECT id FROM remediation_servers WHERE server_name=?`, c.Param("name")).Scan(&key)
	if err != nil {
		c.JSON(200, gin.H{"plans": []remediation.Plan{}, "reviews": []remediation.Review{}})
		return
	}
	plans, err := deps.Remediation.repo().List(c.Request.Context(), key)
	if err != nil {
		c.JSON(500, gin.H{"error": "Remediation history unavailable"})
		return
	}
	for i := range plans {
		if job, e := deps.CurrentJobManager().GetJob(plans[i].JobID); e == nil {
			plans[i].UpdateStatus = job.Status
		}
		plans[i] = publicRemediationPlan(plans[i])
		plans[i].Snapshot = nil
		if plans[i].Comparison != nil {
			plans[i].Comparison.Outcomes = nil
		}
	}
	reviews, err := deps.Remediation.repo().Reviews(c.Request.Context(), key)
	if err != nil {
		c.JSON(500, gin.H{"error": "Risk reviews unavailable"})
		return
	}
	setNoStoreHeaders(c)
	c.JSON(200, gin.H{"plans": plans, "reviews": reviews})
}
func handleRemediationDetail(c *gin.Context, deps AppDeps) {
	p, ok := remediationOwnedPlan(c, deps)
	if !ok {
		return
	}
	if job, e := deps.CurrentJobManager().GetJob(p.JobID); e == nil {
		p.UpdateStatus = job.Status
	}
	page := 1
	if raw := c.Query("page"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 1000000 {
			c.JSON(400, gin.H{"error": "Invalid page"})
			return
		}
		page = v
	}
	total := 0
	if p.Comparison != nil {
		total = len(p.Comparison.Outcomes)
		page = min(page, max(1, (total+49)/50))
		start := (page - 1) * 50
		p.Comparison.Outcomes = p.Comparison.Outcomes[start:min(start+50, total)]
	}
	setNoStoreHeaders(c)
	c.JSON(200, gin.H{"plan": publicRemediationPlan(p), "page": page, "pages": max(1, (total+49)/50), "total": total})
}
func handleRetryRemediation(c *gin.Context, deps AppDeps) {
	p, ok := remediationOwnedPlan(c, deps)
	if !ok {
		return
	}
	if p.State != "verification_failed" && p.State != "partially_verified" {
		c.JSON(409, gin.H{"error": "Verification is not retryable in its current state"})
		return
	}
	previousState := p.State
	p.State = "queued"
	p.Message = "Verification retry queued; maintenance will not be repeated"
	if err := deps.Remediation.repo().Save(c.Request.Context(), p, previousState); err != nil {
		c.JSON(409, gin.H{"error": "Verification changed; refresh"})
		return
	}
	auditWithService(deps.AuditService, c, "remediation.retry", "server", c.Param("name"), "queued", p.Message, map[string]any{"remediation_plan_id": p.ID, "update_job_id": p.JobID})
	c.JSON(202, gin.H{"message": p.Message})
}
func handleRiskReview(c *gin.Context, deps AppDeps) {
	var v remediation.Review
	if err := c.ShouldBindJSON(&v); err != nil || !v.Valid(deps.Now()) {
		c.JSON(400, gin.H{"error": "Package, owner, reason, treatment decision and future reevaluation time are required"})
		return
	}
	if _, found := deps.ServerState.FindByName(c.Param("name")); !found {
		c.JSON(404, gin.H{"error": "Server not found"})
		return
	}
	observation, loadErr := (vulnerabilities.Repository{DB: deps.DB}).Load(c.Request.Context(), c.Param("name"))
	known := false
	for _, f := range observation.Findings {
		if f.ResolvedAt == "" && f.Package == v.Package && (v.CVE == "" || v.CVE == f.ID) {
			known = true
		}
	}
	if loadErr != nil || !known {
		c.JSON(409, gin.H{"error": "Review must identify a currently recorded unresolved package finding"})
		return
	}
	key, err := deps.Remediation.repo().ServerKey(c.Request.Context(), c.Param("name"))
	if err != nil {
		c.JSON(500, gin.H{"error": "Cannot record risk review"})
		return
	}
	v.ID = remediation.ID()
	v.Actor = actorFromContext(c)
	v.CreatedAt = deps.Now().UTC().Format(time.RFC3339Nano)
	if err = deps.Remediation.repo().SaveReview(c.Request.Context(), key, v); err != nil {
		c.JSON(500, gin.H{"error": "Cannot record risk review"})
		return
	}
	auditWithService(deps.AuditService, c, "vulnerability.review", "server", c.Param("name"), "recorded", "Risk review recorded; finding remains visible", map[string]any{"review_id": v.ID, "package": v.Package, "cve": v.CVE, "reevaluate_at": v.ReevaluateAt})
	c.JSON(201, v)
}

func (s *remediationLifecycle) ValidateApproval(status *ServerStatus, server Server) error {
	p, err := s.repo().Load(context.Background(), status.RemediationPlanID)
	if err != nil {
		return err
	}
	at, err := time.Parse(time.RFC3339Nano, p.CreatedAt)
	if err != nil || s.deps.Now().Before(at) || s.deps.Now().Sub(at) > time.Hour {
		return errors.New("plan is stale")
	}
	if p.State != "prepared" || p.Digest != status.RemediationDigest || p.JobID != status.JobID || p.Generation != status.ApprovalGeneration || p.Configuration != remediationConfiguration(server) {
		return errors.New("plan or Server configuration drift")
	}
	return nil
}

type remediationScope struct {
	Transaction       []string                       `json:"transaction_packages,omitempty"`
	DependencyImpacts []string                       `json:"dependency_impacts,omitempty"`
	Scope             string                         `json:"scope"`
	Selected          []string                       `json:"selected_packages"`
	Extra             []string                       `json:"extra_packages"`
	Added             []string                       `json:"new_packages"`
	Removed           []string                       `json:"removed_packages"`
	Available         bool                           `json:"available"`
	Reason            string                         `json:"reason,omitempty"`
	Impact            *observability.RemediationHost `json:"impact"`
}
type remediationTransaction struct {
	Preparation updatespkg.RemediationPreparation `json:"preparation"`
	Status      *ServerStatus                     `json:"status"`
	Scopes      []remediationScope                `json:"scopes"`
}

func buildRemediationTransaction(name string, status *ServerStatus, before vulnerabilities.Observation, now time.Time) remediationTransaction {
	out := remediationTransaction{Status: status, Scopes: []remediationScope{}}
	info := observability.ProjectRemediationEvidence(before, now)
	for _, scope := range []string{updatespkg.ApprovalScopeAll, updatespkg.ApprovalScopeSecurity, updatespkg.ApprovalScopeSecurityKeptBack, updatespkg.ApprovalScopeFullUpgrade} {
		eval := updatespkg.EvaluateManualApproval(status, scope, updatespkg.ApprovalScopeOptions{ConfirmRemovals: true})
		row := remediationScope{Scope: scope, Selected: updatespkg.PackagesForApprovalScope(scope, status.PendingUpdates), Extra: []string{}, Added: []string{}, Removed: []string{}, Available: eval.Allowed, Reason: eval.BodyMessage}
		if row.Available {
			row.Reason = "Review the transaction and removals before explicitly approving this scope."
		}
		if scope == updatespkg.ApprovalScopeFullUpgrade {
			row.Added = status.UpgradePlan.FullUpgradeNewPackages
			row.Removed = status.UpgradePlan.FullUpgradeRemovedPackages
		}
		if scope == updatespkg.ApprovalScopeSecurityKeptBack {
			row.Added = status.UpgradePlan.KeptBackSecurityNewPackages
			row.Removed = status.UpgradePlan.KeptBackSecurityRemovedPackages
		}
		if len(status.UpgradePlan.PackageDeferrals.Rules) > 0 {
			protected := status.UpgradePlan.DeferralScopes[scope]
			row.Selected = protected.SelectedPackages
			row.Added = protected.NewPackages
			row.Removed = protected.RemovedPackages
			row.Transaction = protected.Transaction
			row.DependencyImpacts = protected.DependencyImpacts
			row.Available = row.Available && protected.Available
		}
		pending := map[string]PendingUpdate{}
		for _, p := range status.PendingUpdates {
			pending[p.Package] = p
		}
		selected := map[string]bool{}
		for _, pkg := range row.Selected {
			selected[pkg] = true
		}
		scoped := info
		scoped.Findings = append([]observability.DashboardVulnerabilityFinding{}, info.Findings...)
		addressed := map[string]bool{}
		for i := range scoped.Findings {
			f := &scoped.Findings[i]
			actual := pending[f.Package]
			eligible := selected[f.Package] && row.Available && f.InstalledVersion == actual.CurrentVersion && f.Availability != nil && f.CandidateVersion == actual.CandidateVersion && f.Availability.CandidateBinaryVersion == actual.CandidateVersion
			if actual.SourcePackage != "" && f.SourcePackage != actual.SourcePackage {
				eligible = false
			}
			if actual.CandidateSourceVersion != "" && f.Availability != nil && f.Availability.CandidateSourceVersion != actual.CandidateSourceVersion {
				eligible = false
			}
			// A source candidate outside this scope cannot be presented as a security-only fix.
			if eligible && f.Availability != nil {
				eligible = false
				for _, s := range f.Availability.Scopes {
					if s.Scope == scope && s.State == "simulated" {
						eligible = true
					}
				}
			}
			if !eligible {
				if !selected[f.Package] || !row.Available {
					f.Disposition = "still_affected"
				} else if f.Disposition == "fixed_by_candidate" {
					f.Disposition = "candidate_unverified"
				}
			} else if observability.CandidateRemediationFinding(*f) {
				addressed[f.Package] = true
			}
		}
		for _, pkg := range row.Selected {
			if !addressed[pkg] {
				row.Extra = append(row.Extra, pkg)
			}
		}
		plan := observability.BuildRemediationPlan(observability.DashboardSummaryResponse{Servers: []observability.DashboardServerSummary{{Name: name, Risk: observability.DashboardRiskInfo{Vulnerability: scoped}}}}, observability.RemediationQuery{Server: name})
		row.Impact = plan.Selected
		out.Scopes = append(out.Scopes, row)
	}
	return out
}

// Evidence is bounded separately from the immutable transaction and outcome pages.
func handleRemediationEvidence(c *gin.Context, deps AppDeps) {
	p, ok := remediationOwnedPlan(c, deps)
	if !ok {
		return
	}
	page := 1
	if raw := c.Query("page"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 1000000 {
			c.JSON(400, gin.H{"error": "Invalid evidence page"})
			return
		}
		page = v
	}
	var inventory []PendingUpdate
	if json.Unmarshal(p.Inventory, &inventory) != nil {
		inventory = []PendingUpdate{}
	}
	pkg := c.Query("package")
	if len(pkg) > 300 {
		c.JSON(400, gin.H{"error": "Invalid package"})
		return
	}
	facts := []PendingUpdate{}
	findings := []vulnerabilities.Finding{}
	for _, f := range inventory {
		if pkg == "" || f.Package == pkg {
			facts = append(facts, f)
		}
	}
	for _, f := range p.Before.Findings {
		if pkg != "" && f.Package == pkg {
			findings = append(findings, f)
		}
	}
	total := max(len(facts), len(findings))
	pages := max(1, (total+49)/50)
	page = min(page, pages)
	start := (page - 1) * 50
	facts = facts[min(start, len(facts)):min(start+50, len(facts))]
	findings = findings[min(start, len(findings)):min(start+50, len(findings))]
	setNoStoreHeaders(c)
	c.JSON(200, gin.H{"plan_id": p.ID, "before_scan_id": p.BeforeScanID, "before_at": p.BeforeAt, "inventory": facts, "findings": findings, "package_proof": p.Before.PackageProofs[pkg], "page": page, "pages": pages, "total": total})
}

// Operational readers receive health decisions, never maintenance command output.
func publicRemediationPlan(p remediation.Plan) remediation.Plan {
	if p.Completion != nil {
		public := map[string]any{}
		for _, key := range []string{"status", "upgrade_completed", "postchecks_enabled", "postchecks_passed", "postcheck_failed", "postcheck_warnings", "approval_scope", "approved_package_count", "approved_packages", "interrupted", "remediation_plan_id", "remediation_digest", "before_scan_id", "last_error_class"} {
			if value, ok := p.Completion[key]; ok {
				public[key] = value
			}
		}
		p.Completion = public
	}
	return p
}
