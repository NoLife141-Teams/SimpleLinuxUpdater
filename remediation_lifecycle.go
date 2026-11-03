package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
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
	identityKey [32]byte
	deps        AppDeps
	mu          sync.Mutex
	once        sync.Once
	wg          sync.WaitGroup
}

func newRemediationLifecycle(deps AppDeps) *remediationLifecycle {
	s := &remediationLifecycle{deps: deps}
	if _, err := rand.Read(s.identityKey[:]); err != nil {
		panic(err)
	}
	return s
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

// A persisted or public fingerprint must not become an offline SSH-password
// verifier. This key exists only for this app runtime's live approval runners.
// Restart/restore interrupt maintenance; durable verification uses the separate
// credential-free target identity and never depends on this runtime key.
func (s *remediationLifecycle) configuration(server Server) string {
	b, _ := json.Marshal(struct {
		Server        Server
		Password, Key string
	}{server, server.Pass, server.Key})
	mac := hmac.New(sha256.New, s.identityKey[:])
	_, _ = mac.Write([]byte("remediation-configuration-v1\x00"))
	_, _ = mac.Write(b)
	return hex.EncodeToString(mac.Sum(nil))
}
func (s *remediationLifecycle) request(server Server, actor, ip, jobID string, policy RetryPolicy) UpdateRunRequest {
	var latest remediation.Plan
	configuration := s.configuration(server)
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
				for _, selector := range updatespkg.PackageDiscoverySelectors(snapshot.PendingUpdates[i]) {
					if source, ok := preparation.Sources.Packages[selector]; ok {
						snapshot.PendingUpdates[i].SourcePackage = source.InstalledSourcePackage
						snapshot.PendingUpdates[i].InstalledSourceVersion = source.InstalledSourceVersion
						snapshot.PendingUpdates[i].CandidateSourceVersion = source.CandidateSourceVersion
					}
				}
			}
			transaction := buildRemediationTransaction(server.Name, snapshot, before, s.deps.Now(), preparation)
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
			if !found || !hmac.Equal([]byte(configuration), []byte(s.configuration(current))) {
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
			id, run, admitErr := s.deps.VulnerabilityScans.admitCorrelatedScan(ctx, name, p.Actor, "", p.JobID, p.ID, func(reserved Server) error {
				owner, err := s.repo().ServerName(ctx, p.ServerKey)
				if err != nil || owner != reserved.Name || p.TargetIdentity != remediationTarget(reserved) {
					return errors.New("Server identity changed before verification admission; original request remains queued")
				}
				return nil
			})
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
	// Resolve ownership before reading finding evidence. A later delete/rename
	// invalidates this key rather than moving the review onto a replacement.
	key, err := deps.Remediation.repo().ServerKey(c.Request.Context(), c.Param("name"))
	if err != nil {
		c.JSON(500, gin.H{"error": "Cannot record risk review"})
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
	v.ID = remediation.ID()
	v.Actor = actorFromContext(c)
	v.CreatedAt = deps.Now().UTC().Format(time.RFC3339Nano)
	if err = deps.Remediation.repo().SaveReview(c.Request.Context(), key, c.Param("name"), v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(409, gin.H{"error": "Server identity changed; refresh finding evidence before reviewing"})
			return
		}
		c.JSON(500, gin.H{"error": "Cannot record risk review"})
		return
	}
	auditWithService(deps.AuditService, c, "vulnerability.review", "server", c.Param("name"), "recorded", "Risk review recorded; finding remains visible", map[string]any{"review_id": v.ID, "package": v.Package, "cve": v.CVE, "reevaluate_at": v.ReevaluateAt})
	c.JSON(201, v)
}

func (s *remediationLifecycle) ValidateApproval(status *ServerStatus, server Server, scope string, confirmRemovals bool) error {
	p, err := s.repo().Load(context.Background(), status.RemediationPlanID)
	if err != nil {
		return err
	}
	at, err := time.Parse(time.RFC3339Nano, p.CreatedAt)
	if err != nil || s.deps.Now().Before(at) || s.deps.Now().Sub(at) > time.Hour {
		return errors.New("plan is stale")
	}
	if p.State != "prepared" || p.Digest != status.RemediationDigest || p.JobID != status.JobID || p.Generation != status.ApprovalGeneration || !hmac.Equal([]byte(p.Configuration), []byte(s.configuration(server))) {
		return errors.New("plan or Server configuration drift")
	}
	var tx remediationTransaction
	if json.Unmarshal(p.Snapshot, &tx) != nil || !tx.Preparation.Scopes[scope].Available {
		return errors.New("scope requires a complete fresh transaction simulation")
	}
	if len(tx.Preparation.Scopes[scope].Removed) > 0 && !confirmRemovals {
		return errors.New("scope includes removals; explicit confirmation is required")
	}
	return nil
}

type remediationScope struct {
	Simulation        []serverpkg.SimulatedPackage   `json:"simulation_packages,omitempty"`
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

func buildRemediationTransaction(name string, status *ServerStatus, before vulnerabilities.Observation, now time.Time, preparations ...updatespkg.RemediationPreparation) remediationTransaction {
	out := remediationTransaction{Status: status, Scopes: []remediationScope{}}
	info := observability.ProjectRemediationEvidence(before, now)
	for _, scope := range []string{updatespkg.ApprovalScopeAll, updatespkg.ApprovalScopeSecurity, updatespkg.ApprovalScopeSecurityKeptBack, updatespkg.ApprovalScopeFullUpgrade} {
		eval := updatespkg.EvaluateManualApproval(status, scope, updatespkg.ApprovalScopeOptions{ConfirmRemovals: true})
		row := remediationScope{Scope: scope, Selected: updatespkg.PackagesForApprovalScope(scope, status.PendingUpdates), Extra: []string{}, Added: []string{}, Removed: []string{}, Available: eval.Allowed, Reason: eval.BodyMessage}
		if row.Available {
			row.Reason = "Review the transaction and removals before explicitly approving this scope."
		}
		if scope == updatespkg.ApprovalScopeFullUpgrade {
			row.Simulation = status.UpgradePlan.FullUpgradeSimulationPackages
			row.Added = status.UpgradePlan.FullUpgradeNewPackages
			row.Removed = status.UpgradePlan.FullUpgradeRemovedPackages
		}
		if scope == updatespkg.ApprovalScopeSecurityKeptBack {
			row.Simulation = status.UpgradePlan.KeptBackSecuritySimulationPackages
			row.Added = status.UpgradePlan.KeptBackSecurityNewPackages
			row.Removed = status.UpgradePlan.KeptBackSecurityRemovedPackages
		}
		if len(status.UpgradePlan.PackageDeferrals.Rules) > 0 {
			protected := status.UpgradePlan.DeferralScopes[scope]
			// Protected transactions already carry package=version selectors;
			// the unrestricted simulation is not this scope's executable plan.
			row.Simulation = nil
			row.Selected = protected.SelectedPackages
			row.Added = protected.NewPackages
			row.Removed = protected.RemovedPackages
			row.Transaction = protected.Transaction
			row.DependencyImpacts = protected.DependencyImpacts
			row.Available = row.Available && protected.Available
		}
		actual := updatespkg.RemediationScopePreparation{}
		if len(preparations) > 0 {
			actual = preparations[0].Scopes[scope]
			row.Available = row.Available && actual.Available
			row.Simulation = actual.Packages
			row.Added = actual.Added
			row.Removed = actual.Removed
			if !actual.Available {
				row.Reason = "Complete fresh transaction and source/candidate evidence is required before approval."
			}
		}
		pending := map[string]PendingUpdate{}
		selected := map[string]string{}
		selectedNames := map[string]bool{}
		if len(preparations) == 0 {
			for _, pkg := range row.Selected {
				selectedNames[pkg] = true
			}
		}
		simulated := map[string]serverpkg.SimulatedPackage{}
		for _, pkg := range actual.Packages {
			selector := updatespkg.RemediationSimulationSelector(pkg)
			selectedNames[selector] = true
			simulated[selector] = pkg
		}
		for _, p := range status.PendingUpdates {
			selectors := updatespkg.PackageDiscoverySelectors(p)
			selection := ""
			if selectedNames[p.Package] {
				selection = p.Package
			}
			for _, selector := range selectors {
				pending[selector] = p
				if selectedNames[selector] {
					selection = selector
				}
			}
			if selection != "" {
				for _, selector := range selectors {
					selected[selector] = selection
					if v, ok := simulated[selection]; ok {
						simulated[selector] = v
					}
				}
			}
		}
		scoped := info
		scoped.Findings = append([]observability.DashboardVulnerabilityFinding{}, info.Findings...)
		addressed := map[string]bool{}
		for i := range scoped.Findings {
			f := &scoped.Findings[i]
			actual := pending[f.Package]
			selection, included := selected[f.Package]
			eligible := included && row.Available && f.InstalledVersion == actual.CurrentVersion && f.Availability != nil && f.CandidateVersion == actual.CandidateVersion && f.Availability.CandidateBinaryVersion == actual.CandidateVersion
			if len(preparations) > 0 {
				v := simulated[f.Package]
				eligible = eligible && v.CurrentVersion == f.InstalledVersion && v.CandidateVersion == actual.CandidateVersion
				source, ok := preparations[0].Sources.Packages[selection]
				eligible = eligible && ok && source.InstalledProvenanceVerified && source.CandidateProvenanceVerified && source.InstalledVersion == f.InstalledVersion && source.InstalledSourcePackage == f.SourcePackage && source.InstalledSourceVersion == f.Assessment.SourceVersion && source.InstalledEcosystem == f.Assessment.Ecosystem
				if f.Availability != nil {
					eligible = eligible && source.CandidateVersion == f.Availability.CandidateBinaryVersion && source.CandidateSourcePackage == f.Availability.CandidateSourcePackage && source.CandidateSourceVersion == f.Availability.CandidateSourceVersion && source.CandidateEcosystem == source.InstalledEcosystem
				}
			}
			if actual.SourcePackage != "" && f.SourcePackage != actual.SourcePackage {
				eligible = false
			}
			if actual.CandidateSourceVersion != "" && f.Availability != nil && f.Availability.CandidateSourceVersion != actual.CandidateSourceVersion {
				eligible = false
			}
			// A source candidate outside this scope cannot be presented as a security-only fix.
			if eligible && f.Availability != nil && len(preparations) == 0 {
				eligible = false
				for _, s := range f.Availability.Scopes {
					if s.Scope == scope && s.State == "simulated" {
						eligible = true
					}
				}
			}
			if !eligible {
				if !included || !row.Available {
					f.Disposition = "still_affected"
				} else if f.Disposition == "fixed_by_candidate" {
					f.Disposition = "candidate_unverified"
				}
			} else if observability.CandidateRemediationFinding(*f) {
				addressed[selection] = true
				for _, selector := range updatespkg.PackageDiscoverySelectors(actual) {
					addressed[selector] = true
				}
			}
		}
		for _, pkg := range row.Selected {
			if !addressed[pkg] {
				row.Extra = append(row.Extra, pkg)
			}
		}
		row.Extra = append(row.Extra, actual.Extra...)
		plan := observability.BuildRemediationPlan(observability.DashboardSummaryResponse{Servers: []observability.DashboardServerSummary{{Name: name, Risk: observability.DashboardRiskInfo{Vulnerability: scoped}}}}, observability.RemediationQuery{Server: name})
		row.Impact = plan.Selected
		out.Scopes = append(out.Scopes, row)
	}
	return out
}
