const LOG_BOTTOM_THRESHOLD = 20;
        let sortKey = "name";
        let sortDir = "asc";
        let allServers = [];
        let selectedServerName = "";
        let lastSuccessfulSyncAt = null;
        let lastFetchError = null;
        let recentActivity = [];
        let observabilitySummary = null;
        let policySummary = null;
        let dashboardSummary = null;
        let dashboardByServer = new Map();
        let globalKeyAvailable = false;
        let dashboardExtraErrors = new Map();
        let page = 1;
        let selectedServers = new Set();
        let hoveredName = null;
        let expandedHostFactsServers = new Set();
        let expandedMiniLists = new Set();
        let fetchInFlight = false;
        let fetchQueued = false;
        let queuedForceRender = false;
        let fleetQuickFilter = "";
        let fleetTagFilter = "";
        let drawerOpen = false;
        let drawerServerName = "";
        let drawerTab = "logs";
        let drawerLogFollow = true;
        let drawerLogScrollTop = 0;
        let drawerPendingScrollTop = 0;
        let passwordResolve = null;
        let passwordReject = null;
        let passwordModalPreviousFocus = null;
        let drawerPreviousFocus = null;
        let suppressSortClickUntil = 0;
        let actionInteractionDepth = 0;
        let actionInteractionReleaseTimer = null;
        let deferredServerRender = false;
        let dashboardEventSource = null;
        let dashboardEventReconnectTimer = null;
        let dashboardEventReconnectDelay = 1000;
        let serverPollIntervalID = null;
        let dashboardExtrasIntervalID = null;
        const actionInteractionDeferMs = 350;
        const eventBackedServerPollMs = 10000;
        const fallbackServerPollMs = 5000;
        const eventBackedExtrasPollMs = 60000;
        const fallbackExtrasPollMs = 30000;
        const columnResizeStorageKey = "simplelinuxupdater.statusTableColWidths.v15";
        const dashboardFilterStorageKey = "simplelinuxupdater.dashboard.filters.v1";
        const defaultColumnWidths = Object.freeze({
            name: 140,
            status: 116,
            actions: 280
        });
        const minColumnWidths = Object.freeze({
            name: 112,
            status: 96,
            actions: 248
        });
        const maxColumnWidths = Object.freeze({
            name: 240,
            status: 170,
            actions: 360
        });
        const allowedStatuses = new Set([
            "idle", "updating", "pending_approval", "approved", "cancelled",
            "upgrading", "autoremove", "sudoers", "done", "error", "success",
            "failure", "failed", "started", "ignored", "running", "queued", "skipped",
            "facts_refresh"
        ]);
        const activeStatuses = new Set(["updating", "upgrading", "autoremove", "sudoers", "facts_refresh"]);
        const nonFailedStatuses = new Set(["idle", "updating", "pending_approval", "approved", "upgrading", "autoremove", "sudoers", "facts_refresh", "done"]);

        function isRunningTimelineState(state) {
            return ["active", "queued"].includes(String(state || "").toLowerCase());
        }

        function setText(id, value) {
            const el = document.getElementById(id);
            if (el) {
                el.textContent = value;
            }
        }

        function pluralize(count, singular, plural = `${singular}s`) {
            return `${count} ${count === 1 ? singular : plural}`;
        }

        function progressClass(value) {
            const numeric = Number(value || 0);
            const clamped = Number.isFinite(numeric) ? Math.max(0, Math.min(100, numeric)) : 0;
            return `progress-${Math.round(clamped / 5) * 5}`;
        }

        function formatDuration(ms) {
            const value = Number(ms || 0);
            if (!Number.isFinite(value) || value <= 0) return "--";
            if (value < 1000) return `${Math.round(value)}ms`;
            const seconds = value / 1000;
            if (seconds < 60) return `${seconds.toFixed(seconds < 10 ? 1 : 0)}s`;
            const minutes = Math.floor(seconds / 60);
            const remainder = Math.round(seconds % 60);
            return remainder > 0 ? `${minutes}m ${remainder}s` : `${minutes}m`;
        }

        function formatDiskFree(kb) {
            if (kb === null || kb === undefined || kb === "") return "--";
            const value = Number(kb);
            if (!Number.isFinite(value) || value < 0) return "--";
            const gib = value / 1024 / 1024;
            if (gib >= 1) return `${gib.toFixed(gib >= 10 ? 0 : 1)} GiB`;
            return `${Math.round(value / 1024)} MiB`;
        }

        function formatDiskCapacity(freeKB, totalKB) {
            const free = formatDiskFree(freeKB);
            const total = formatDiskFree(totalKB);
            if (free === "--" && total === "--") return "--";
            if (total === "--") return `${free} free`;
            if (free === "--") return `${total} total`;
            return `${free} free of ${total} total`;
        }

        function formatUptime(seconds) {
            const value = Number(seconds || 0);
            if (!Number.isFinite(value) || value <= 0) return "--";
            const days = Math.floor(value / 86400);
            if (days > 0) return `${days}d`;
            const hours = Math.floor(value / 3600);
            if (hours > 0) return `${hours}h`;
            return `${Math.floor(value / 60)}m`;
        }

        function statusLabel(value) {
            return String(value || "unknown").replace(/_/g, " ");
        }

        function formatRelativeTime(date) {
            if (!date) return "Waiting for sync";
            const seconds = Math.max(0, Math.round((Date.now() - date.getTime()) / 1000));
            if (seconds < 5) return "Synced just now";
            if (seconds < 60) return `Synced ${seconds}s ago`;
            const minutes = Math.floor(seconds / 60);
            if (minutes < 60) return `Synced ${minutes}m ago`;
            return `Synced ${Math.floor(minutes / 60)}h ago`;
        }

        function formatRelativeTimestamp(raw, empty = "--") {
            if (!raw) return empty;
            const parsed = new Date(raw);
            if (Number.isNaN(parsed.getTime())) return raw;
            const seconds = Math.max(0, Math.round((Date.now() - parsed.getTime()) / 1000));
            if (seconds < 60) return "just now";
            const minutes = Math.floor(seconds / 60);
            if (minutes < 60) return `${minutes}m ago`;
            const hours = Math.floor(minutes / 60);
            if (hours < 48) return `${hours}h ago`;
            return `${Math.floor(hours / 24)}d ago`;
        }

        function formatCompactSchedule(raw) {
            if (!raw) return "Scheduled";
            const parsed = new Date(raw);
            if (Number.isNaN(parsed.getTime())) return raw;
            const month = parsed.toLocaleString(undefined, { month: "short" });
            const day = parsed.getDate();
            const time = parsed.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false });
            return `${month} ${day} ${time}`;
        }

        function isFailedServer(server) {
            return server?.status === "error";
        }

        function isReachableServer(server) {
            const status = String(server?.status || "").toLowerCase();
            return nonFailedStatuses.has(status) || (status && status !== "error");
        }

        function safeStatusClass(value) {
            const normalized = String(value ?? "").toLowerCase();
            return allowedStatuses.has(normalized) ? normalized : "error";
        }

        function isNearBottom(el) {
            return (el.scrollHeight - el.scrollTop - el.clientHeight) <= LOG_BOTTOM_THRESHOLD;
        }

        function classifyLogLine(line) {
            const lower = line.toLowerCase();
            const bracketState = String(line || "").match(/\[(PASS|WARN|FAIL)\]/i);
            if (bracketState) {
                const state = bracketState[1].toUpperCase();
                if (state === "FAIL") return "error";
                if (state === "WARN") return "warning";
                if (state === "PASS") return "success";
            }
            if (/(error|failed|fatal|denied|refused|panic|timeout|timed out)/.test(lower)) return "error";
            if (/(warn|warning|retry|deprecated)/.test(lower)) return "warning";
            if (/(done|completed|success|approved|enabled|disabled|cancelled)/.test(lower)) return "success";
            if (/(starting|running|connecting|upgradable|upgrade|apt|ssh|info)/.test(lower)) return "info";
            return "";
        }

        function formatLogsHtml(logText) {
            const text = String(logText || "");
            if (!text) {
                return `<div class="log-line log-line-info">No logs yet.</div>`;
            }
            const lines = text.split(/\r?\n/);
            return lines.map(line => {
                const klass = classifyLogLine(line);
                const classAttr = klass ? ` log-line-${klass}` : "";
                const printable = line.length ? line : " ";
                return `<div class="log-line${classAttr}">${escapeHtml(printable)}</div>`;
            }).join("");
        }

        function pendingStateBadge(state) {
            const normalized = String(state || "").toLowerCase();
            if (normalized === "pending") return `<span class="pending-badge">Scanning CVEs...</span>`;
            if (normalized === "unavailable") return `<span class="pending-badge">CVE lookup unavailable</span>`;
            if (normalized === "skipped") return `<span class="pending-badge">CVE lookup skipped</span>`;
            if (normalized === "ready") return "";
            return `<span class="pending-badge">Unknown state</span>`;
        }

        function hasPendingUpdates(server) {
            if (!server || server.status !== "pending_approval") return false;
            return Array.isArray(server.pending_updates) && server.pending_updates.length > 0;
        }

        function getUpgradePlan(server) {
            return server?.upgrade_plan && typeof server.upgrade_plan === "object" ? server.upgrade_plan : {};
        }

        function getPendingApprovalCounts(server) {
            const pendingUpdates = Array.isArray(server?.pending_updates) ? server.pending_updates : [];
            const totalFromPending = pendingUpdates.length;
            const plan = getUpgradePlan(server);
            const planStandard = Number(plan.standard_package_count);
            const planKeptBack = Number(plan.kept_back_package_count);
            const planStandardSecurity = Number(plan.standard_security_count);
            const planTotalSecurity = Number(plan.total_security_count);
            const hasPlan = Number.isFinite(planStandard) && Number.isFinite(planKeptBack) && (planStandard > 0 || planKeptBack > 0 || Number(plan.full_upgrade_package_count || 0) > 0);
            const securityFromPending = pendingUpdates.filter(update => !!update?.security).length;
            const keptBackSecurityFromPending = pendingUpdates.filter(update => !!update?.security && (!!update?.kept_back || !!update?.requires_full_upgrade)).length;
            const standardSecurityFromPending = Math.max(0, securityFromPending - keptBackSecurityFromPending);
            const upgradableFallback = Array.isArray(server?.upgradable) ? server.upgradable.length : 0;
            const total = totalFromPending > 0 ? totalFromPending : upgradableFallback;
            const standard = hasPlan ? planStandard : total;
            const keptBack = hasPlan ? planKeptBack : pendingUpdates.filter(update => !!update?.kept_back || !!update?.requires_full_upgrade).length;
            const full = Number(plan.full_upgrade_package_count || 0) || total;
            const keptBackSecurity = hasPlan && Number.isFinite(planTotalSecurity) && Number.isFinite(planStandardSecurity)
                ? Math.max(0, planTotalSecurity - planStandardSecurity)
                : (totalFromPending > 0 ? keptBackSecurityFromPending : 0);
            return {
                total,
                standard,
                keptBack,
                full,
                security: hasPlan && Number.isFinite(planStandardSecurity) ? planStandardSecurity : (totalFromPending > 0 ? standardSecurityFromPending : null),
                totalSecurity: hasPlan && Number.isFinite(planTotalSecurity) ? planTotalSecurity : (totalFromPending > 0 ? securityFromPending : null),
                keptBackSecurity,
                fullPlanAvailable: !!plan.full_upgrade_plan_available,
                keptBackSecurityPlanAvailable: !!plan.kept_back_security_plan_available,
                keptBackSecurityNewPackages: Array.isArray(plan.kept_back_security_new_packages) ? plan.kept_back_security_new_packages : [],
                keptBackSecurityRemovedPackages: Array.isArray(plan.kept_back_security_removed_packages) ? plan.kept_back_security_removed_packages : [],
                newPackages: Array.isArray(plan.full_upgrade_new_packages) ? plan.full_upgrade_new_packages : [],
                removedPackages: Array.isArray(plan.full_upgrade_removed_packages) ? plan.full_upgrade_removed_packages : []
            };
        }

        function getPendingPackageCount(server) {
            const triageCount = Number(getServerIntelligence(server?.name)?.approval_triage?.pending_packages);
            if (Number.isFinite(triageCount) && triageCount >= 0) return triageCount;
            return getPendingApprovalCounts(server).total;
        }

        function getSecurityUpdateCount(server) {
            const triageCount = Number(getServerIntelligence(server?.name)?.approval_triage?.security_updates);
            if (Number.isFinite(triageCount) && triageCount >= 0) return triageCount;
            const updates = Array.isArray(server?.pending_updates) ? server.pending_updates : [];
            if (updates.length > 0) {
                return updates.filter(update => !!update?.security).length;
            }
            return 0;
        }

        function getRiskLabel(server) {
            const intelligence = getServerIntelligence(server?.name);
            if (intelligence?.approval_triage?.risk_label) return intelligence.approval_triage.risk_label;
            if (intelligence?.risk?.summary) return intelligence.risk.summary;
            const updates = Array.isArray(server?.pending_updates) ? server.pending_updates : [];
            const cveCount = updates.reduce((sum, update) => sum + (Array.isArray(update?.cves) ? update.cves.length : 0), 0);
            const securityCount = getSecurityUpdateCount(server);
            if (cveCount > 0) return `${cveCount} CVE`;
            if (securityCount > 0) return `${securityCount} security`;
            if (getPendingPackageCount(server) > 0) return "Package updates";
            return "Normal";
        }

        function getRiskLevel(server) {
            const intelligence = getServerIntelligence(server?.name);
            return String(intelligence?.approval_triage?.risk_level || intelligence?.risk?.level || "normal").toLowerCase();
        }

        function getServerIntelligence(name) {
            if (!name) return null;
            return dashboardByServer.get(name) || null;
        }

        function getServerTimeline(server) {
            const intelligence = getServerIntelligence(server?.name);
            return intelligence?.timeline || {
                current_phase: "",
                current_label: "Idle",
                state: "idle",
                progress_pct: 0,
                summary: "No maintenance activity",
                phases: []
            };
        }

        function getServerApprovalTriage(server) {
            const intelligence = getServerIntelligence(server?.name);
            const approvalCounts = getPendingApprovalCounts(server);
            return intelligence?.approval_triage || {
                eligible: hasPendingUpdates(server) || approvalCounts.total > 0,
                pending_packages: approvalCounts.total,
                security_updates: getSecurityUpdateCount(server),
                cve_count: (Array.isArray(server?.pending_updates) ? server.pending_updates : [])
                    .reduce((sum, update) => sum + (Array.isArray(update?.cves) ? update.cves.length : 0), 0),
                risk_level: getRiskLevel(server),
                risk_label: getRiskLabel(server),
                risk_order: 1,
                facts_state: "stale",
                last_check_display: "--",
                standard_packages: approvalCounts.standard,
                kept_back_packages: approvalCounts.keptBack,
                standard_security_updates: approvalCounts.security || 0,
                kept_back_security_updates: approvalCounts.keptBackSecurity || 0,
                can_approve_all: server?.status === "pending_approval" && approvalCounts.standard > 0,
                can_approve_security: server?.status === "pending_approval" && (approvalCounts.security || 0) > 0,
                can_approve_kept_back_security: server?.status === "pending_approval" && (approvalCounts.keptBackSecurity || 0) > 0 && approvalCounts.keptBackSecurityPlanAvailable,
                can_approve_full: server?.status === "pending_approval" && approvalCounts.fullPlanAvailable && (approvalCounts.keptBack > 0 || approvalCounts.newPackages.length > 0 || approvalCounts.removedPackages.length > 0),
                can_cancel: server?.status === "pending_approval",
                can_refresh_facts: true,
                can_run_checks: true
            };
        }

        function timelinePhaseMap(server) {
            const phases = Array.isArray(getServerTimeline(server)?.phases) ? getServerTimeline(server).phases : [];
            return new Map(phases.map(phase => [phase.key, phase]));
        }

        function timelinePhaseCell(server, key) {
            const phase = timelinePhaseMap(server).get(key) || { state: "pending", progress_pct: 0 };
            const state = String(phase.state || "pending").toLowerCase();
            const pct = Math.max(0, Math.min(100, Number(phase.progress_pct || 0)));
            const title = [phase.label, phase.summary, phase.updated_at_display].filter(Boolean).join(" · ");
            return `
                <span class="timeline-dot phase-${escapeHtml(state)}" title="${escapeHtml(title)}" aria-label="${escapeHtml(`${phase.label || key}: ${state}`)}">
                    <span class="${progressClass(pct)}"></span>
                </span>
            `;
        }

        function hasEffectiveKey(server) {
            return !!server?.has_key || (!!globalKeyAvailable && !server?.has_key);
        }

        function usesGlobalKey(server) {
            return !!globalKeyAvailable && !server?.has_key;
        }

        function getAuthLabel(server) {
            if (server?.has_key && server?.has_password) return "Server key + password";
            if (usesGlobalKey(server) && server?.has_password) return "Global SSH key + password";
            if (server?.has_key) return "Server key";
            if (usesGlobalKey(server)) return "Global SSH key";
            if (server?.has_password) return "Password";
            return "Missing";
        }

        function getLatestLogLines(server, limit = 5) {
            const lines = String(server?.logs || "")
                .split(/\r?\n/)
                .map(line => line.trim())
                .filter(Boolean);
            return lines.slice(-limit);
        }

        function getAuthPostureMetrics(servers) {
            const withKey = servers.filter(hasEffectiveKey).length;
            const withServerKey = servers.filter(server => !!server.has_key).length;
            const withGlobalKey = servers.filter(usesGlobalKey).length;
            const withPassword = servers.filter(server => !!server.has_password).length;
            const missing = servers.filter(server => !hasEffectiveKey(server) && !server.has_password).length;
            const mixed = servers.filter(server => hasEffectiveKey(server) && !!server.has_password).length;

            let label = "--";
            if (servers.length === 0) {
                label = "--";
            } else if (missing > 0) {
                label = "Gaps";
            } else if (mixed > 0 || (withKey > 0 && withPassword > 0)) {
                label = "Mixed";
            } else if (withKey > 0) {
                label = "Key";
            } else if (withPassword > 0) {
                label = "Password";
            }

            return { label, withKey, withServerKey, withGlobalKey, withPassword, missing };
        }

        function renderDashboardMetrics() {
            const total = allServers.length;
            const reachable = allServers.filter(isReachableServer).length;
            const pending = Number(dashboardSummary?.fleet?.pending_approval ?? allServers.filter(server => server.status === "pending_approval").length);
            const active = Number(dashboardSummary?.fleet?.in_progress ?? allServers.filter(server => activeStatuses.has(server.status) || isRunningTimelineState(getServerTimeline(server).state)).length);
            const failed = allServers.filter(server => server.status === "error").length;
            const done = Number(dashboardSummary?.fleet?.done ?? allServers.filter(server => server.status === "done").length);
            const prechecks = Number(dashboardSummary?.fleet?.prechecks_running ?? active);
            const highRiskCVE = Number(dashboardSummary?.fleet?.high_risk_cve ?? 0);
            const pendingPackages = Number(dashboardSummary?.fleet?.pending_packages ?? allServers.reduce((sum, server) => sum + getPendingPackageCount(server), 0));
            const securityUpdates = Number(dashboardSummary?.fleet?.security_updates ?? allServers.reduce((sum, server) => sum + getSecurityUpdateCount(server), 0));
            const staleFacts = Number(dashboardSummary?.fleet?.stale_facts || 0);
            const auth = getAuthPostureMetrics(allServers);

            setText("metric-total-hosts", String(total));
            setText("metric-total-note", total === 0 ? "No servers loaded" : `${pluralize(total, "host")} monitored`);
            setText("metric-reachable-hosts", String(reachable));
            setText("metric-pending-approvals", String(pending));
            setText("metric-prechecks", String(prechecks));
            setText("metric-active-runs", String(active));
            setText("metric-done-hosts", String(done));
            setText("metric-failed-hosts", String(failed));
            setText("metric-pending-packages", String(pendingPackages));
            setText("metric-security-updates", String(securityUpdates));
            setText("metric-stale-facts", String(staleFacts));
            setText("metric-high-risk-cve", String(highRiskCVE));
            setText("metric-auth-posture", auth.label);
            setText("metric-auth-note", `${auth.withServerKey} server key · ${auth.withGlobalKey} global SSH key · ${auth.withPassword} password · ${auth.missing} missing`);
        }

        function renderPendingUpdatesHtml(server, includeHeading = true) {
            const updates = Array.isArray(server?.pending_updates) ? server.pending_updates : [];
            if (server?.status !== "pending_approval" || updates.length === 0) {
                return `<p class="drawer-empty">No pending update details for this server.</p>`;
            }

            const hasPending = updates.some(update => String(update.cve_state || "").toLowerCase() === "pending");
            const securityCount = updates.filter(update => !!update.security).length;
            const keptBackCount = updates.filter(update => !!update.kept_back || !!update.requires_full_upgrade).length;
            const stateCounts = updates.reduce((acc, update) => {
                const state = String(update.cve_state || "").toLowerCase();
                acc[state] = (acc[state] || 0) + 1;
                return acc;
            }, {});

            const rows = updates.map(update => {
                const pkg = escapeHtml(update.package || "unknown");
                const currentVersion = escapeHtml(update.current_version || "?");
                const candidateVersion = escapeHtml(update.candidate_version || "?");
                const source = escapeHtml(update.source || "");
                const state = String(update.cve_state || "").toLowerCase();
                const cves = Array.isArray(update.cves) ? update.cves : [];

                const badges = [];
                if (update.security) badges.push(`<span class="pending-badge pending-badge-security">Security</span>`);
                if (update.kept_back || update.requires_full_upgrade) {
                    badges.push(`<span class="pending-badge">Kept back</span>`);
                    badges.push(`<span class="pending-badge">Requires full-upgrade</span>`);
                }
                if (state === "ready") {
                    if (cves.length > 0) {
                        badges.push(`<span class="pending-badge pending-badge-cve">${cves.length} CVE${cves.length > 1 ? "s" : ""}</span>`);
                        cves.slice(0, 3).forEach((cve) => {
                            badges.push(`<span class="pending-badge">${escapeHtml(cve)}</span>`);
                        });
                    } else {
                        badges.push(`<span class="pending-badge">No CVE found</span>`);
                    }
                } else {
                    badges.push(pendingStateBadge(state));
                }

                return `
                    <tr>
                        <td>
                            <div>${pkg}</div>
                            ${source ? `<div class="subtle">${source}</div>` : ""}
                        </td>
                        <td>${currentVersion} &rarr; ${candidateVersion}</td>
                        <td><div class="pending-badges">${badges.join("")}</div></td>
                    </tr>
                `;
            }).join("");

            return `
                <div class="pending-updates">
                    ${includeHeading ? "<h4>Pending updates (security first)</h4>" : ""}
                    <div class="pending-summary">
                        <span class="pending-badge">${updates.length} package${updates.length > 1 ? "s" : ""}</span>
                        <span class="pending-badge pending-badge-security">${securityCount} security</span>
                        <span class="pending-badge">${keptBackCount} kept back</span>
                        <span class="pending-badge">${stateCounts.ready || 0} ready</span>
                        <span class="pending-badge">${stateCounts.pending || 0} scanning</span>
                        <span class="pending-badge">${stateCounts.unavailable || 0} unavailable</span>
                        <span class="pending-badge">${stateCounts.skipped || 0} skipped</span>
                    </div>
                    ${hasPending ? `<p class="pending-note">CVE scan in progress; list will update automatically.</p>` : ""}
                    <div class="table-wrap">
                        <table class="pending-table">
                            <thead>
                                <tr>
                                    <th>Package</th>
                                    <th>Version</th>
                                    <th>Risk</th>
                                </tr>
                            </thead>
                            <tbody>${rows}</tbody>
                        </table>
                    </div>
                </div>
            `;
        }

        function renderSyncState() {
            const pollingEl = document.getElementById('polling-state-label');
            const lastSyncEl = document.getElementById('last-sync-label');
            const extrasError = dashboardExtraErrors.size > 0
                ? Array.from(dashboardExtraErrors.values()).join("; ")
                : "";
            const degraded = !!lastFetchError || !!extrasError;
            if (pollingEl) {
                pollingEl.textContent = degraded ? "Polling degraded" : (dashboardEventSource ? "Live events" : "Live polling");
                pollingEl.classList.toggle('warning', degraded);
                pollingEl.classList.toggle('live', !degraded);
            }
            if (lastSyncEl) {
                lastSyncEl.textContent = lastFetchError || extrasError
                    ? `Last sync error: ${lastFetchError?.message || extrasError || "unknown"}`
                    : formatRelativeTime(lastSuccessfulSyncAt);
                lastSyncEl.classList.toggle('warning', degraded);
            }
        }

        function setDashboardExtraError(key, err) {
            if (err) {
                const message = err.message || "unknown error";
                dashboardExtraErrors.set(key, `${key}: ${message}`);
            } else {
                dashboardExtraErrors.delete(key);
            }
            renderSyncState();
        }

        function miniEmpty(text) {
            return `<p class="empty-state">${escapeHtml(text)}</p>`;
        }

        function renderServerTags(server) {
            const tags = Array.isArray(server?.tags) ? server.tags.filter(Boolean) : [];
            if (tags.length === 0) return `<span class="chip muted-chip">untagged</span>`;
            return tags.map(tag => `<span class="chip">${escapeHtml(tag)}</span>`).join("");
        }

        function renderMiniServerList(id, servers, emptyText, options = {}) {
            const el = document.getElementById(id);
            if (!el) return;
            if (!Array.isArray(servers) || servers.length === 0) {
                el.innerHTML = miniEmpty(emptyText);
                return;
            }
            const limit = Math.max(1, Number(options.limit || 3));
            const expandable = !!options.expandable;
            const expanded = expandable && expandedMiniLists.has(id);
            const visibleServers = expanded ? servers : servers.slice(0, limit);
            const hiddenCount = Math.max(0, servers.length - visibleServers.length);
            const rows = visibleServers.map(server => {
                const safeName = escapeHtml(server.name || "");
                const safeDataName = escapeHtml(server.name || "");
                const status = statusLabel(server.status);
                const risk = getRiskLabel(server);
                const action = options.action || "open-drawer";
                const actionLabel = options.actionLabel || "Logs";
                const actionTab = options.actionTab || "logs";
                const detail = options.compactDetail
                    ? risk
                    : `${status} · ${risk}`;
                return `
                    <div class="mini-row compact-row">
                        <button type="button" class="mini-row-main" data-select-server="${safeDataName}">
                            <strong>${safeName || "Unnamed host"}</strong>
                            <span>${escapeHtml(detail)}</span>
                        </button>
                        <button type="button" class="mini-action" data-action="${action}" data-name="${safeDataName}" data-tab="${actionTab}">${actionLabel}</button>
                    </div>
                `;
            });
            if (expandable && servers.length > limit) {
                const moreText = expanded
                    ? (options.lessLabel || "Show fewer")
                    : (typeof options.moreLabel === "function" ? options.moreLabel(hiddenCount, servers.length) : `+${hiddenCount} more`);
                rows.push(`<button type="button" class="mini-more-row mini-more-button" data-toggle-mini-list="${escapeHtml(id)}" aria-expanded="${expanded ? "true" : "false"}">${escapeHtml(moreText)}</button>`);
            } else if (hiddenCount > 0) {
                const moreText = typeof options.moreLabel === "function"
                    ? options.moreLabel(hiddenCount, servers.length)
                    : `+${hiddenCount} more`;
                rows.push(`<div class="mini-more-row">${escapeHtml(moreText)}</div>`);
            }
            el.innerHTML = rows.join("");
        }

        function compareRiskPriority(a, b) {
            const aTriage = getServerApprovalTriage(a);
            const bTriage = getServerApprovalTriage(b);
            const fallbackOrder = {
                critical: 4,
                high: 4,
                elevated: 3,
                warning: 2,
                normal: 1,
                routine: 1
            };
            const aOrder = Number(aTriage.risk_order || 0) || fallbackOrder[getRiskLevel(a)] || 0;
            const bOrder = Number(bTriage.risk_order || 0) || fallbackOrder[getRiskLevel(b)] || 0;
            return bOrder - aOrder
                || Number(bTriage.cve_count || 0) - Number(aTriage.cve_count || 0)
                || Number(bTriage.security_updates || 0) - Number(aTriage.security_updates || 0)
                || Number(bTriage.pending_packages || 0) - Number(aTriage.pending_packages || 0)
                || String(a.name || "").localeCompare(String(b.name || ""));
        }

        function renderTagSummary() {
            const el = document.getElementById('tag-summary');
            if (!el) return;
            const counts = new Map();
            allServers.forEach(server => {
                const tags = Array.isArray(server.tags) && server.tags.length ? server.tags : ["untagged"];
                tags.forEach(tag => counts.set(tag, (counts.get(tag) || 0) + 1));
            });
            const entries = Array.from(counts.entries()).sort((a, b) => {
                if (b[1] === a[1]) return a[0].localeCompare(b[0]);
                return b[1] - a[1];
            });
            if (entries.length === 0) {
                el.innerHTML = miniEmpty("No tags yet.");
                return;
            }
            el.innerHTML = entries.slice(0, 10).map(([tag, count]) => (
                `<span class="chip">${escapeHtml(tag)} <strong>${count}</strong></span>`
            )).join("");
        }

        function renderFleetFilters() {
            setText("fleet-filter-summary", `${pluralize(applyFilters(allServers).length, "host")} visible`);
            const statusEl = document.getElementById('fleet-status-filters');
            if (statusEl) {
                const activeCount = allServers.filter(server => activeStatuses.has(server.status) || isRunningTimelineState(getServerTimeline(server).state)).length;
                const staleCount = allServers.filter(server => getServerApprovalTriage(server).facts_state === "stale").length;
                const highRiskCount = allServers.filter(server => getServerApprovalTriage(server).cve_count > 0 || getRiskLevel(server) === "critical").length;
                const filters = [
                    { key: "", label: "All", count: allServers.length },
                    { key: "pending_approval", label: "Pending", count: allServers.filter(server => server.status === "pending_approval").length },
                    { key: "active", label: "Active", count: activeCount },
                    { key: "stale_facts", label: "Stale", count: staleCount },
                    { key: "high_risk", label: "High risk", count: highRiskCount }
                ];
                statusEl.innerHTML = filters.map(item => `
                    <button type="button" class="filter-pill${fleetQuickFilter === item.key ? " active" : ""}" data-fleet-filter="${escapeHtml(item.key)}">
                        <span>${escapeHtml(item.label)}</span>
                        <strong>${item.count}</strong>
                    </button>
                `).join("");
            }

            const tagEl = document.getElementById('fleet-tag-list');
            if (!tagEl) return;
            const counts = new Map();
            allServers.forEach(server => {
                const tags = Array.isArray(server.tags) && server.tags.length ? server.tags : ["untagged"];
                tags.forEach(tag => counts.set(tag, (counts.get(tag) || 0) + 1));
            });
            const entries = Array.from(counts.entries()).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
            if (entries.length === 0) {
                tagEl.innerHTML = `<span class="empty-state compact-empty">No tags</span>`;
                return;
            }
            tagEl.innerHTML = [
                `<button type="button" class="filter-pill${fleetTagFilter === "" ? " active" : ""}" data-fleet-tag=""><span>All tags</span><strong>${allServers.length}</strong></button>`,
                ...entries.slice(0, 8).map(([tag, count]) => `
                    <button type="button" class="filter-pill${fleetTagFilter === tag ? " active" : ""}" data-fleet-tag="${escapeHtml(tag)}">
                        <span>${escapeHtml(tag)}</span>
                        <strong>${count}</strong>
                    </button>
                `)
            ].join("");
        }

        function renderApprovalTriage() {
            const body = document.getElementById('approval-triage-body');
            if (!body) return;
            const servers = applyFilters(allServers)
                .filter(server => getServerApprovalTriage(server).eligible || server.status === "pending_approval")
                .sort((a, b) => {
                    const aTriage = getServerApprovalTriage(a);
                    const bTriage = getServerApprovalTriage(b);
                    const riskDelta = Number(bTriage.risk_order || 0) - Number(aTriage.risk_order || 0);
                    if (riskDelta !== 0) return riskDelta;
                    const pendingDelta = Number(bTriage.pending_packages || 0) - Number(aTriage.pending_packages || 0);
                    if (pendingDelta !== 0) return pendingDelta;
                    return String(a.name || "").localeCompare(String(b.name || ""));
                });
            setText("approval-queue-count", String(servers.length));
            if (servers.length === 0) {
                body.innerHTML = `<tr><td colspan="9">${miniEmpty("No approvals require triage.")}</td></tr>`;
                return;
            }
            body.innerHTML = servers.slice(0, 12).map(server => {
                const safeName = escapeHtml(server.name || "");
                const safeDataName = escapeHtml(server.name || "");
                const triage = getServerApprovalTriage(server);
                const approvalCounts = getPendingApprovalCounts(server);
                const keptBackSecurityCount = Number(triage.kept_back_security_updates ?? approvalCounts.keptBackSecurity ?? 0);
                const canApproveKeptBackSecurity = server.status === "pending_approval" && keptBackSecurityCount > 0 && approvalCounts.keptBackSecurityPlanAvailable;
                const riskLevel = String(triage.risk_level || getRiskLevel(server)).toLowerCase();
                const tags = Array.isArray(server.tags) && server.tags.length ? server.tags.join(", ") : "ungrouped";
                const factsState = String(triage.facts_state || "stale").toLowerCase();
                const actions = server.status === "pending_approval"
                    ? `
	                        <div class="triage-actions">
	                            <button type="button" data-action="approve-all" data-name="${safeDataName}" ${triage.can_approve_all ? "" : "disabled"}>Approve (${Number(triage.standard_packages ?? approvalCounts.standard)})</button>
	                            <button type="button" class="btn-security" data-action="approve-security" data-name="${safeDataName}" ${triage.can_approve_security ? "" : "disabled"} title="Approve only standard security updates">Std sec (${Number(triage.standard_security_updates ?? approvalCounts.security ?? 0)})</button>
	                            ${canApproveKeptBackSecurity ? `<button type="button" class="btn-security" data-action="approve-security-kept-back" data-name="${safeDataName}" title="Approve only kept-back security updates">Kept sec (${keptBackSecurityCount})</button>` : ""}
	                            ${triage.can_approve_full ? `<button type="button" class="btn-full-upgrade" data-action="approve-full" data-name="${safeDataName}" title="Run apt full-upgrade">Full upgrade (${approvalCounts.full})</button>` : ""}
	                            <button type="button" class="btn-danger" data-action="cancel-upgrade" data-name="${safeDataName}" ${triage.can_cancel ? "" : "disabled"}>Cancel</button>
                            <button type="button" class="btn-ghost" data-action="open-drawer" data-name="${safeDataName}" data-tab="pending">Packages</button>
                        </div>
                    `
                    : `
                        <div class="triage-actions">
                            <button type="button" data-action="update-server" data-name="${safeDataName}" ${triage.can_run_checks ? "" : "disabled"}>Update</button>
                            <button type="button" class="btn-ghost" data-action="refresh-facts" data-name="${safeDataName}" title="Refresh host facts">Facts</button>
                            <button type="button" class="btn-ghost" data-action="open-drawer" data-name="${safeDataName}" data-tab="logs">Logs</button>
                        </div>
                    `;
                return `
                    <tr data-name="${safeDataName}" class="${selectedServerName === server.name ? "row-selected" : ""}">
                        <td><button type="button" class="select-host" data-select-host="${safeDataName}">${safeName || "Unnamed host"}</button></td>
                        <td>${escapeHtml(tags)}</td>
                        <td>${Number(triage.pending_packages || 0)}</td>
                        <td>${Number(triage.security_updates || 0)}</td>
                        <td>${Number(triage.cve_count || 0)}</td>
                        <td><span class="facts-pill facts-${escapeHtml(factsState)}">${escapeHtml(factsState)}</span></td>
                        <td>${escapeHtml(triage.last_check_display || triage.last_check_at || "--")}</td>
                        <td><span class="risk-chip risk-${escapeHtml(riskLevel)}">${escapeHtml(triage.risk_label || getRiskLabel(server))}</span></td>
                        <td>${actions}</td>
                    </tr>
                `;
            }).join("");
        }

        function renderScheduledRuns() {
            const el = document.getElementById('scheduled-runs');
            if (!el) return;
            const scheduled = allServers
                .map(server => ({ server, nextRun: getServerIntelligence(server.name)?.next_run || {} }))
                .filter(item => item.nextRun.state === "scheduled")
                .sort((a, b) => String(a.nextRun.scheduled_for_utc || "").localeCompare(String(b.nextRun.scheduled_for_utc || "")));
            setText("scheduled-runs-count", String(scheduled.length));
            if (scheduled.length === 0) {
                el.innerHTML = miniEmpty("No scheduled runs.");
                return;
            }
            const listID = "scheduled-runs";
            const limit = 6;
            const expanded = expandedMiniLists.has(listID);
            const visibleScheduled = expanded ? scheduled : scheduled.slice(0, limit);
            const rows = visibleScheduled.map(({ server, nextRun }) => {
                const safeName = escapeHtml(server.name || "");
                const safeDataName = escapeHtml(server.name || "");
                const label = nextRun.policy_name || "Policy";
                const when = formatCompactSchedule(nextRun.scheduled_for_utc || nextRun.scheduled_for_display);
                return `
                    <div class="mini-row compact-row">
                        <button type="button" class="mini-row-main" data-select-server="${safeDataName}">
                            <strong>${safeName}</strong>
                            <span>${escapeHtml(label)} · ${escapeHtml(when)}</span>
                        </button>
                        <span class="mini-badge">${escapeHtml(nextRun.status || "scheduled")}</span>
                    </div>
                `;
            });
            const hiddenCount = scheduled.length - visibleScheduled.length;
            if (scheduled.length > limit) {
                const moreText = expanded ? "Show fewer scheduled runs" : `+${hiddenCount} more scheduled`;
                rows.push(`<button type="button" class="mini-more-row mini-more-button" data-toggle-mini-list="${listID}" aria-expanded="${expanded ? "true" : "false"}">${escapeHtml(moreText)}</button>`);
            }
            el.innerHTML = rows.join("");
        }

        function formatActivityTime(evt) {
            if (window.formatAppTimestamp) {
                const formatted = window.formatAppTimestamp(evt?.created_at, { titleUTC: true, preformattedPrimary: evt?.created_at_display });
                return formatted.primary || evt?.created_at || "";
            }
            return evt?.created_at_display || evt?.created_at || "";
        }

        function renderRecentActivity() {
            const el = document.getElementById('recent-activity');
            if (!el) return;
            if (!Array.isArray(recentActivity) || recentActivity.length === 0) {
                el.innerHTML = miniEmpty("No recent activity.");
                return;
            }
            el.innerHTML = recentActivity.slice(0, 2).map(evt => {
                const status = String(evt.status || "unknown").toLowerCase();
                const statusClass = safeStatusClass(status === "failure" ? "error" : status);
                const target = [evt.target_type, evt.target_name].filter(Boolean).join(": ");
                return `
                    <div class="activity-row">
                        <span class="status-pill status-${statusClass}">${escapeHtml(status || "unknown")}</span>
                        <div>
                            <strong>${escapeHtml(evt.action || "activity")}</strong>
                            <span>${escapeHtml(target || evt.message || "system")} · ${escapeHtml(formatActivityTime(evt))}</span>
                        </div>
                    </div>
                `;
            }).join("");
        }

        function renderIntelligenceLists() {
            const rebootHosts = allServers
                .filter(server => getServerIntelligence(server.name)?.health?.reboot_required === true)
                .sort(compareRiskPriority);
            const riskHosts = allServers.filter(server => {
                const level = getRiskLevel(server);
                return level === "critical" || level === "elevated";
            }).sort(compareRiskPriority);
            setText("reboot-required-count", String(rebootHosts.length));
            setText("risk-exposure-count", String(riskHosts.length));
            renderMiniServerList("reboot-required-panel", rebootHosts, "No reboot required.", {
                limit: 1,
                expandable: true,
                compactDetail: true,
                action: "open-drawer",
                actionLabel: "Logs",
                lessLabel: "Show fewer reboot hosts",
                moreLabel: hidden => `+${hidden} more reboot host${hidden === 1 ? "" : "s"}`
            });
            renderMiniServerList("risk-exposure-panel", riskHosts, "No CVE exposure.", {
                limit: 1,
                expandable: true,
                compactDetail: true,
                action: "open-drawer",
                actionLabel: "Review",
                actionTab: "pending",
                lessLabel: "Show fewer risk hosts",
                moreLabel: hidden => `+${hidden} more risk host${hidden === 1 ? "" : "s"}`
            });
            renderCommandHistoryPanel();
        }

        function renderCommandHistoryPanel() {
            const el = document.getElementById('command-history-panel');
            if (!el) return;
            const intelligence = getServerIntelligence(selectedServerName);
            const history = Array.isArray(intelligence?.command_history) ? intelligence.command_history : [];
            setText("command-history-count", String(history.length));
            if (history.length === 0) {
                el.innerHTML = miniEmpty("No command history.");
                return;
            }
            const listID = "command-history-panel";
            const limit = 3;
            const expanded = expandedMiniLists.has(listID);
            const visibleHistory = expanded ? history : history.slice(0, limit);
            const rows = visibleHistory.map(item => {
                const status = String(item.status || "unknown").toLowerCase();
                const statusClass = safeStatusClass(status === "failure" ? "error" : status);
                return `
                    <div class="activity-row">
                        <span class="status-pill status-${statusClass}">${escapeHtml(status || "unknown")}</span>
                        <div>
                            <strong>${escapeHtml(item.action || "command")}</strong>
                            <span>${escapeHtml(item.message || selectedServerName || "server")} · ${escapeHtml(item.created_at_display || formatRelativeTimestamp(item.created_at))}</span>
                        </div>
                    </div>
                `;
            });
            const hiddenCount = history.length - visibleHistory.length;
            if (history.length > limit) {
                const moreText = expanded ? "Show fewer commands" : `+${hiddenCount} more command${hiddenCount === 1 ? "" : "s"}`;
                rows.push(`<button type="button" class="mini-more-row mini-more-button" data-toggle-mini-list="${listID}" aria-expanded="${expanded ? "true" : "false"}">${escapeHtml(moreText)}</button>`);
            }
            el.innerHTML = rows.join("");
        }

        function renderSummaryBadges() {
            const policyEl = document.getElementById('policy-summary-label');
            if (policyEl) {
                const count = Array.isArray(policySummary) ? policySummary.length : null;
                policyEl.textContent = count === null ? "Policies --" : `Policies ${count}`;
            }
            const obsEl = document.getElementById('observability-summary-label');
            if (obsEl) {
                const total = Number(observabilitySummary?.totals?.updates_total || 0);
                const success = Number(observabilitySummary?.totals?.success_rate_pct || 0);
                obsEl.textContent = total > 0 ? `7d ${success.toFixed(0)}%` : "7d no runs";
            }
        }

        function renderSelectedHostPanel() {
            const panel = document.getElementById('selected-host-panel');
            const title = document.getElementById('selected-host-title');
            const subtitle = document.getElementById('selected-host-subtitle');
            if (!panel || !title || !subtitle) return;
            const server = getServerByName(selectedServerName);
            if (!server) {
                title.textContent = "No host selected";
                subtitle.textContent = "Select a table row to inspect host details.";
                panel.innerHTML = miniEmpty("No host selected.");
                return;
            }
            const safeName = escapeHtml(server.name || "");
            const safeDataName = escapeHtml(server.name || "");
            const safeStatus = safeStatusClass(server.status);
            const pendingCount = getPendingPackageCount(server);
            const securityCount = getSecurityUpdateCount(server);
            const approvalCounts = getPendingApprovalCounts(server);
            const intelligence = getServerIntelligence(server.name);
            const timeline = getServerTimeline(server);
            const triage = getServerApprovalTriage(server);
            const keptBackSecurityCount = Number(triage.kept_back_security_updates ?? approvalCounts.keptBackSecurity ?? 0);
            const canApproveKeptBackSecurity = server.status === "pending_approval" && keptBackSecurityCount > 0 && approvalCounts.keptBackSecurityPlanAvailable;
            const health = intelligence?.health || {};
            const nextRun = intelligence?.next_run || {};
            const noRun = intelligence?.no_run || {};
            const lastUpdate = intelligence?.last_update;
            const lastFailed = intelligence?.last_failed_update;
            const rebootText = health.reboot_required === true ? "Required" : (health.reboot_required === false ? "Not required" : "Unknown");
            const factsAge = health.collected_at ? formatRelativeTimestamp(health.collected_at, "Facts not collected") : "Facts not collected";
            const canRunUpdate = !!triage.can_run_checks && server.status !== "pending_approval";
            const factsMoreOpen = expandedHostFactsServers.has(server.name);
            const packageSummaryParts = [
                `${Number(triage.pending_packages ?? pendingCount)} pending`,
                `${Number(triage.standard_packages ?? approvalCounts.standard)} standard`,
                `${Number(triage.kept_back_packages ?? approvalCounts.keptBack)} kept back`,
                `${Number(triage.security_updates ?? securityCount)} security`,
            ];
            if (keptBackSecurityCount > 0) {
                packageSummaryParts.push(`${keptBackSecurityCount} kept-back security`);
            }
            packageSummaryParts.push(`${Number(triage.cve_count || 0)} CVE`);
            const packageSummary = packageSummaryParts.join(" · ");
            const lastUpdateSummary = lastUpdate ? `${formatRelativeTimestamp(lastUpdate.finished_at)} · ${formatDuration(lastUpdate.duration_ms)}` : "No update history";
            const nextRunSummary = nextRun.state === "scheduled" ? `${nextRun.policy_name || "Policy"} · ${nextRun.scheduled_for_display || nextRun.scheduled_for_utc}` : "No scheduled run";
            title.textContent = server.name || "Selected host";
            subtitle.textContent = `${server.user || "user"}@${server.host || "host"}:${server.port || 22}`;
            panel.innerHTML = `
                <div class="selected-status-row">
                    <span class="status-pill status-${safeStatus}">${escapeHtml(statusLabel(server.status))}</span>
                    <span class="risk-chip risk-${escapeHtml(getRiskLevel(server))}">${escapeHtml(getRiskLabel(server))}</span>
                    <span class="stage-chip phase-${escapeHtml(timeline.state || "idle")}">${escapeHtml(timeline.current_label || "Idle")}</span>
                </div>
                <div class="inspector-actions inspector-actions-primary">
                    ${server.status === 'pending_approval' ? `<button type="button" class="inline-btn btn-success" data-action="approve-all" data-name="${safeDataName}">Approve</button>` : ""}
                    ${server.status === 'pending_approval' ? `<button type="button" class="inline-btn btn-security" data-action="approve-security" data-name="${safeDataName}" title="Approve only standard security updates">Std security</button>` : ""}
                    ${canApproveKeptBackSecurity ? `<button type="button" class="inline-btn btn-security" data-action="approve-security-kept-back" data-name="${safeDataName}" title="Approve only kept-back security updates">Kept sec (${keptBackSecurityCount})</button>` : ""}
                    ${triage.can_approve_full ? `<button type="button" class="inline-btn btn-full-upgrade" data-action="approve-full" data-name="${safeDataName}" title="Run apt full-upgrade">Full (${approvalCounts.full})</button>` : ""}
                    ${canRunUpdate ? `<button type="button" class="inline-btn primary-action" data-action="update-server" data-name="${safeDataName}">Update</button>` : ""}
                    <button type="button" class="inline-btn btn-ghost" data-action="open-drawer" data-name="${safeDataName}" data-tab="logs">Logs</button>
                    ${hasPendingUpdates(server) ? `<button type="button" class="inline-btn btn-ghost" data-action="open-drawer" data-name="${safeDataName}" data-tab="pending">Packages</button>` : ""}
                </div>
                <div class="inspector-tools">
                    <span class="mini-label">Tools</span>
                    <div class="inspector-actions inspector-actions-secondary">
                        <button type="button" class="inline-btn" data-action="run-autoremove" data-name="${safeDataName}">Autoremove</button>
                        <button type="button" class="inline-btn" data-action="refresh-facts" data-name="${safeDataName}">Facts</button>
                        <button type="button" class="inline-btn" data-action="enable-apt" data-name="${safeDataName}" title="Enable passwordless apt">Enable apt</button>
                        <button type="button" class="inline-btn" data-action="disable-apt" data-name="${safeDataName}" title="Disable passwordless apt">Disable apt</button>
                    </div>
                </div>
                <dl class="host-facts host-facts-primary">
                    <div><dt>Packages</dt><dd>${escapeHtml(packageSummary)}</dd></div>
                    <div><dt>OS</dt><dd>${escapeHtml(health.os_pretty_name || "Facts not collected")}</dd></div>
                    <div><dt>Reboot</dt><dd>${escapeHtml(rebootText)}</dd></div>
                    <div><dt>Disk</dt><dd>${escapeHtml(`${health.disk_status || "unknown"} · ${formatDiskCapacity(health.disk_free_kb, health.disk_total_kb)}`)}</dd></div>
                    <div><dt>APT</dt><dd>${escapeHtml(health.apt_status || "unknown")}</dd></div>
                    <div><dt>Facts</dt><dd>${escapeHtml(triage.facts_state || "stale")} · ${escapeHtml(factsAge)}</dd></div>
                </dl>
                <details class="inspector-more facts-more" data-name="${safeDataName}" ${factsMoreOpen ? "open" : ""}>
                    <summary>More host facts</summary>
                    <dl class="host-facts host-facts-secondary">
                        <div><dt>Host</dt><dd>${escapeHtml(server.host || "-")}</dd></div>
                        <div><dt>User</dt><dd>${escapeHtml(server.user || "-")}</dd></div>
                        <div><dt>Port</dt><dd>${escapeHtml(String(server.port || 22))}</dd></div>
                        <div><dt>Auth</dt><dd>${escapeHtml(getAuthLabel(server))}</dd></div>
                        <div><dt>Tags</dt><dd><div class="chip-list">${renderServerTags(server)}</div></dd></div>
                        <div><dt>Timeline</dt><dd>${escapeHtml(timeline.summary || "No maintenance activity")}</dd></div>
                        <div><dt>Uptime</dt><dd>${escapeHtml(formatUptime(health.uptime_seconds))}</dd></div>
                        <div><dt>Last update</dt><dd>${escapeHtml(lastUpdateSummary)}</dd></div>
                        <div><dt>Avg duration</dt><dd>${escapeHtml(intelligence?.duration_samples ? formatDuration(intelligence.avg_duration_ms) : "No samples")}</dd></div>
                        <div><dt>Last failure</dt><dd>${escapeHtml(lastFailed ? `${formatRelativeTimestamp(lastFailed.finished_at)} · ${lastFailed.failure_cause || "failure"}` : "No failed update")}</dd></div>
                        <div><dt>Next run</dt><dd>${escapeHtml(nextRunSummary)}</dd></div>
                        <div><dt>No-run</dt><dd>${escapeHtml(noRun.summary || "No no-run window active")}</dd></div>
                    </dl>
                </details>
            `;
        }

        function renderDashboardPanels() {
            const activeServers = allServers.filter(server => activeStatuses.has(server.status) || isRunningTimelineState(getServerTimeline(server).state));
            const failedServers = allServers.filter(server => isFailedServer(server) || getServerTimeline(server).state === "error");
            setText("active-operations-count", String(activeServers.length));
            setText("failed-hosts-count", String(failedServers.length));
            renderMiniServerList("active-operations", activeServers, "No active runs.", {
                limit: 1,
                expandable: true,
                lessLabel: "Show fewer running operations",
                moreLabel: hidden => `+${hidden} more running`
            });
            renderMiniServerList("failed-hosts-panel", failedServers, "No failures.", {
                limit: 1,
                expandable: true,
                lessLabel: "Show fewer failures",
                moreLabel: hidden => `+${hidden} more failure${hidden === 1 ? "" : "s"}`
            });
            renderScheduledRuns();
            renderApprovalTriage();
            renderFleetFilters();
            renderTagSummary();
            renderRecentActivity();
            renderIntelligenceLists();
            renderSummaryBadges();
            renderSelectedHostPanel();
            renderSyncState();
        }

        async function fetchRecentActivity() {
            try {
                const response = await fetch('/api/audit-events?page=1&page_size=8');
                if (!response.ok) throw new Error(`HTTP ${response.status}`);
                const data = await response.json();
                recentActivity = Array.isArray(data?.items) ? data.items : [];
                setDashboardExtraError("audit", null);
            } catch (err) {
                recentActivity = [];
                setDashboardExtraError("audit", err);
            }
            renderRecentActivity();
        }

        async function fetchObservabilitySummary() {
            try {
                const response = await fetch('/api/observability/summary?window=7d');
                if (!response.ok) throw new Error(`HTTP ${response.status}`);
                observabilitySummary = await response.json();
                setDashboardExtraError("observability", null);
            } catch (err) {
                observabilitySummary = null;
                setDashboardExtraError("observability", err);
            }
            renderSummaryBadges();
        }

        async function fetchPolicySummary() {
            try {
                const response = await fetch('/api/update-policies');
                if (!response.ok) throw new Error(`HTTP ${response.status}`);
                const data = await response.json();
                policySummary = Array.isArray(data) ? data : (Array.isArray(data?.items) ? data.items : []);
                setDashboardExtraError("policies", null);
            } catch (err) {
                policySummary = null;
                setDashboardExtraError("policies", err);
            }
            renderSummaryBadges();
        }

        async function fetchDashboardSummary() {
            try {
                const response = await fetch('/api/dashboard/summary?window=7d');
                if (!response.ok) throw new Error(`HTTP ${response.status}`);
                dashboardSummary = await response.json();
                const items = Array.isArray(dashboardSummary?.servers) ? dashboardSummary.servers : [];
                dashboardByServer = new Map(items.map(item => [item.name, item]));
                setDashboardExtraError("dashboard", null);
            } catch (err) {
                dashboardSummary = null;
                dashboardByServer = new Map();
                setDashboardExtraError("dashboard", err);
            }
            renderDashboardMetrics();
            renderDashboardPanels();
            if (allServers.length > 0) {
                renderTable();
            }
        }

        async function fetchGlobalKeyStatus() {
            try {
                const response = await fetch('/api/keys/global');
                if (!response.ok) throw new Error(`HTTP ${response.status}`);
                const data = await response.json();
                const nextGlobalKeyAvailable = !!data?.has_key;
                if (nextGlobalKeyAvailable !== globalKeyAvailable) {
                    globalKeyAvailable = nextGlobalKeyAvailable;
                    renderDashboardMetrics();
                    if (allServers.length > 0) {
                        renderTable();
                        renderDrawer();
                    }
                } else {
                    globalKeyAvailable = nextGlobalKeyAvailable;
                }
                setDashboardExtraError("global key", null);
            } catch (err) {
                setDashboardExtraError("global key", err);
            }
        }

        function fetchDashboardExtras() {
            fetchGlobalKeyStatus();
            fetchRecentActivity();
            fetchObservabilitySummary();
            fetchPolicySummary();
            fetchDashboardSummary();
        }

        function configurePolling(serverMs, extrasMs) {
            if (serverPollIntervalID !== null) {
                clearInterval(serverPollIntervalID);
            }
            if (dashboardExtrasIntervalID !== null) {
                clearInterval(dashboardExtrasIntervalID);
            }
            serverPollIntervalID = setInterval(fetchServers, serverMs);
            dashboardExtrasIntervalID = setInterval(fetchDashboardExtras, extrasMs);
        }

        function scheduleDashboardEventReconnect() {
            if (dashboardEventReconnectTimer !== null) return;
            const delay = dashboardEventReconnectDelay;
            dashboardEventReconnectDelay = Math.min(dashboardEventReconnectDelay * 2, 30000);
            dashboardEventReconnectTimer = setTimeout(() => {
                dashboardEventReconnectTimer = null;
                connectDashboardEvents();
            }, delay);
        }

        function connectDashboardEvents() {
            if (!window.EventSource) {
                configurePolling(fallbackServerPollMs, fallbackExtrasPollMs);
                return;
            }
            if (dashboardEventSource) {
                dashboardEventSource.close();
            }
            const source = new EventSource('/api/dashboard/events');
            dashboardEventSource = source;
            source.addEventListener('open', () => {
                dashboardEventReconnectDelay = 1000;
                configurePolling(eventBackedServerPollMs, eventBackedExtrasPollMs);
                renderSyncState();
            });
            source.addEventListener('dashboard', () => {
                fetchServers(true);
                fetchDashboardExtras();
            });
            source.addEventListener('error', () => {
                if (dashboardEventSource === source) {
                    dashboardEventSource = null;
                }
                source.close();
                configurePolling(fallbackServerPollMs, fallbackExtrasPollMs);
                renderSyncState();
                scheduleDashboardEventReconnect();
            });
        }

        function saveWindowScroll() {
            return { x: window.scrollX, y: window.scrollY };
        }

        function restoreWindowScroll(pos) {
            if (!pos) return;
            window.scrollTo(pos.x, pos.y);
        }

        function isActionInteractionActive() {
            return actionInteractionDepth > 0 || actionInteractionReleaseTimer !== null;
        }

        function renderServerState() {
            const pageScroll = saveWindowScroll();
            renderDashboardMetrics();
            renderTable();
            renderDrawer();
            requestAnimationFrame(() => restoreWindowScroll(pageScroll));
        }

        function flushDeferredServerRender() {
            if (!deferredServerRender || isActionInteractionActive()) return;
            deferredServerRender = false;
            renderServerState();
        }

        function renderServerStateWhenSafe(forceRender = false) {
            if (!forceRender && isActionInteractionActive()) {
                deferredServerRender = true;
                return;
            }
            deferredServerRender = false;
            renderServerState();
        }

        function beginActionInteraction() {
            actionInteractionDepth += 1;
            if (actionInteractionReleaseTimer !== null) {
                clearTimeout(actionInteractionReleaseTimer);
                actionInteractionReleaseTimer = null;
            }
        }

        function endActionInteraction() {
            if (actionInteractionDepth > 0) {
                actionInteractionDepth -= 1;
            }
            if (actionInteractionDepth > 0) return;
            if (actionInteractionReleaseTimer !== null) {
                clearTimeout(actionInteractionReleaseTimer);
            }
            actionInteractionReleaseTimer = setTimeout(() => {
                actionInteractionReleaseTimer = null;
                flushDeferredServerRender();
            }, actionInteractionDeferMs);
        }

        function resetActionInteraction() {
            actionInteractionDepth = 0;
            if (actionInteractionReleaseTimer !== null) {
                clearTimeout(actionInteractionReleaseTimer);
                actionInteractionReleaseTimer = null;
            }
            flushDeferredServerRender();
        }

        function isServerActionControl(target) {
            return !!target?.closest?.([
                'button[data-action]',
                '#bulk-update',
                '#bulk-approve',
                '#bulk-cancel',
                '#bulk-autoremove',
                '#drawer-approve-all',
                '#drawer-approve-security',
                '#drawer-approve-security-kept-back',
                '#drawer-approve-full'
            ].join(','));
        }

        function getTableColByKey(key) {
            return document.querySelector(`#servers-table col[data-col-key="${key}"]`);
        }

        function getTableColumnIndexByKey(key) {
            const table = document.getElementById('servers-table');
            if (!table) return -1;
            return Array.from(table.querySelectorAll('col')).findIndex(col => col.dataset.colKey === key);
        }

        function getRenderedTableColumnWidths(table) {
            const headerCells = Array.from(table?.tHead?.rows?.[0]?.cells || []);
            const cols = Array.from(table?.querySelectorAll('col') || []);
            return cols.map((col, index) => {
                const headerWidth = headerCells[index]?.getBoundingClientRect().width || 0;
                const colWidth = col.getBoundingClientRect().width || 0;
                return Math.max(1, Math.round(headerWidth || colWidth));
            });
        }

        function freezeRenderedTableWidths(table, widths) {
            const cols = Array.from(table?.querySelectorAll('col') || []);
            const totalWidth = widths.reduce((sum, width) => sum + width, 0);
            cols.forEach((col, index) => {
                if (widths[index]) {
                    col.style.width = `${widths[index]}px`;
                }
            });
            if (totalWidth > 0) {
                table.style.width = `${totalWidth}px`;
                table.style.minWidth = `${totalWidth}px`;
            }
        }

        function updateSortIndicators() {
            document.querySelectorAll('#servers-table th.sortable').forEach((th) => {
                if (th.dataset.sortKey === sortKey) {
                    th.dataset.sortDir = sortDir;
                    th.setAttribute('aria-sort', sortDir === "asc" ? "ascending" : "descending");
                    const indicator = th.querySelector('.sort-indicator');
                    if (indicator) indicator.textContent = sortDir === "asc" ? "▲" : "▼";
                } else {
                    delete th.dataset.sortDir;
                    th.setAttribute('aria-sort', 'none');
                    const indicator = th.querySelector('.sort-indicator');
                    if (indicator) indicator.textContent = "";
                }
            });
        }

        function loadColumnWidths() {
            try {
                const raw = localStorage.getItem(columnResizeStorageKey);
                if (!raw) return {};
                const parsed = JSON.parse(raw);
                return parsed && typeof parsed === "object" ? parsed : {};
            } catch (_) {
                return {};
            }
        }

        function saveColumnWidths(widths) {
            try {
                localStorage.setItem(columnResizeStorageKey, JSON.stringify(widths));
            } catch (_) {
                // Ignore storage failures (private mode, quota, etc.)
            }
        }

        function applyColumnWidths(widths) {
            if (!widths || Object.keys(widths).length === 0) return;
            Object.keys(defaultColumnWidths).forEach((key) => {
                const col = getTableColByKey(key);
                if (!col) return;
                const configured = Number(widths[key]);
                const minWidth = minColumnWidths[key] || 100;
                const maxWidth = maxColumnWidths[key] || 9999;
                const fallback = defaultColumnWidths[key];
                const boundedFallback = Math.min(maxWidth, Math.max(minWidth, fallback));
                const nextWidth = Number.isFinite(configured)
                    ? Math.min(maxWidth, Math.max(minWidth, configured))
                    : boundedFallback;
                col.style.width = `${Math.round(nextWidth)}px`;
            });
        }

        function initColumnResizing() {
            const savedWidths = loadColumnWidths();
            applyColumnWidths(savedWidths);

            document.querySelectorAll('#servers-table .col-resize-handle').forEach((handle) => {
                if (handle.dataset.bound === "1") return;
                handle.dataset.bound = "1";

                handle.addEventListener('pointerdown', (event) => {
                    event.preventDefault();
                    event.stopPropagation();

                    const colKey = handle.dataset.colKey || "";
                    const col = getTableColByKey(colKey);
                    const th = handle.closest('th');
                    const table = document.getElementById('servers-table');
                    const colIndex = getTableColumnIndexByKey(colKey);
                    if (!col || !th || !table || colIndex < 0) return;

                    const minWidth = minColumnWidths[colKey] || 100;
                    const maxWidth = maxColumnWidths[colKey] || 9999;
                    const startX = event.clientX;
                    const startWidths = getRenderedTableColumnWidths(table);
                    const startTableWidth = startWidths.reduce((sum, width) => sum + width, 0);
                    const startWidth = Math.max(minWidth, Math.round(startWidths[colIndex] || col.getBoundingClientRect().width));
                    const nextWidths = startWidths.slice();
                    freezeRenderedTableWidths(table, startWidths);

                    const onPointerMove = (moveEvent) => {
                        const delta = moveEvent.clientX - startX;
                        const nextWidth = Math.min(maxWidth, Math.max(minWidth, startWidth + delta));
                        nextWidths[colIndex] = Math.round(nextWidth);
                        col.style.width = `${Math.round(nextWidth)}px`;
                        const nextTableWidth = startTableWidth - startWidth + nextWidth;
                        table.style.width = `${Math.round(nextTableWidth)}px`;
                        table.style.minWidth = `${Math.round(nextTableWidth)}px`;
                    };

                    const finishResize = (endEvent, canceled) => {
                        window.removeEventListener('pointermove', onPointerMove);
                        window.removeEventListener('pointerup', onPointerUp);
                        window.removeEventListener('pointercancel', onPointerCancel);
                        document.body.classList.remove('col-resizing');
                        th.classList.remove('resizing');

                        if (canceled) {
                            freezeRenderedTableWidths(table, startWidths);
                        } else {
                            const finalWidth = Math.min(maxWidth, Math.max(minWidth, Math.round(nextWidths[colIndex] || col.getBoundingClientRect().width)));
                            const savedWidths = loadColumnWidths();
                            savedWidths[colKey] = finalWidth;
                            saveColumnWidths(savedWidths);
                            suppressSortClickUntil = Date.now() + 250;
                        }

                        if (handle.releasePointerCapture && endEvent.pointerId !== undefined) {
                            try {
                                handle.releasePointerCapture(endEvent.pointerId);
                            } catch (_) {
                                // Ignore pointer release issues.
                            }
                        }
                    };

                    const onPointerUp = (endEvent) => finishResize(endEvent, false);
                    const onPointerCancel = (endEvent) => finishResize(endEvent, true);

                    document.body.classList.add('col-resizing');
                    th.classList.add('resizing');
                    if (handle.setPointerCapture && event.pointerId !== undefined) {
                        try {
                            handle.setPointerCapture(event.pointerId);
                        } catch (_) {
                            // Ignore pointer capture issues.
                        }
                    }

                    window.addEventListener('pointermove', onPointerMove);
                    window.addEventListener('pointerup', onPointerUp);
                    window.addEventListener('pointercancel', onPointerCancel);
                });
            });
        }

        async function fetchServers(forceRender = false) {
            if (fetchInFlight) {
                fetchQueued = true;
                queuedForceRender = queuedForceRender || forceRender;
                return;
            }
            fetchInFlight = true;
            try {
                let parsedServers;
                try {
                    const response = await fetch('/api/servers');
                    if (!response.ok) {
                        throw new Error(`Failed to fetch servers: HTTP ${response.status}`);
                    }
                    parsedServers = await response.json();
                    if (!Array.isArray(parsedServers)) {
                        throw new Error('Invalid servers payload: expected an array');
                    }
                } catch (err) {
                    console.error('Unable to refresh servers list:', err);
                    lastFetchError = err;
                    renderSyncState();
                    return;
                }
                allServers = parsedServers;
                lastFetchError = null;
                lastSuccessfulSyncAt = new Date();
                renderServerStateWhenSafe(forceRender);
            } finally {
                fetchInFlight = false;
                if (fetchQueued) {
                    fetchQueued = false;
                    const nextRender = queuedForceRender;
                    queuedForceRender = false;
                    fetchServers(nextRender);
                }
            }
        }

        function sortServers(servers) {
            const dir = sortDir === "asc" ? 1 : -1;
            return servers.slice().sort((a, b) => {
                const aVal = (a[sortKey] || "").toString().toLowerCase();
                const bVal = (b[sortKey] || "").toString().toLowerCase();
                if (aVal < bVal) return -1 * dir;
                if (aVal > bVal) return 1 * dir;
                return 0;
            });
        }

        function applyFilters(servers) {
            const search = document.getElementById('search').value.trim().toLowerCase();
            const statusFilter = document.getElementById('status-filter').value;
            const authFilter = document.getElementById('auth-filter').value;
            return servers.filter(server => {
                if (statusFilter && server.status !== statusFilter) return false;
                if (fleetTagFilter) {
                    const tags = Array.isArray(server.tags) && server.tags.length ? server.tags : ["untagged"];
                    if (!tags.includes(fleetTagFilter)) return false;
                }
                if (fleetQuickFilter === "pending_approval" && server.status !== "pending_approval") return false;
                if (fleetQuickFilter === "active" && !activeStatuses.has(server.status) && !isRunningTimelineState(getServerTimeline(server).state)) return false;
                if (fleetQuickFilter === "stale_facts" && getServerApprovalTriage(server).facts_state !== "stale") return false;
                if (fleetQuickFilter === "high_risk" && getServerApprovalTriage(server).cve_count <= 0 && getRiskLevel(server) !== "critical") return false;
                if (authFilter === "password" && !server.has_password) return false;
                if (authFilter === "key" && !hasEffectiveKey(server)) return false;
                if (!search) return true;
                const haystack = [
                    server.name,
                    server.host,
                    server.port ? server.port.toString() : "",
                    server.user,
                    (server.tags || []).join(" ")
                ].join(" ").toLowerCase();
                return haystack.includes(search);
            });
        }

        function paginate(servers) {
            const size = parseInt(document.getElementById('page-size').value, 10);
            const totalPages = Math.max(1, Math.ceil(servers.length / size));
            page = Math.min(page, totalPages);
            const start = (page - 1) * size;
            const end = start + size;
            const pageInfo = document.getElementById('page-info');
            const pagination = document.querySelector('.pagination');
            pageInfo.textContent = `Page ${page} of ${totalPages} (${pluralize(servers.length, "host")})`;
            pagination?.classList.toggle('single-page', totalPages <= 1);
            document.getElementById('prev-page').disabled = page <= 1;
            document.getElementById('next-page').disabled = page >= totalPages;
            return servers.slice(start, end);
        }

        function groupServers(servers) {
            const groupBy = document.getElementById('group-by').value;
            if (!groupBy) return [{ key: "", items: servers }];
            const groups = new Map();
            if (groupBy === "status") {
                servers.forEach(server => {
                    const key = server.status || "unknown";
                    if (!groups.has(key)) groups.set(key, []);
                    groups.get(key).push(server);
                });
            } else if (groupBy === "tag") {
                servers.forEach(server => {
                    const tags = server.tags && server.tags.length ? server.tags : ["untagged"];
                    tags.forEach(tag => {
                        if (!groups.has(tag)) groups.set(tag, []);
                        groups.get(tag).push(server);
                    });
                });
            }
            return Array.from(groups.entries()).map(([key, items]) => ({ key, items }));
        }

        function loadDashboardFilters() {
            try {
                const raw = localStorage.getItem(dashboardFilterStorageKey);
                if (!raw) return;
                const saved = JSON.parse(raw);
                if (!saved || typeof saved !== "object") return;
                restoreTextInputValue("search", saved.search, "");
                restoreSelectValue("status-filter", saved.statusFilter, "");
                restoreSelectValue("auth-filter", saved.authFilter, "");
                restoreSelectValue("group-by", saved.groupBy, "");
                restorePageSizeValue(saved.pageSize);
                selectedServerName = typeof saved.selectedServerName === "string" ? saved.selectedServerName : "";
            } catch (_) {
                // Ignore invalid saved dashboard state.
            }
        }

        function restoreTextInputValue(id, value, fallback) {
            const el = document.getElementById(id);
            if (!el || el.tagName !== "INPUT") return;
            el.value = typeof value === "string" && value.length <= 200 ? value : fallback;
        }

        function restoreSelectValue(id, value, fallback) {
            const el = document.getElementById(id);
            if (!el || el.tagName !== "SELECT") return;
            const optionValues = Array.from(el.options).map(option => option.value);
            const normalized = value === undefined || value === null ? fallback : String(value);
            el.value = optionValues.includes(normalized) ? normalized : fallback;
        }

        function restorePageSizeValue(value) {
            const el = document.getElementById("page-size");
            if (!el || el.tagName !== "SELECT") return;
            const fallback = Array.from(el.options).some(option => option.value === "25") ? "25" : (el.options[0]?.value || "");
            const parsed = parseInt(value, 10);
            const normalized = Number.isFinite(parsed) && parsed > 0 ? String(parsed) : fallback;
            restoreSelectValue("page-size", normalized, fallback);
        }

        function saveDashboardFilters() {
            try {
                localStorage.setItem(dashboardFilterStorageKey, JSON.stringify({
                    search: document.getElementById('search')?.value || "",
                    statusFilter: document.getElementById('status-filter')?.value || "",
                    authFilter: document.getElementById('auth-filter')?.value || "",
                    groupBy: document.getElementById('group-by')?.value || "",
                    pageSize: document.getElementById('page-size')?.value || "25",
                    selectedServerName
                }));
            } catch (_) {
                // Ignore storage failures.
            }
        }

        function openDrawer(name, tab = "logs") {
            const nextTab = tab === "pending" ? "pending" : "logs";
            if (drawerServerName !== name) {
                drawerLogFollow = true;
                drawerLogScrollTop = 0;
                drawerPendingScrollTop = 0;
            }
            if (!drawerOpen) {
                drawerPreviousFocus = document.activeElement;
            }
            drawerOpen = true;
            drawerServerName = name;
            drawerTab = nextTab;
            document.body.classList.add('drawer-open');
            renderDrawer();
            window.setTimeout(() => {
                const drawer = document.getElementById('status-drawer');
                if (!drawerOpen || !drawer) return;
                const focusable = drawerFocusableElements(drawer);
                const target = focusable[0] || drawer;
                if (target && typeof target.focus === 'function') {
                    target.focus({ preventScroll: true });
                }
            }, 0);
        }

        function closeDrawer() {
            drawerOpen = false;
            const drawer = document.getElementById('status-drawer');
            const backdrop = document.getElementById('status-drawer-backdrop');
            drawer.classList.remove('open');
            backdrop.classList.remove('open');
            drawer.setAttribute('aria-hidden', 'true');
            document.body.classList.remove('drawer-open');
            const previous = drawerPreviousFocus;
            drawerPreviousFocus = null;
            if (previous && document.contains(previous) && typeof previous.focus === 'function') {
                window.setTimeout(() => previous.focus({ preventScroll: true }), 0);
            }
        }

        function setDrawerTab(tab) {
            if (tab !== "logs" && tab !== "pending") return;
            drawerTab = tab;
            renderDrawer();
        }

        function renderDrawer() {
            const drawer = document.getElementById('status-drawer');
            const backdrop = document.getElementById('status-drawer-backdrop');
            const title = document.getElementById('status-drawer-title');
            const statusContainer = document.getElementById('status-drawer-status');
            const logsTabBtn = document.getElementById('drawer-tab-logs');
            const pendingTabBtn = document.getElementById('drawer-tab-pending');
            const logsPanel = document.getElementById('drawer-panel-logs');
            const pendingPanel = document.getElementById('drawer-panel-pending');
            const logsHint = document.getElementById('drawer-logs-hint');
            const logsEl = document.getElementById('drawer-logs');
            const approvalActions = document.getElementById('status-drawer-approval-actions');
            const drawerApproveAllBtn = document.getElementById('drawer-approve-all');
            const drawerApproveSecurityBtn = document.getElementById('drawer-approve-security');
            const drawerApproveKeptBackSecurityBtn = document.getElementById('drawer-approve-security-kept-back');
            const drawerApproveFullBtn = document.getElementById('drawer-approve-full');

            if (!drawerOpen) {
                drawer.classList.remove('open');
                backdrop.classList.remove('open');
                drawer.setAttribute('aria-hidden', 'true');
                return;
            }

            const server = getServerByName(drawerServerName);
            if (!server) {
                closeDrawer();
                return;
            }

            const safeStatus = safeStatusClass(server.status);
            const safeStatusText = escapeHtml(server.status || "unknown");
            const isPendingApproval = server.status === "pending_approval";
            const hasPending = hasPendingUpdates(server);
            const triage = getServerApprovalTriage(server);
            const approvalCounts = getPendingApprovalCounts(server);
            const keptBackSecurityCount = Number(triage.kept_back_security_updates ?? approvalCounts.keptBackSecurity ?? 0);
            const canApproveKeptBackSecurity = server.status === "pending_approval" && keptBackSecurityCount > 0 && approvalCounts.keptBackSecurityPlanAvailable;
            const securityApprovalLabel = approvalCounts.security === null
                ? "Standard security (?)"
                : `Standard security (${approvalCounts.security})`;
            if (drawerTab === "pending" && !hasPending) {
                drawerTab = "logs";
                drawerPendingScrollTop = 0;
            }

            title.textContent = server.name || "Server details";
            statusContainer.innerHTML = `<span class="status-pill status-${safeStatus}">${safeStatusText}</span>`;
            approvalActions.classList.toggle('hidden', !isPendingApproval);
            drawerApproveAllBtn.textContent = `Approve (${approvalCounts.standard})`;
            drawerApproveAllBtn.disabled = approvalCounts.standard <= 0;
            drawerApproveSecurityBtn.textContent = securityApprovalLabel;
            drawerApproveSecurityBtn.disabled = !approvalCounts.security || approvalCounts.security <= 0;
            drawerApproveKeptBackSecurityBtn.textContent = `Kept-back security (${keptBackSecurityCount})`;
            drawerApproveKeptBackSecurityBtn.disabled = !canApproveKeptBackSecurity;
            drawerApproveKeptBackSecurityBtn.classList.toggle('hidden', !canApproveKeptBackSecurity);
            drawerApproveFullBtn.textContent = `Full upgrade (${approvalCounts.full})`;
            drawerApproveFullBtn.classList.toggle('hidden', !triage.can_approve_full);

            pendingTabBtn.disabled = !hasPending;
            pendingTabBtn.classList.toggle('active', drawerTab === "pending");
            logsTabBtn.classList.toggle('active', drawerTab === "logs");

            logsPanel.classList.toggle('active', drawerTab === "logs");
            pendingPanel.classList.toggle('active', drawerTab === "pending");

            if (drawerTab === "logs") {
                logsEl.innerHTML = formatLogsHtml(server.logs || "");
                if (drawerLogFollow) {
                    logsEl.scrollTop = logsEl.scrollHeight;
                } else {
                    logsEl.scrollTop = drawerLogScrollTop;
                }
                logsHint.textContent = drawerLogFollow ? "Live auto-scroll" : "Auto-scroll paused";
            }

            if (drawerTab === "pending") {
                const pendingScrollTop = drawerPendingScrollTop;
                pendingPanel.innerHTML = renderPendingUpdatesHtml(server, true);
                requestAnimationFrame(() => {
                    const maxScrollTop = Math.max(0, pendingPanel.scrollHeight - pendingPanel.clientHeight);
                    pendingPanel.scrollTop = Math.min(pendingScrollTop, maxScrollTop);
                });
            } else {
                pendingPanel.innerHTML = "";
            }

            drawer.classList.add('open');
            backdrop.classList.add('open');
            drawer.setAttribute('aria-hidden', 'false');
        }

        function renderTable() {
            const tbody = document.querySelector('#servers-table tbody');
            tbody.innerHTML = '';
            let servers = applyFilters(allServers);
            servers = sortServers(servers);
            const totalFiltered = servers.length;
            if (servers.length > 0 && !servers.some(server => server.name === selectedServerName)) {
                selectedServerName = servers[0].name;
            } else if (servers.length === 0) {
                selectedServerName = "";
            }
            const paged = paginate(servers);
            setText(
                "table-summary",
                allServers.length === 0
                    ? "Waiting for status data"
                    : `${pluralize(totalFiltered, "host")} visible · ${pluralize(allServers.length, "host")} loaded`
            );
            const groups = groupServers(paged);
            groups.forEach(group => {
                if (group.key) {
                    const groupRow = document.createElement('tr');
                    groupRow.className = 'group-row';
                    groupRow.innerHTML = `<td colspan="11">${escapeHtml(group.key)}</td>`;
                    tbody.appendChild(groupRow);
                }
                group.items.forEach(server => {
                    const row = document.createElement('tr');
                    row.dataset.name = server.name;
                    const rowSelected = selectedServerName === server.name;
                    row.setAttribute("aria-selected", rowSelected ? "true" : "false");
                    if (rowSelected) {
                        row.classList.add('row-selected');
                    }
                    if (hoveredName === server.name) {
                        row.classList.add('row-hover');
                    }
                    const isBusy = activeStatuses.has(server.status);
                    const safeNameHtml = escapeHtml(server.name);
                    const safeStatusText = escapeHtml(statusLabel(server.status));
                    const safeStatus = safeStatusClass(server.status);
                    const safeDataName = escapeHtml(server.name);
                    const intelligence = getServerIntelligence(server.name);
                    const timeline = getServerTimeline(server);
                    const triage = getServerApprovalTriage(server);
                    const lastUpdate = intelligence?.last_update;
                    const nextRun = intelligence?.next_run;
                    const lastUpdateLabel = lastUpdate ? `${formatRelativeTimestamp(lastUpdate.finished_at)} · ${formatDuration(lastUpdate.duration_ms)}` : "No history";
                    const nextRunLabel = nextRun?.state === "scheduled"
                        ? (nextRun.scheduled_for_display || nextRun.scheduled_for_utc || "Scheduled")
                        : "None";
                    const timelineWindow = timeline?.updated_at_display || timeline?.updated_at || (nextRun?.state === "scheduled" ? nextRunLabel : lastUpdateLabel);
                    const approvalCounts = getPendingApprovalCounts(server);
                    const keptBackSecurityCount = Number(triage.kept_back_security_updates ?? approvalCounts.keptBackSecurity ?? 0);
                    const canApproveKeptBackSecurity = server.status === "pending_approval" && keptBackSecurityCount > 0 && approvalCounts.keptBackSecurityPlanAvailable;
                    const securityApprovalLabel = approvalCounts.security === null
                        ? "Std sec (?)"
                        : `Std sec (${approvalCounts.security})`;
                    const keptBackSecurityButton = canApproveKeptBackSecurity
                        ? `<button type="button" class="btn-security" data-action="approve-security-kept-back" data-name="${safeDataName}" title="Approve only kept-back security updates">Kept sec (${keptBackSecurityCount})</button>`
                        : "";
                    const fullApprovalButton = triage.can_approve_full
                        ? `<button type="button" class="btn-full-upgrade" data-action="approve-full" data-name="${safeDataName}" title="Run apt full-upgrade">Full upgrade (${approvalCounts.full})</button>`
                        : "";
                    const actionButtons = server.status === 'pending_approval'
                        ? `
                            <div class="actions-grid timeline-actions">
                                <button type="button" data-action="approve-all" data-name="${safeDataName}" title="Approve standard updates">Approve (${approvalCounts.standard})</button>
                                <button type="button" class="btn-security" data-action="approve-security" data-name="${safeDataName}" title="Approve only standard security updates">${securityApprovalLabel}</button>
                                ${keptBackSecurityButton}
                                ${fullApprovalButton}
                                <button type="button" class="btn-ghost" data-action="open-drawer" data-name="${safeDataName}" data-tab="pending">Packages</button>
                            </div>
                          `
                        : isBusy
                            ? `
                                <div class="actions-grid timeline-actions">
                                    <button type="button" class="btn-ghost action-span" data-action="open-drawer" data-name="${safeDataName}" data-tab="logs">Logs</button>
                                </div>
                              `
                        : `
                            <div class="actions-grid timeline-actions">
                                <button type="button" data-action="update-server" data-name="${safeDataName}">Update</button>
                                <button type="button" class="btn-ghost" data-action="open-drawer" data-name="${safeDataName}" data-tab="logs">Logs</button>
                            </div>
                          `;
                    row.innerHTML = `
                        <td class="select-col"><input type="checkbox" class="row-select" data-name="${safeDataName}" ${selectedServers.has(server.name) ? "checked" : ""}></td>
                        <td class="name-cell" title="${safeNameHtml}">
                            <button type="button" class="select-host" data-select-host="${safeDataName}" aria-pressed="${rowSelected ? 'true' : 'false'}">${safeNameHtml}</button>
                            <span class="server-subline">${escapeHtml((server.tags || []).join(", ") || "ungrouped")}</span>
                        </td>
                        <td class="status-col">
                            <span class="status-pill status-${safeStatus}">${safeStatusText}</span>
                            <span class="stage-progress" aria-label="${escapeHtml(`${timeline.current_label || "Idle"} ${timeline.progress_pct || 0}%`)}"><span class="${progressClass(timeline.progress_pct)}"></span></span>
                        </td>
                        <td class="phase-col">${timelinePhaseCell(server, "pending_approval")}</td>
                        <td class="phase-col">${timelinePhaseCell(server, "prechecks")}</td>
                        <td class="phase-col">${timelinePhaseCell(server, "apt_update")}</td>
                        <td class="phase-col">${timelinePhaseCell(server, "upgrade")}</td>
                        <td class="phase-col">${timelinePhaseCell(server, "postchecks")}</td>
                        <td class="phase-col">${timelinePhaseCell(server, "done_error")}</td>
                        <td class="timeline-summary-col">
                            <strong>${escapeHtml(timeline.current_label || "Idle")}</strong>
                            <span>${escapeHtml(timeline.summary || timelineWindow || "No activity")}</span>
                            <span>${escapeHtml(`${Number(triage.pending_packages || 0)} pkg · ${Number(triage.kept_back_packages || 0)} kept · ${Number(triage.security_updates || 0)} sec · ${Number(triage.cve_count || 0)} CVE`)}</span>
                        </td>
                        <td class="actions-col">${actionButtons}</td>
                    `;
                    tbody.appendChild(row);
                });
            });
            applyHoverClass();
            tbody.querySelectorAll('.row-select').forEach(cb => {
                cb.addEventListener('change', (e) => {
                    const name = e.target.dataset.name;
                    if (e.target.checked) {
                        selectedServers.add(name);
                    } else {
                        selectedServers.delete(name);
                    }
                });
            });
            document.getElementById('select-all').checked = paged.length > 0 && paged.every(s => selectedServers.has(s.name));
            updateSortIndicators();
            renderDashboardPanels();
        }

        function getServerByName(name) {
            return allServers.find(server => server.name === name);
        }

        function selectServer(name) {
            selectedServerName = name || "";
            saveDashboardFilters();
            renderTable();
        }

        async function copyLogs(name = drawerServerName) {
            const server = getServerByName(name);
            const logs = String(server?.logs || "");
            try {
                await navigator.clipboard.writeText(logs);
            } catch (_) {
                const tmp = document.createElement('textarea');
                tmp.value = logs;
                document.body.appendChild(tmp);
                tmp.select();
                document.execCommand('copy');
                tmp.remove();
            }
        }

        function downloadLogs(name = drawerServerName) {
            const server = getServerByName(name);
            const logs = String(server?.logs || "");
            const blob = new Blob([logs], { type: 'text/plain;charset=utf-8' });
            const url = URL.createObjectURL(blob);
            const link = document.createElement('a');
            const safeName = String(name || "server").replace(/[^a-zA-Z0-9._-]/g, '_');
            link.href = url;
            link.download = `${safeName}-logs.txt`;
            document.body.appendChild(link);
            link.click();
            link.remove();
            URL.revokeObjectURL(url);
        }

        function applyHoverClass() {
            const tbody = document.querySelector('#servers-table tbody');
            tbody.querySelectorAll('tr').forEach((tr) => {
                tr.classList.remove('row-hover');
            });
            if (!hoveredName) return;
            const row = tbody.querySelector(`tr[data-name="${CSS.escape(hoveredName)}"]`);
            if (row) {
                row.classList.add('row-hover');
            }
        }

        function handleServerAction(action, name, tab = "logs") {
            if (!name) return;
            if (action === "open-drawer") {
                openDrawer(name, tab || "logs");
                return;
            }
            if (action === "approve-all") {
                approveAllUpdates(name);
                return;
            }
            if (action === "approve-security") {
                approveSecurityUpdates(name);
                return;
            }
            if (action === "approve-security-kept-back") {
                approveKeptBackSecurityUpdates(name);
                return;
            }
            if (action === "approve-full") {
                approveFullUpgrade(name);
                return;
            }
            if (action === "cancel-upgrade") {
                cancelUpgrade(name);
                return;
            }
            if (action === "update-server") {
                updateServer(name);
                return;
            }
            if (action === "run-autoremove") {
                runAutoremove(name);
                return;
            }
            if (action === "enable-apt") {
                enablePasswordlessApt(name);
                return;
            }
            if (action === "disable-apt") {
                disablePasswordlessApt(name);
                return;
            }
            if (action === "refresh-facts") {
                refreshHostFacts(name);
            }
        }

        const tbodyHover = document.querySelector('#servers-table tbody');
        tbodyHover.addEventListener('click', (e) => {
            const button = e.target.closest('button[data-action]');
            if (button) {
                handleServerAction(button.dataset.action || "", button.dataset.name || "", button.dataset.tab || "logs");
                return;
            }
            const selectHostButton = e.target.closest('button[data-select-host]');
            if (selectHostButton) {
                selectServer(selectHostButton.dataset.selectHost || "");
                return;
            }
            if (e.target.closest('button, input, select, textarea, a, label')) return;
            const row = e.target.closest('tr[data-name]');
            if (row) {
                selectServer(row.dataset.name || "");
            }
        });
        tbodyHover.addEventListener('mouseover', (e) => {
            const row = e.target.closest('tr[data-name]');
            if (!row) return;
            hoveredName = row.dataset.name || null;
            applyHoverClass();
        });
        tbodyHover.addEventListener('mouseleave', () => {
            hoveredName = null;
            applyHoverClass();
        });

        const triageTable = document.getElementById('approval-triage-table');
        if (triageTable) {
            triageTable.addEventListener('click', (e) => {
                const button = e.target.closest('button[data-action]');
                if (button) {
                    handleServerAction(button.dataset.action || "", button.dataset.name || "", button.dataset.tab || "logs");
                    return;
                }
                const selectHostButton = e.target.closest('button[data-select-host]');
                if (selectHostButton) {
                    selectServer(selectHostButton.dataset.selectHost || "");
                    return;
                }
                const row = e.target.closest('tr[data-name]');
                if (row) {
                    selectServer(row.dataset.name || "");
                }
            });
        }

        const fleetRail = document.querySelector('.fleet-rail');
        if (fleetRail) {
            fleetRail.addEventListener('click', (e) => {
                const filterButton = e.target.closest('button[data-fleet-filter]');
                if (filterButton) {
                    fleetQuickFilter = filterButton.dataset.fleetFilter || "";
                    page = 1;
                    renderTable();
                    return;
                }
                const tagButton = e.target.closest('button[data-fleet-tag]');
                if (tagButton) {
                    fleetTagFilter = tagButton.dataset.fleetTag || "";
                    page = 1;
                    renderTable();
                }
            });
        }

        const applySortFromHeader = (th) => {
            if (!th) return;
            if (Date.now() < suppressSortClickUntil) {
                return;
            }
            const key = th.dataset.sortKey;
            if (sortKey === key) {
                sortDir = sortDir === "asc" ? "desc" : "asc";
            } else {
                sortKey = key;
                sortDir = "asc";
            }
            updateSortIndicators();
            renderTable();
        };

        document.querySelectorAll('#servers-table th.sortable').forEach((th) => {
            const trigger = th.querySelector('.sort-header-btn');
            if (trigger) {
                trigger.addEventListener('click', () => {
                    applySortFromHeader(th);
                });
                return;
            }
            th.addEventListener('click', () => {
                applySortFromHeader(th);
            });
        });

        document.getElementById('search').addEventListener('input', () => { page = 1; saveDashboardFilters(); renderTable(); });
        document.getElementById('status-filter').addEventListener('change', () => { page = 1; saveDashboardFilters(); renderTable(); });
        document.getElementById('auth-filter').addEventListener('change', () => { page = 1; saveDashboardFilters(); renderTable(); });
        document.getElementById('group-by').addEventListener('change', () => { page = 1; saveDashboardFilters(); renderTable(); });
        document.getElementById('page-size').addEventListener('change', () => { page = 1; saveDashboardFilters(); renderTable(); });

        document.getElementById('prev-page').addEventListener('click', () => {
            page = Math.max(1, page - 1);
            renderTable();
        });
        document.getElementById('next-page').addEventListener('click', () => {
            page += 1;
            renderTable();
        });

        document.getElementById('select-all').addEventListener('change', (e) => {
            const checked = e.target.checked;
            const filtered = sortServers(applyFilters(allServers));
            const paged = paginate(filtered);
            paged.forEach(server => {
                if (checked) {
                    selectedServers.add(server.name);
                } else {
                    selectedServers.delete(server.name);
                }
            });
            renderTable();
        });

        async function runBulkAction(actionPath, actionLabel) {
            const visibleSelected = new Set(
                Array.from(document.querySelectorAll('#servers-table tbody tr[data-name] .row-select:checked'))
                    .map(cb => cb.dataset.name)
                    .filter(Boolean)
            );
            const selectedNames = Array.from(selectedServers);
            const names = selectedNames.filter(name => visibleSelected.has(name));
            if (names.length === 0) {
                if (selectedNames.length > 0) {
                    alert(`No visible selected hosts for bulk ${actionLabel}.`);
                }
                return;
            }
            const hiddenCount = Math.max(0, selectedNames.length - names.length);

            const jobs = names.map(async (name) => {
                const response = await fetch(`/api/${actionPath}/${encodeURIComponent(name)}`, { method: 'POST' });
                if (!response.ok) {
                    const payload = await response.json().catch(() => ({}));
                    const detail = typeof payload.error === 'string' && payload.error.trim()
                        ? payload.error.trim()
                        : `${response.status} ${response.statusText}`.trim();
                    throw new Error(detail || 'Request failed');
                }
            });

            const results = await Promise.allSettled(jobs);
            const failures = [];
            results.forEach((result, index) => {
                if (result.status === 'rejected') {
                    failures.push(`${names[index]}: ${result.reason?.message || 'Request failed'}`);
                }
            });

            if (failures.length > 0) {
                console.error(`Bulk ${actionLabel} failures:`, failures);
                alert(`Bulk ${actionLabel} completed with ${failures.length} failure(s): ${failures.join(', ')}`);
            } else if (hiddenCount > 0) {
                alert(`Bulk ${actionLabel} completed for visible selected hosts; ${hiddenCount} hidden selected host(s) were skipped.`);
            }

            await fetchServers(true);
        }

        document.getElementById('bulk-update').addEventListener('click', async () => {
            await runBulkAction('update', 'update');
        });
        document.getElementById('bulk-approve').addEventListener('click', async () => {
            if (!window.confirm('Bulk approve all pending updates for the visible selected hosts?')) {
                return;
            }
            await runBulkAction('approve', 'approve');
        });
        document.getElementById('bulk-cancel').addEventListener('click', async () => {
            await runBulkAction('cancel', 'cancel');
        });
        document.getElementById('bulk-autoremove').addEventListener('click', async () => {
            await runBulkAction('autoremove', 'apt autoremove');
        });

        async function postServerAction(url, fallbackMessage, options = {}) {
            try {
                const response = await fetch(url, { method: 'POST', ...options });
                if (!response.ok) {
                    alert(await parseErrorResponse(response, fallbackMessage));
                    return false;
                }
                return true;
            } catch (error) {
                alert(error?.message || fallbackMessage);
                return false;
            }
        }

        async function updateServer(name) {
            if (await postServerAction(`/api/update/${encodeURIComponent(name)}`, 'Failed to start update.')) {
                fetchServers(true);
            }
        }

        async function runAutoremove(name) {
            if (await postServerAction(`/api/autoremove/${encodeURIComponent(name)}`, 'Failed to start apt autoremove.')) {
                fetchServers(true);
            }
        }

        async function enablePasswordlessApt(name) {
            let password;
            try {
                password = await promptPassword(`Enter sudo password for ${name}`);
            } catch {
                return;
            }
            if (!password) return;
            if (await postServerAction(`/api/sudoers/${encodeURIComponent(name)}`, 'Failed to enable passwordless apt.', {
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ password })
            })) {
                fetchServers(true);
            }
        }

        async function disablePasswordlessApt(name) {
            let password;
            try {
                password = await promptPassword(`Enter sudo password to disable for ${name}`);
            } catch {
                return;
            }
            if (!password) return;
            if (await postServerAction(`/api/sudoers/disable/${encodeURIComponent(name)}`, 'Failed to disable passwordless apt.', {
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ password })
            })) {
                fetchServers(true);
            }
        }

        function passwordModalFocusableElements(backdrop) {
            if (!backdrop) return [];
            return Array.from(backdrop.querySelectorAll([
                'button:not([disabled])',
                'input:not([disabled]):not([type="hidden"])',
                'select:not([disabled])',
                'textarea:not([disabled])',
                'a[href]',
                '[tabindex]:not([tabindex="-1"])'
            ].join(','))).filter((el) => {
                return !!(el.offsetWidth || el.offsetHeight || el.getClientRects().length);
            });
        }

        function trapPasswordModalFocus(event) {
            const backdrop = document.getElementById('password-modal');
            if (!backdrop || !backdrop.classList.contains('active')) return false;
            const focusable = passwordModalFocusableElements(backdrop);
            if (!focusable.length) {
                event.preventDefault();
                return true;
            }
            const first = focusable[0];
            const last = focusable[focusable.length - 1];
            if (!backdrop.contains(document.activeElement)) {
                event.preventDefault();
                first.focus();
                return true;
            }
            if (event.shiftKey && document.activeElement === first) {
                event.preventDefault();
                last.focus();
                return true;
            }
            if (!event.shiftKey && document.activeElement === last) {
                event.preventDefault();
                first.focus();
                return true;
            }
            return false;
        }

        function drawerFocusableElements(drawer) {
            if (!drawer) return [];
            return Array.from(drawer.querySelectorAll([
                'button:not([disabled])',
                'input:not([disabled]):not([type="hidden"])',
                'select:not([disabled])',
                'textarea:not([disabled])',
                'a[href]',
                '[tabindex]:not([tabindex="-1"])'
            ].join(','))).filter((el) => {
                return !!(el.offsetWidth || el.offsetHeight || el.getClientRects().length);
            });
        }

        function trapDrawerFocus(event) {
            if (!drawerOpen) return false;
            const drawer = document.getElementById('status-drawer');
            if (!drawer || drawer.getAttribute('aria-hidden') === 'true') return false;
            const focusable = drawerFocusableElements(drawer);
            if (!focusable.length) {
                event.preventDefault();
                drawer.focus({ preventScroll: true });
                return true;
            }
            const first = focusable[0];
            const last = focusable[focusable.length - 1];
            if (!drawer.contains(document.activeElement)) {
                event.preventDefault();
                first.focus({ preventScroll: true });
                return true;
            }
            if (event.shiftKey && document.activeElement === first) {
                event.preventDefault();
                last.focus({ preventScroll: true });
                return true;
            }
            if (!event.shiftKey && document.activeElement === last) {
                event.preventDefault();
                first.focus({ preventScroll: true });
                return true;
            }
            return false;
        }

        function promptPassword(message) {
            const backdrop = document.getElementById('password-modal');
            const input = document.getElementById('password-modal-input');
            const msg = document.getElementById('password-modal-message');
            msg.textContent = message;
            input.value = '';
            passwordModalPreviousFocus = document.activeElement;
            backdrop.classList.add('active');
            window.setTimeout(() => input.focus({ preventScroll: true }), 0);
            return new Promise((resolve, reject) => {
                passwordResolve = resolve;
                passwordReject = reject;
            });
        }

        function closePasswordModal() {
            const backdrop = document.getElementById('password-modal');
            backdrop.classList.remove('active');
            const previous = passwordModalPreviousFocus;
            passwordModalPreviousFocus = null;
            if (previous && document.contains(previous) && typeof previous.focus === 'function') {
                window.setTimeout(() => previous.focus({ preventScroll: true }), 0);
            }
        }

        function clearPasswordPromptHandlers() {
            passwordResolve = null;
            passwordReject = null;
        }

        document.getElementById('password-modal-cancel').addEventListener('click', () => {
            if (passwordReject) {
                const reject = passwordReject;
                clearPasswordPromptHandlers();
                closePasswordModal();
                reject(new Error('password prompt cancelled'));
                return;
            }
            closePasswordModal();
        });

        document.getElementById('password-modal-submit').addEventListener('click', () => {
            const input = document.getElementById('password-modal-input');
            if (passwordResolve) {
                const resolve = passwordResolve;
                clearPasswordPromptHandlers();
                closePasswordModal();
                resolve(input.value);
                return;
            }
            closePasswordModal();
        });

        document.getElementById('password-modal-form').addEventListener('submit', (e) => {
            e.preventDefault();
            const input = document.getElementById('password-modal-input');
            if (passwordResolve) {
                const resolve = passwordResolve;
                clearPasswordPromptHandlers();
                closePasswordModal();
                resolve(input.value);
                return;
            }
            closePasswordModal();
        });

        document.getElementById('password-modal-input').addEventListener('keydown', (e) => {
            if (e.key === 'Enter') {
                e.preventDefault();
                document.getElementById('password-modal-submit').click();
            }
        });

        window.addEventListener('keydown', (e) => {
            const backdrop = document.getElementById('password-modal');
            if (backdrop && backdrop.classList.contains('active')) {
                if (e.key === 'Tab') {
                    if (trapPasswordModalFocus(e)) {
                        e.stopImmediatePropagation();
                    }
                    return;
                }
                if (e.key === 'Escape') {
                    e.preventDefault();
                    e.stopImmediatePropagation();
                    document.getElementById('password-modal-cancel').click();
                    return;
                }
            }
            if (e.key === 'Tab' && trapDrawerFocus(e)) {
                e.stopImmediatePropagation();
                return;
            }
            if (e.key === 'Escape' && drawerOpen) {
                e.preventDefault();
                closeDrawer();
            }
        });

        document.getElementById('status-drawer-close').addEventListener('click', closeDrawer);
        document.getElementById('status-drawer-backdrop').addEventListener('click', closeDrawer);
        document.getElementById('drawer-tab-logs').addEventListener('click', () => setDrawerTab('logs'));
        document.getElementById('drawer-tab-pending').addEventListener('click', () => setDrawerTab('pending'));
        document.getElementById('drawer-copy-logs').addEventListener('click', () => copyLogs());
        document.getElementById('drawer-download-logs').addEventListener('click', () => downloadLogs());
        document.getElementById('drawer-approve-all').addEventListener('click', () => {
            if (!drawerServerName) return;
            approveAllUpdates(drawerServerName);
        });
        document.getElementById('drawer-approve-security').addEventListener('click', () => {
            if (!drawerServerName) return;
            approveSecurityUpdates(drawerServerName);
        });
        document.getElementById('drawer-approve-security-kept-back').addEventListener('click', () => {
            if (!drawerServerName) return;
            approveKeptBackSecurityUpdates(drawerServerName);
        });
        document.getElementById('drawer-approve-full').addEventListener('click', () => {
            if (!drawerServerName) return;
            approveFullUpgrade(drawerServerName);
        });

        const drawerLogsElement = document.getElementById('drawer-logs');
        drawerLogsElement.addEventListener('scroll', () => {
            drawerLogScrollTop = drawerLogsElement.scrollTop;
            drawerLogFollow = isNearBottom(drawerLogsElement);
            document.getElementById('drawer-logs-hint').textContent = drawerLogFollow ? "Live auto-scroll" : "Auto-scroll paused";
        });

        const drawerPendingElement = document.getElementById('drawer-panel-pending');
        drawerPendingElement.addEventListener('scroll', () => {
            if (drawerTab === "pending") {
                drawerPendingScrollTop = drawerPendingElement.scrollTop;
            }
        });

        document.addEventListener('pointerdown', (event) => {
            if (isServerActionControl(event.target)) {
                beginActionInteraction();
            }
        }, true);
        document.addEventListener('pointerup', () => {
            if (actionInteractionDepth > 0) {
                endActionInteraction();
            }
        }, true);
        document.addEventListener('pointercancel', () => {
            if (actionInteractionDepth > 0) {
                endActionInteraction();
            }
        }, true);
        document.addEventListener('keydown', (event) => {
            if (event.repeat || (event.key !== "Enter" && event.key !== " ")) return;
            if (isServerActionControl(event.target)) {
                beginActionInteraction();
            }
        }, true);
        document.addEventListener('keyup', (event) => {
            if (event.key !== "Enter" && event.key !== " ") return;
            if (actionInteractionDepth > 0) {
                endActionInteraction();
            }
        }, true);
        window.addEventListener('blur', resetActionInteraction);
        document.addEventListener('visibilitychange', () => {
            if (document.hidden) {
                resetActionInteraction();
            }
        });

        async function approveAllUpdates(name) {
            if (await postServerAction(`/api/approve/${encodeURIComponent(name)}`, 'Failed to approve updates.')) {
                fetchServers(true);
            }
        }

        async function approveSecurityUpdates(name) {
            if (await postServerAction(`/api/approve-security/${encodeURIComponent(name)}`, 'Failed to approve security updates.')) {
                fetchServers(true);
            }
        }

        async function approveKeptBackSecurityUpdates(name) {
            const server = getServerByName(name);
            const counts = getPendingApprovalCounts(server);
            if (!counts.keptBackSecurityPlanAvailable) {
                window.alert("Run a fresh package scan before approving kept-back security updates.");
                return;
            }
            const pendingUpdates = Array.isArray(server?.pending_updates) ? server.pending_updates : [];
            const packages = pendingUpdates
                .filter(update => !!update?.security && (!!update?.kept_back || !!update?.requires_full_upgrade))
                .map(update => update?.install_package || update?.package)
                .filter(Boolean);
            const removed = counts.keptBackSecurityRemovedPackages;
            const newPackages = counts.keptBackSecurityNewPackages;
            const impact = [];
            if (packages.length) impact.push(`Packages: ${packages.join(", ")}`);
            if (newPackages.length) impact.push(`May install dependencies: ${newPackages.join(", ")}`);
            if (removed.length) impact.push(`May remove packages: ${removed.join(", ")}`);
            const confirmText = [
                `Run kept-back security update on ${name}?`,
                "This uses targeted apt install for kept-back security packages only, not full-upgrade.",
                impact.join("\n")
            ].filter(Boolean).join("\n\n");
            if (!window.confirm(confirmText)) return;
            const body = removed.length ? { confirm_removals: true } : {};
            if (await postServerAction(`/api/approve-security-kept-back/${encodeURIComponent(name)}`, 'Failed to approve kept-back security updates.', {
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(body)
            })) {
                fetchServers(true);
            }
        }

        async function approveFullUpgrade(name) {
            const server = getServerByName(name);
            const counts = getPendingApprovalCounts(server);
            if (!counts.fullPlanAvailable) {
                window.alert("Run a fresh package scan before approving full-upgrade.");
                return;
            }
            const removed = counts.removedPackages;
            const newPackages = counts.newPackages;
            const impact = [];
            if (newPackages.length) impact.push(`New packages: ${newPackages.join(", ")}`);
            if (removed.length) impact.push(`Removed packages: ${removed.join(", ")}`);
            const confirmText = [
                `Run full-upgrade on ${name}?`,
                impact.join("\n")
            ].filter(Boolean).join("\n\n");
            if (!window.confirm(confirmText)) return;
            const body = removed.length ? { confirm_removals: true } : {};
            if (await postServerAction(`/api/approve-full/${encodeURIComponent(name)}`, 'Failed to approve full upgrade.', {
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(body)
            })) {
                fetchServers(true);
            }
        }

        async function cancelUpgrade(name) {
            if (await postServerAction(`/api/cancel/${encodeURIComponent(name)}`, 'Failed to cancel upgrade.')) {
                fetchServers(true);
            }
        }

        async function refreshHostFacts(name) {
            try {
                const response = await fetch(`/api/servers/${encodeURIComponent(name)}/facts/refresh`, { method: 'POST' });
                if (!response.ok) {
                    const payload = await response.json().catch(() => ({}));
                    alert(payload.error || "Failed to refresh host facts");
                    return;
                }
                await fetchDashboardSummary();
            } catch (err) {
                alert(err?.message || "Failed to refresh host facts");
                return;
            }
        }

        document.getElementById('selected-host-panel').addEventListener('click', (e) => {
            const button = e.target.closest('button[data-action]');
            if (button) {
                handleServerAction(button.dataset.action || "", button.dataset.name || "", button.dataset.tab || "logs");
            }
        });
        document.getElementById('selected-host-panel').addEventListener('toggle', (e) => {
            const details = e.target;
            if (!details?.matches?.('details.facts-more')) return;
            const hostName = details.dataset.name || selectedServerName;
            if (!hostName) return;
            if (details.open) {
                expandedHostFactsServers.add(hostName);
            } else {
                expandedHostFactsServers.delete(hostName);
            }
        }, true);

        document.querySelectorAll('.operations-grid, .context-ops-grid').forEach((panel) => {
            panel.addEventListener('click', (e) => {
                const actionButton = e.target.closest('button[data-action]');
                if (actionButton) {
                    handleServerAction(actionButton.dataset.action || "", actionButton.dataset.name || "", actionButton.dataset.tab || "logs");
                    return;
                }
                const selectButton = e.target.closest('button[data-select-server]');
                if (selectButton) {
                    selectServer(selectButton.dataset.selectServer || "");
                    return;
                }
                const miniListButton = e.target.closest('button[data-toggle-mini-list]');
                if (miniListButton) {
                    const listID = miniListButton.dataset.toggleMiniList || "";
                    if (!listID) return;
                    if (expandedMiniLists.has(listID)) {
                        expandedMiniLists.delete(listID);
                    } else {
                        expandedMiniLists.add(listID);
                    }
                    renderDashboardPanels();
                }
            });
        });

        document.getElementById('logout-btn').addEventListener('click', () => window.logout());
        loadDashboardFilters();
        initColumnResizing();
        setInterval(renderSyncState, 5000);
        configurePolling(fallbackServerPollMs, fallbackExtrasPollMs);
        connectDashboardEvents();
        fetchDashboardExtras();
        fetchServers();
