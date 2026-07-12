const LOG_BOTTOM_THRESHOLD = 20;
        const dashboardConsumption = window.DashboardProjectionConsumption;
        const statusFormatting = window.StatusFormatting;
        const statusRendering = window.StatusRendering.create(document);
        const statusDrawerAdapter = window.StatusDrawerAdapter.create(document, window);
        const statusTableAdapter = window.StatusTableAdapter.create(localStorage, "simplelinuxupdater.statusTableColWidths.v15");
        const statusActionAdapter = window.StatusActionAdapter.create(window);
        const statusInteraction = window.StatusPageInteraction.createStore({
            presentationFacts: dashboardConsumption.presentationFacts
        });
        window.statusPageInteraction = statusInteraction;
        let allServers = [];
        let lastSuccessfulSyncAt = null;
        let lastFetchError = null;
        let recentActivity = [];
        let observabilitySummary = null;
        let policySummary = null;
        let dashboardPresentation = dashboardConsumption.project({ statusView: statusInteraction.getView() });
        let globalKeyAvailable = false;
        let dashboardExtraErrors = new Map();
        let hoveredName = null;
	        let expandedHostFactsServers = new Set();
	        let expandedMiniLists = new Set();
	        let activePhaseTooltipTarget = null;
	        let bulkReviewResolve = null;
        let drawerLogScrollTop = 0;
        let drawerPendingScrollTop = 0;
        let passwordResolve = null;
        let passwordReject = null;
        let passwordModalPreviousFocus = null;
        let suppressSortClickUntil = 0;
        let actionInteractionReleaseTimer = null;
        const actionInteractionDeferMs = 350;
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

        function getStatusView() {
            return statusInteraction.getView();
        }

        function refreshDashboardPresentation() {
            dashboardPresentation = dashboardConsumption.project({
                statusView: getStatusView(),
                globalKeyAvailable,
                extras: { recentActivity, observabilitySummary, policySummary }
            });
            return dashboardPresentation;
        }

        function serverPresentation(serverOrName) {
            const name = typeof serverOrName === "string" ? serverOrName : serverOrName?.name;
            return dashboardPresentation.serversByName[name] || null;
        }

        function dispatchStatusInteraction(event) {
            const effects = statusInteraction.dispatch(event);
            const tasks = [];
            effects.forEach(effect => {
                if (effect.type === "persistFilters") {
                    persistDashboardFilters(effect.value);
                } else if (effect.type === "fetchSnapshot") {
                    tasks.push(executeStatusSnapshotFetch(effect));
                } else if (effect.type === "render" && effect.scope === "serverState") {
                    renderServerState();
                } else if (effect.type === "renderSyncState") {
                    renderSyncState();
                } else if (effect.type === "cancelInteractionRelease") {
                    if (actionInteractionReleaseTimer !== null) {
                        clearTimeout(actionInteractionReleaseTimer);
                        actionInteractionReleaseTimer = null;
                    }
                } else if (effect.type === "scheduleInteractionRelease") {
                    if (actionInteractionReleaseTimer !== null) clearTimeout(actionInteractionReleaseTimer);
                    actionInteractionReleaseTimer = setTimeout(() => {
                        actionInteractionReleaseTimer = null;
                        dispatchStatusInteraction({ type: "interactionReleased" });
                    }, effect.delayMs);
                } else if (effect.type === "actionRejected") {
                    statusActionAdapter.notify(effect.reason || "Action is no longer available");
                } else if (effect.type === "announceResult" && effect.message) {
                    statusActionAdapter.notify(effect.message);
                }
            });
            return Promise.all(tasks);
        }

        function isRunningTimelineState(state) {
            return ["active", "queued"].includes(String(state || "").toLowerCase());
        }

        function setText(id, value) {
            statusRendering.text(id, value);
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
            return statusFormatting.duration(ms);
        }

        function formatDiskFree(kb) {
            return statusFormatting.diskFree(kb);
        }

        function formatDiskCapacity(freeKB, totalKB) {
            return statusFormatting.diskCapacity(freeKB, totalKB);
        }

        function formatUptime(seconds) {
            return statusFormatting.uptime(seconds);
        }

        function statusLabel(value) {
            return statusFormatting.statusLabel(value);
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
            return !!serverPresentation(server)?.hasPendingUpdates;
        }

        function getPendingApprovalCounts(server) {
            return serverPresentation(server)?.approvalCounts || {
                total: 0, standard: 0, keptBack: 0, full: 0, security: null, totalSecurity: null,
                keptBackSecurity: 0, fullPlanAvailable: false, keptBackSecurityPlanAvailable: false,
                keptBackSecurityNewPackages: [], keptBackSecurityRemovedPackages: [], newPackages: [], removedPackages: []
            };
        }

        function getPendingPackageCount(server) {
            return serverPresentation(server)?.risk.pendingPackages || 0;
        }

        function getSecurityUpdateCount(server) {
            return serverPresentation(server)?.risk.securityUpdates || 0;
        }

        function getRiskLabel(server) {
            return serverPresentation(server)?.risk.label || "Normal";
        }

        function getRiskLevel(server) {
            return serverPresentation(server)?.risk.level || "normal";
        }

        function getServerIntelligence(name) {
            return serverPresentation(name)?.intelligence || null;
        }

	        function getServerTimeline(server) {
	            return serverPresentation(server)?.timeline || { current_phase: "", current_label: "Idle", state: "idle", progress_pct: 0, summary: "No maintenance activity", phases: [] };
	        }

	        function isRuntimePendingApproval(server) {
	            return !!serverPresentation(server)?.runtimePending;
	        }

	        function isTimelinePendingApproval(server) {
	            return !!serverPresentation(server)?.timelinePending;
	        }

	        function isPendingApprovalHost(server) {
	            return !!serverPresentation(server)?.pendingApproval;
	        }

	        function pendingApprovalDriftReason(server) {
	            return serverPresentation(server)?.driftReason || "";
	        }

	        function getServerFailureReason(server) {
	            return serverPresentation(server)?.failureReason || "";
	        }

	        function getServerApprovalTriage(server, options = {}) {
	            const projected = serverPresentation(server)?.triage || { eligible: false, pending_packages: 0, security_updates: 0, cve_count: 0, risk_level: "normal", risk_label: "Normal", risk_order: 0, facts_state: "unknown" };
	            if (!options.ignoreInFlight || !server) return projected;
	            return {
	                ...projected,
	                can_approve_all: !!statusInteraction.getAction(server.name, "approve_all", options)?.enabled,
	                can_approve_security: !!statusInteraction.getAction(server.name, "approve_security", options)?.enabled,
	                can_approve_kept_back_security: !!statusInteraction.getAction(server.name, "approve_security_kept_back", options)?.enabled,
	                can_approve_full: !!statusInteraction.getAction(server.name, "approve_full", options)?.enabled,
	                can_cancel: !!statusInteraction.getAction(server.name, "cancel", options)?.enabled
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
	            const label = phase.label || key;
	            const summary = phase.summary || "";
	            const updatedAt = phase.updated_at_display || phase.updated_at || "";
	            const aria = [label, state, summary, updatedAt].filter(Boolean).join(" · ");
	            return `
                <span class="timeline-dot phase-${escapeHtml(state)}" role="img" tabindex="0" aria-label="${escapeHtml(aria)}" data-phase-label="${escapeHtml(label)}" data-phase-state="${escapeHtml(state)}" data-phase-summary="${escapeHtml(summary)}" data-phase-time="${escapeHtml(updatedAt)}" data-phase-progress="${escapeHtml(String(pct))}">
	                    <span class="${progressClass(pct)}"></span>
	                </span>
	            `;
	        }

        function hasEffectiveKey(server) {
            return !!serverPresentation(server)?.auth.effectiveKey;
        }

        function usesGlobalKey(server) {
            return !!serverPresentation(server)?.auth.usesGlobalKey;
        }

        function getAuthLabel(server) {
            return serverPresentation(server)?.auth.label || "No auth configured";
        }

        function getLatestLogLines(server, limit = 5) {
            const lines = String(server?.logs || "")
                .split(/\r?\n/)
                .map(line => line.trim())
                .filter(Boolean);
            return lines.slice(-limit);
        }

        function renderDashboardMetrics() {
            statusRendering.metrics(dashboardPresentation, getStatusView().filters.quick);
            updateRefreshAllFactsState();
        }

        function updateMetricFilterState() {
            statusRendering.metricFilterState(getStatusView().filters.quick);
        }

        function quickFilterActionLabel(key) {
            return statusRendering.metricFilterLabel(key);
        }

        function buttonStateAttrs(enabled, enabledTitle, disabledTitle) {
            const title = enabled ? enabledTitle : disabledTitle;
            return `${enabled ? "" : "disabled"} title="${escapeHtml(title)}"`;
        }

        function renderPendingUpdatesHtml(server, includeHeading = true) {
            const updates = Array.isArray(server?.pending_updates) ? server.pending_updates : [];
            if (!hasPendingUpdates(server)) {
                return `<p class="drawer-empty">No pending update details for this server.</p>`;
            }
            const driftReason = pendingApprovalDriftReason(server);

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
                    ${driftReason ? `<p class="pending-note pending-drift-note" title="${escapeHtml(driftReason)}">${escapeHtml(driftReason)}. Approval actions stay disabled until the host is pending approval again.</p>` : ""}
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
            const extrasError = dashboardExtraErrors.size > 0
                ? Array.from(dashboardExtraErrors.values()).join("; ")
                : "";
            const degraded = !!lastFetchError || !!extrasError;
            statusRendering.syncState({
                degraded,
                live: statusTransport.isLive(),
                lastSyncText: lastFetchError || extrasError
                    ? `Last sync error: ${lastFetchError?.message || extrasError || "unknown"}`
                    : formatRelativeTime(lastSuccessfulSyncAt)
            });
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
	                const detail = typeof options.detail === "function"
	                    ? options.detail(server)
	                    : options.compactDetail
	                    ? risk
	                    : `${status} · ${risk}`;
	                return `
	                    <div class="mini-row compact-row">
	                        <button type="button" class="mini-row-main" data-select-server="${safeDataName}">
	                            <strong>${safeName || "Unnamed host"}</strong>
	                            <span title="${escapeHtml(detail)}">${escapeHtml(detail)}</span>
	                        </button>
	                        <button type="button" class="mini-action" data-action="${action}" data-name="${safeDataName}" data-tab="${actionTab}">${actionLabel}</button>
	                    </div>
	                `;
	            });
	            if (expandable && servers.length > limit) {
	                const moreText = expanded
	                    ? (options.lessLabel || "Show fewer")
	                    : (typeof options.moreLabel === "function" ? options.moreLabel(hiddenCount, servers.length) : `Show all ${servers.length}`);
                rows.push(`<button type="button" class="mini-more-row mini-more-button" data-toggle-mini-list="${escapeHtml(id)}" aria-expanded="${expanded ? "true" : "false"}">${escapeHtml(moreText)}</button>`);
            } else if (hiddenCount > 0) {
                const moreText = typeof options.moreLabel === "function"
                    ? options.moreLabel(hiddenCount, servers.length)
                    : `+${hiddenCount} more`;
                rows.push(`<div class="mini-more-row">${escapeHtml(moreText)}</div>`);
            }
            el.innerHTML = rows.join("");
        }

	        function failureMiniDetail(server) {
	            const reason = getServerFailureReason(server);
	            return reason ? `${statusLabel(server.status)} · ${reason}` : `${statusLabel(server.status)} · ${getRiskLabel(server)}`;
	        }

        function renderTagSummary() {
            const el = document.getElementById('tag-summary');
            if (!el) return;
            const entries = dashboardPresentation.panels.tags.map(item => [item.tag, item.total]);
            if (entries.length === 0) {
                el.innerHTML = miniEmpty("No tags yet.");
                return;
            }
            el.innerHTML = entries.slice(0, 10).map(([tag, count]) => (
                `<span class="chip">${escapeHtml(tag)} <strong>${count}</strong></span>`
            )).join("");
        }

        function renderFleetFilters() {
            const view = getStatusView();
            setText("fleet-filter-summary", `${pluralize(view.visibleServers.length, "host")} visible`);
            const statusEl = document.getElementById('fleet-status-filters');
            if (statusEl) {
                const activeCount = dashboardPresentation.fleet.active;
                const staleCount = dashboardPresentation.fleet.staleFacts;
                const highRiskCount = dashboardPresentation.fleet.highRiskCVE;
                const filters = [
                    { key: "", label: "All", count: allServers.length },
                    { key: "pending_approval", label: "Pending", count: dashboardPresentation.fleet.pendingApproval },
                    { key: "active", label: "Active", count: activeCount },
                    { key: "stale_facts", label: "Stale", count: staleCount },
                    { key: "high_risk", label: "High risk", count: highRiskCount }
                ];
                statusEl.innerHTML = filters.map(item => `
                    <button type="button" class="filter-pill${view.filters.quick === item.key ? " active" : ""}" data-fleet-filter="${escapeHtml(item.key)}" aria-label="${escapeHtml(quickFilterActionLabel(item.key))}" title="${escapeHtml(quickFilterActionLabel(item.key))}">
                        <span>${escapeHtml(item.label)}</span>
                        <strong>${item.count}</strong>
                    </button>
                `).join("");
            }
            updateMetricFilterState();

            const tagEl = document.getElementById('fleet-tag-list');
            if (!tagEl) return;
            const entries = dashboardPresentation.panels.tags.map(item => [item.tag, item.total]);
            if (entries.length === 0) {
                tagEl.innerHTML = `<span class="empty-state compact-empty">No tags</span>`;
                return;
            }
	            tagEl.innerHTML = [
	                `<button type="button" class="filter-pill${view.filters.tag === "" ? " active" : ""}" data-fleet-tag="" aria-label="Show hosts with any tag" title="Show hosts with any tag"><span>All tags</span><strong>${allServers.length}</strong></button>`,
	                ...entries.slice(0, 8).map(([tag, count]) => `
	                    <button type="button" class="filter-pill${view.filters.tag === tag ? " active" : ""}" data-fleet-tag="${escapeHtml(tag)}" aria-label="${escapeHtml(`Show hosts tagged ${tag}`)}" title="${escapeHtml(`Show hosts tagged ${tag}`)}">
	                        <span>${escapeHtml(tag)}</span>
	                        <strong>${count}</strong>
	                    </button>
	                `)
	            ].join("");
        }

        function renderApprovalTriage() {
            const body = document.getElementById('approval-triage-body');
            if (!body) return;
            const listID = "approval-triage";
            const limit = 12;
            const expanded = expandedMiniLists.has(listID);
            const primaryServerName = getStatusView().primaryServerName;
            const servers = dashboardPresentation.panels.approval.map(model => model.server);
            setText("approval-queue-count", String(servers.length));
            if (servers.length === 0) {
                body.innerHTML = `<tr><td colspan="9">${miniEmpty("No approvals require triage.")}</td></tr>`;
                return;
            }
            const rows = servers.slice(0, expanded ? servers.length : limit).map(server => {
                const safeName = escapeHtml(server.name || "");
                const safeDataName = escapeHtml(server.name || "");
                const triage = getServerApprovalTriage(server);
                const approvalCounts = getPendingApprovalCounts(server);
                const keptBackSecurityCount = Number(triage.kept_back_security_updates ?? approvalCounts.keptBackSecurity ?? 0);
                const canApproveKeptBackSecurity = !!triage.can_approve_kept_back_security;
                const canApproveAll = !!triage.can_approve_all;
                const canApproveSecurity = !!triage.can_approve_security;
	                const riskLevel = String(triage.risk_level || getRiskLevel(server)).toLowerCase();
	                const tags = Array.isArray(server.tags) && server.tags.length ? server.tags.join(", ") : "ungrouped";
	                const factsState = String(triage.facts_state || "unknown").toLowerCase();
	                const canUpdate = canRunUpdateAction(server);
	                const canRefreshFacts = canRefreshFactsAction(server);
	                const rowSelected = getStatusView().primaryServerName === server.name;
	                const driftReason = pendingApprovalDriftReason(server);
	                const driftNotice = driftReason
	                    ? `<span class="action-note pending-drift-note" title="${escapeHtml(driftReason)}">${escapeHtml(driftReason)}</span>`
	                    : "";
	                const actions = server.status === "pending_approval"
	                    ? `
		                        <div class="triage-actions">
		                            <button type="button" data-action="approve-all" data-name="${safeDataName}" ${buttonStateAttrs(canApproveAll, "Approve standard updates", "No standard updates are eligible")}>Approve (${Number(triage.standard_packages ?? approvalCounts.standard)})</button>
		                            <button type="button" class="btn-security" data-action="approve-security" data-name="${safeDataName}" ${buttonStateAttrs(canApproveSecurity, "Approve only standard security updates", "No standard security updates are eligible")}>Standard security (${Number(triage.standard_security_updates ?? approvalCounts.security ?? 0)})</button>
		                            ${canApproveKeptBackSecurity ? `<button type="button" class="btn-security" data-action="approve-security-kept-back" data-name="${safeDataName}" title="Approve only kept-back security updates">Kept-back security (${keptBackSecurityCount})</button>` : ""}
		                            ${triage.can_approve_full ? `<button type="button" class="btn-full-upgrade" data-action="approve-full" data-name="${safeDataName}" title="Run apt full-upgrade">Full upgrade (${approvalCounts.full})</button>` : ""}
		                            <button type="button" class="btn-danger" data-action="cancel-upgrade" data-name="${safeDataName}" ${triage.can_cancel ? "" : "disabled"}>Cancel</button>
                            <button type="button" class="btn-ghost" data-action="open-drawer" data-name="${safeDataName}" data-tab="pending">Packages</button>
	                        </div>
                    `
	                    : driftReason
	                        ? `
	                            <div class="triage-actions triage-actions-note">
	                                ${driftNotice}
	                                ${canUpdate ? `<button type="button" data-action="update-server" data-name="${safeDataName}" title="Run fresh update checks">Update</button>` : ""}
	                                ${hasPendingUpdates(server) ? `<button type="button" class="btn-ghost" data-action="open-drawer" data-name="${safeDataName}" data-tab="pending">Packages</button>` : ""}
	                                <button type="button" class="btn-ghost" data-action="open-drawer" data-name="${safeDataName}" data-tab="logs">Logs</button>
	                            </div>
		                    `
	                    : `
	                        <div class="triage-actions">
	                            <button type="button" data-action="update-server" data-name="${safeDataName}" ${canUpdate ? "" : "disabled"} title="${canUpdate ? "Run update checks" : "Host cannot run update checks right now"}">Update</button>
	                            <button type="button" class="btn-ghost" data-action="refresh-facts" data-name="${safeDataName}" ${canRefreshFacts ? "" : "disabled"} title="${canRefreshFacts ? "Refresh host facts" : "Host facts cannot refresh while another action is active"}">Host facts</button>
	                            <button type="button" class="btn-ghost" data-action="open-drawer" data-name="${safeDataName}" data-tab="logs">Logs</button>
	                        </div>
		                    `;
                return `
                    <tr data-name="${safeDataName}" class="${rowSelected ? "row-selected" : ""}" aria-selected="${rowSelected ? "true" : "false"}">
                        <td><button type="button" class="select-host" data-select-host="${safeDataName}" aria-pressed="${rowSelected ? "true" : "false"}">${safeName || "Unnamed host"}</button></td>
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
            });
            if (servers.length > limit) {
                const label = expanded ? "Show fewer triage hosts" : `Show all ${servers.length} triage hosts`;
                rows.push(`<tr class="triage-more-row"><td colspan="9"><button type="button" class="mini-more-row mini-more-button" data-toggle-mini-list="${listID}" aria-expanded="${expanded ? "true" : "false"}">${escapeHtml(label)}</button></td></tr>`);
            }
            body.innerHTML = rows.join("");
        }

        function renderScheduledRuns() {
            const el = document.getElementById('scheduled-runs');
            if (!el) return;
            const scheduled = dashboardPresentation.panels.scheduled.map(model => ({ server: model.server, nextRun: model.nextRun }));
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
	                const moreText = expanded ? "Show fewer scheduled runs" : `Show all ${scheduled.length} scheduled runs`;
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
            const activity = dashboardPresentation.panels.recentActivity;
            if (activity.length === 0) {
                el.innerHTML = miniEmpty("No recent activity.");
                return;
            }
            el.innerHTML = activity.slice(0, 2).map(item => {
                const evt = item.raw;
                const status = item.status;
                const statusClass = safeStatusClass(status === "failure" ? "error" : status);
                return `
                    <div class="activity-row">
                        <span class="status-pill status-${statusClass}">${escapeHtml(status || "unknown")}</span>
                        <div>
                            <strong>${escapeHtml(item.action)}</strong>
                            <span>${escapeHtml(item.target)} · ${escapeHtml(formatActivityTime(evt))}</span>
                        </div>
                    </div>
                `;
            }).join("");
        }

        function renderIntelligenceLists() {
            const rebootHosts = dashboardPresentation.panels.reboot.map(model => model.server);
            const riskHosts = dashboardPresentation.panels.risk.map(model => model.server);
            setText("reboot-required-count", String(rebootHosts.length));
            setText("risk-exposure-count", String(riskHosts.length));
            renderMiniServerList("reboot-required-panel", rebootHosts, "No reboot required.", {
                limit: 1,
                expandable: true,
                compactDetail: true,
	                action: "open-drawer",
	                actionLabel: "Logs",
	                lessLabel: "Show fewer reboot hosts",
	                moreLabel: (_hidden, total) => `Show all ${total} reboot host${total === 1 ? "" : "s"}`
	            });
	            renderMiniServerList("risk-exposure-panel", riskHosts, "No CVE exposure.", {
	                limit: 1,
                expandable: true,
                compactDetail: true,
                action: "open-drawer",
	                actionLabel: "Review",
	                actionTab: "pending",
	                lessLabel: "Show fewer risk hosts",
	                moreLabel: (_hidden, total) => `Show all ${total} risk host${total === 1 ? "" : "s"}`
	            });
            renderCommandHistoryPanel();
        }

	        function renderCommandHistoryPanel() {
	            const el = document.getElementById('command-history-panel');
	            if (!el) return;
	            const primaryServerName = getStatusView().primaryServerName;
            const history = dashboardPresentation.panels.commandHistory;
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
	                            <span>${escapeHtml(item.message || primaryServerName || "server")} · ${escapeHtml(item.created_at_display || formatRelativeTimestamp(item.created_at))}</span>
                        </div>
                    </div>
                `;
            });
            const hiddenCount = history.length - visibleHistory.length;
            if (history.length > limit) {
	                const moreText = expanded ? "Show fewer commands" : `Show all ${history.length} command${history.length === 1 ? "" : "s"}`;
                rows.push(`<button type="button" class="mini-more-row mini-more-button" data-toggle-mini-list="${listID}" aria-expanded="${expanded ? "true" : "false"}">${escapeHtml(moreText)}</button>`);
            }
	            el.innerHTML = rows.join("");
	        }

	        function renderPriorityAttention(failedServers, rebootHosts) {
	            const strip = document.getElementById('priority-attention-strip');
	            if (!strip) return;
	            const hasPriority = failedServers.length > 0 || rebootHosts.length > 0;
	            strip.classList.toggle('hidden', !hasPriority);
	            setText("priority-failures-count", String(failedServers.length));
	            setText("priority-reboot-count", String(rebootHosts.length));
	            if (!hasPriority) {
	                const failuresPanel = document.getElementById("priority-failures-panel");
	                const rebootPanel = document.getElementById("priority-reboot-panel");
	                if (failuresPanel) failuresPanel.innerHTML = "";
	                if (rebootPanel) rebootPanel.innerHTML = "";
	                return;
	            }
	            renderMiniServerList("priority-failures-panel", failedServers, "No failures.", {
	                limit: 1,
	                expandable: true,
	                detail: failureMiniDetail,
	                action: "open-drawer",
	                actionLabel: "Logs",
	                lessLabel: "Show fewer failures",
	                moreLabel: (_hidden, total) => `Show all ${total} failure${total === 1 ? "" : "s"}`
	            });
	            renderMiniServerList("priority-reboot-panel", rebootHosts, "No reboot required.", {
	                limit: 1,
	                expandable: true,
	                compactDetail: true,
	                action: "open-drawer",
	                actionLabel: "Logs",
	                lessLabel: "Show fewer reboot hosts",
	                moreLabel: (_hidden, total) => `Show all ${total} reboot host${total === 1 ? "" : "s"}`
	            });
	        }

	        function renderSummaryBadges() {
            const policyEl = document.getElementById('policy-summary-label');
            if (policyEl) {
                const count = dashboardPresentation.summaries.policyCount;
                policyEl.textContent = count === null ? "Policies --" : `Policies ${count}`;
            }
            const obsEl = document.getElementById('observability-summary-label');
            if (obsEl) {
                const total = dashboardPresentation.summaries.observabilityUpdates;
                const success = dashboardPresentation.summaries.observabilitySuccessRate;
                obsEl.textContent = total > 0 ? `7d ${success.toFixed(0)}%` : "7d no runs";
            }
        }

        function renderSelectedHostPanel() {
            const panel = document.getElementById('selected-host-panel');
            const title = document.getElementById('selected-host-title');
            const subtitle = document.getElementById('selected-host-subtitle');
            if (!panel || !title || !subtitle) return;
            const selected = dashboardPresentation.selectedHost;
            const server = selected?.server;
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
            const canApproveKeptBackSecurity = !!triage.can_approve_kept_back_security;
            const canApproveAll = !!triage.can_approve_all;
            const canApproveSecurity = !!triage.can_approve_security;
            const health = intelligence?.health || {};
            const nextRun = intelligence?.next_run || {};
            const noRun = intelligence?.no_run || {};
            const lastUpdate = intelligence?.last_update;
            const lastFailed = intelligence?.last_failed_update;
            const rebootText = health.reboot_required === true ? "Required" : (health.reboot_required === false ? "Not required" : "Unknown");
	            const factsAge = health.collected_at ? formatRelativeTimestamp(health.collected_at, "Facts not collected") : "Facts not collected";
		            const canRunUpdate = canRunUpdateAction(server);
		            const canRunAutoremove = canRunAutoremoveAction(server);
		            const canRefreshFacts = canRefreshFactsAction(server);
		            const canRunSudoers = canRunSudoersAction(server);
		            const driftReason = pendingApprovalDriftReason(server);
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
                ${driftReason ? `<p class="inspector-note pending-drift-note" title="${escapeHtml(driftReason)}">${escapeHtml(driftReason)}. Approval actions stay disabled until the host is pending approval again.</p>` : ""}
                <div class="inspector-actions inspector-actions-primary">
                    ${server.status === 'pending_approval' ? `<button type="button" class="inline-btn btn-success" data-action="approve-all" data-name="${safeDataName}" ${buttonStateAttrs(canApproveAll, "Approve standard updates", "No standard updates are eligible")}>Approve (${approvalCounts.standard})</button>` : ""}
                    ${server.status === 'pending_approval' ? `<button type="button" class="inline-btn btn-security" data-action="approve-security" data-name="${safeDataName}" ${buttonStateAttrs(canApproveSecurity, "Approve only standard security updates", "No standard security updates are eligible")}>Standard securityurity (${approvalCounts.security ?? 0})</button>` : ""}
                    ${canApproveKeptBackSecurity ? `<button type="button" class="inline-btn btn-security" data-action="approve-security-kept-back" data-name="${safeDataName}" title="Approve only kept-back security updates">Kept-back security (${keptBackSecurityCount})</button>` : ""}
                    ${triage.can_approve_full ? `<button type="button" class="inline-btn btn-full-upgrade" data-action="approve-full" data-name="${safeDataName}" title="Run apt full-upgrade">Full (${approvalCounts.full})</button>` : ""}
	                    ${canRunUpdate ? `<button type="button" class="inline-btn primary-action" data-action="update-server" data-name="${safeDataName}">Update</button>` : ""}
                    <button type="button" class="inline-btn btn-ghost" data-action="open-drawer" data-name="${safeDataName}" data-tab="logs">Logs</button>
                    ${hasPendingUpdates(server) ? `<button type="button" class="inline-btn btn-ghost" data-action="open-drawer" data-name="${safeDataName}" data-tab="pending">Packages</button>` : ""}
                </div>
                <div class="inspector-tools">
                    <span class="mini-label">Tools</span>
		                    <div class="inspector-actions inspector-actions-secondary">
		                        <button type="button" class="inline-btn" data-action="run-autoremove" data-name="${safeDataName}" ${canRunAutoremove ? "" : "disabled"} title="${canRunAutoremove ? "Run apt autoremove" : "Host cannot run autoremove right now"}">Autoremove</button>
		                        <button type="button" class="inline-btn" data-action="refresh-facts" data-name="${safeDataName}" ${canRefreshFacts ? "" : "disabled"} title="${canRefreshFacts ? "Refresh host facts" : "Host facts cannot refresh while another action is active"}">Host facts</button>
	                        <button type="button" class="inline-btn" data-action="enable-apt" data-name="${safeDataName}" ${canRunSudoers ? "" : "disabled"} title="${canRunSudoers ? "Enable passwordless apt" : "Host cannot change passwordless apt while another action is active"}">Enable apt</button>
	                        <button type="button" class="inline-btn" data-action="disable-apt" data-name="${safeDataName}" ${canRunSudoers ? "" : "disabled"} title="${canRunSudoers ? "Disable passwordless apt" : "Host cannot change passwordless apt while another action is active"}">Disable apt</button>
	                    </div>
	                </div>
                <dl class="host-facts host-facts-primary">
                    <div><dt>Packages</dt><dd>${escapeHtml(packageSummary)}</dd></div>
                    <div><dt>OS</dt><dd>${escapeHtml(health.os_pretty_name || "Facts not collected")}</dd></div>
                    <div><dt>Reboot</dt><dd>${escapeHtml(rebootText)}</dd></div>
                    <div><dt>Disk</dt><dd>${escapeHtml(`${health.disk_status || "unknown"} · ${formatDiskCapacity(health.disk_free_kb, health.disk_total_kb)}`)}</dd></div>
                    <div><dt>APT</dt><dd>${escapeHtml(health.apt_status || "unknown")}</dd></div>
                    <div><dt>Host facts</dt><dd>${escapeHtml(triage.facts_state || "unknown")} · ${escapeHtml(factsAge)}</dd></div>
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
	            const activeServers = dashboardPresentation.panels.active.map(model => model.server);
	            const failedServers = dashboardPresentation.panels.failed.map(model => model.server);
	            const rebootHosts = dashboardPresentation.panels.reboot.map(model => model.server);
	            setText("active-operations-count", String(activeServers.length));
	            setText("failed-hosts-count", String(failedServers.length));
	            renderPriorityAttention(failedServers, rebootHosts);
	            renderMiniServerList("active-operations", activeServers, "No active runs.", {
	                limit: 1,
	                expandable: true,
	                lessLabel: "Show fewer running operations",
	                moreLabel: (_hidden, total) => `Show all ${total} running`
	            });
	            renderMiniServerList("failed-hosts-panel", failedServers, "No failures.", {
	                limit: 1,
	                expandable: true,
	                detail: failureMiniDetail,
	                lessLabel: "Show fewer failures",
	                moreLabel: (_hidden, total) => `Show all ${total} failure${total === 1 ? "" : "s"}`
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

	        function renderTableDependentPanels() {
	            renderFleetFilters();
	            renderApprovalTriage();
	            renderSelectedHostPanel();
	            renderCommandHistoryPanel();
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
            refreshDashboardPresentation();
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
            refreshDashboardPresentation();
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
            refreshDashboardPresentation();
            renderSummaryBadges();
        }

        function requestStatusRefresh(streams, priority = "deferable", reason = "refresh") {
            return dispatchStatusInteraction({ type: "refreshRequested", streams, priority, reason });
        }

        function fetchDashboardSummary(forceRender = false, reason = "dashboard") {
            return requestStatusRefresh(["dashboard"], forceRender ? "immediate" : "deferable", reason);
        }

        function executeStatusSnapshotFetch(effect) {
            if (effect.stream === "servers") return executeServersSnapshotFetch(effect);
            if (effect.stream === "dashboard") return executeDashboardSnapshotFetch(effect);
            return Promise.resolve();
        }

        async function executeDashboardSnapshotFetch(effect) {
            try {
                const response = await fetch('/api/dashboard/summary?window=7d');
                if (!response.ok) throw new Error(`HTTP ${response.status}`);
                const nextDashboardSummary = await response.json();
                setDashboardExtraError("dashboard", null);
                await dispatchStatusInteraction({
                    type: "dashboardSnapshotReceived",
                    requestId: effect.requestId,
                    snapshot: nextDashboardSummary
                });
            } catch (err) {
                setDashboardExtraError("dashboard", err);
                await dispatchStatusInteraction({
                    type: "snapshotFailed",
                    stream: "dashboard",
                    requestId: effect.requestId,
                    error: err?.message || String(err)
                });
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
                    dispatchStatusInteraction({ type: "globalKeyAvailabilityChanged", available: globalKeyAvailable });
                    refreshDashboardPresentation();
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

        function fetchDashboardExtras(reason = "extras", includeDashboard = true) {
            fetchGlobalKeyStatus();
            fetchRecentActivity();
            fetchObservabilitySummary();
            fetchPolicySummary();
            if (includeDashboard) fetchDashboardSummary(false, reason);
        }

        const statusTransport = window.StatusTransport.createController({
            EventSourceType: window.EventSource,
            onServersPoll: () => fetchServers(false, "poll"),
            onExtrasPoll: () => fetchDashboardExtras("poll"),
            onDashboardEvent: () => {
                requestStatusRefresh(["servers", "dashboard"], "immediate", "sse");
                fetchDashboardExtras("sse", false);
            },
            onConnectionChanged: () => renderSyncState()
        });

	        function updateRefreshAllFactsState() {
	            const button = document.getElementById('refresh-all-facts');
	            if (!button) return;
	            const view = getStatusView();
	            const plan = statusInteraction.planBulkAction("refresh_facts", { actionLabel: "refresh facts", preview: true });
	            const visibleCount = plan.visibleNames.length;
	            const refreshableCount = plan.eligibleNames.length;
	            const selectedCount = plan.selectedNames.length;
	            const bulk = view.actions.bulk;
	            const refreshing = bulk?.actionKey === "refresh_facts";
	            const enabled = !bulk && refreshableCount > 0;
	            button.disabled = !enabled;
	            button.textContent = refreshing ? "Refreshing..." : "Refresh facts";
	            button.classList.toggle("refreshing", refreshing);
	            button.setAttribute('aria-label', refreshing ? "Refreshing facts for visible selected hosts" : "Refresh facts for visible selected hosts");
	            button.setAttribute('aria-describedby', 'bulk-action-hint');
	            button.title = refreshing
	                ? `Refreshing facts for ${pluralize(refreshableCount, "visible selected host")}`
	                : bulk
	                ? `Bulk ${bulk.actionLabel} is running`
	                : enabled
	                ? `Refresh facts for ${pluralize(refreshableCount, "visible selected host")}`
	                : selectedCount === 0
	                ? "Select visible hosts first"
	                : visibleCount === 0
	                ? "No selected host is visible in the current filter"
	                : "No visible selected host can refresh facts right now";
	        }

        function saveWindowScroll() {
            return { x: window.scrollX, y: window.scrollY };
        }

        function restoreWindowScroll(pos) {
            if (!pos) return;
            window.scrollTo(pos.x, pos.y);
        }

	        function renderServerState() {
	            const view = getStatusView();
	            allServers = view.servers;
	            refreshDashboardPresentation();
	            const pageScroll = saveWindowScroll();
	            renderDashboardMetrics();
	            renderTable();
	            renderDrawer();
	            requestAnimationFrame(() => restoreWindowScroll(pageScroll));
	        }
        function beginActionInteraction() {
            dispatchStatusInteraction({ type: "interactionStarted" });
        }

        function endActionInteraction() {
            dispatchStatusInteraction({ type: "interactionEnded", delayMs: actionInteractionDeferMs });
        }

        function resetActionInteraction() {
            dispatchStatusInteraction({ type: "interactionReset" });
        }

        function isServerActionControl(target) {
            return !!target?.closest?.([
                'button[data-action]',
                '#bulk-update',
                '#bulk-approve',
                '#bulk-approve-security',
                '#bulk-approve-kept-security',
                '#bulk-cancel',
                '#bulk-autoremove',
                '#refresh-all-facts',
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
            const sort = getStatusView().sort;
            document.querySelectorAll('#servers-table th.sortable').forEach((th) => {
                if (th.dataset.sortKey === sort.key) {
                    th.dataset.sortDir = sort.dir;
                    th.setAttribute('aria-sort', sort.dir === "asc" ? "ascending" : "descending");
                    const indicator = th.querySelector('.sort-indicator');
                    if (indicator) indicator.textContent = sort.dir === "asc" ? "▲" : "▼";
                } else {
                    delete th.dataset.sortDir;
                    th.setAttribute('aria-sort', 'none');
                    const indicator = th.querySelector('.sort-indicator');
                    if (indicator) indicator.textContent = "";
                }
            });
        }

        function loadColumnWidths() {
            return statusTableAdapter.load();
        }

        function saveColumnWidths(widths) {
            statusTableAdapter.save(widths);
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
                const nextWidth = statusTableAdapter.boundedWidth(configured, minWidth, maxWidth, boundedFallback);
                col.style.width = `${nextWidth}px`;
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

        function fetchServers(forceRender = false, reason = "servers") {
            return requestStatusRefresh(["servers"], forceRender ? "immediate" : "deferable", reason);
        }

        async function executeServersSnapshotFetch(effect) {
            try {
                const response = await fetch('/api/servers');
                if (!response.ok) {
                    throw new Error(`Failed to fetch servers: HTTP ${response.status}`);
                }
                const parsedServers = await response.json();
                if (!Array.isArray(parsedServers)) {
                    throw new Error('Invalid servers payload: expected an array');
                }
                lastFetchError = null;
                lastSuccessfulSyncAt = new Date();
                await dispatchStatusInteraction({
                    type: "serversSnapshotReceived",
                    requestId: effect.requestId,
                    servers: parsedServers
                });
            } catch (err) {
                console.error('Unable to refresh servers list:', err);
                lastFetchError = err;
                await dispatchStatusInteraction({
                    type: "snapshotFailed",
                    stream: "servers",
                    requestId: effect.requestId,
                    error: err?.message || String(err)
                });
            }
        }

        function loadDashboardFilters() {
            let saved = {};
            try {
                const raw = localStorage.getItem(dashboardFilterStorageKey);
                if (raw) saved = JSON.parse(raw);
	            } catch (_) {
	                // Ignore invalid saved dashboard state.
            }
            dispatchStatusInteraction({ type: "navigationRestored", value: saved });
            const view = getStatusView();
            document.getElementById("search").value = view.filters.search;
            document.getElementById("status-filter").value = view.filters.status;
            document.getElementById("auth-filter").value = view.filters.auth;
            document.getElementById("group-by").value = view.filters.groupBy;
            document.getElementById("page-size").value = String(view.pageSize);
        }

        function persistDashboardFilters(value) {
            try {
                localStorage.setItem(dashboardFilterStorageKey, JSON.stringify(value));
	            } catch (_) {
	                // Ignore storage failures.
	            }
	        }

	        function applyFleetQuickFilter(key) {
	            dispatchStatusInteraction({ type: "filtersChanged", patch: { quick: key || "" } });
	            renderTable({ refreshPanels: false });
	        }

		        function isServerActionBusy(server) {
		            return !!serverPresentation(server)?.busy;
		        }

	        async function runSingleHostAction(name, actionKey, actionLabel, work, refreshStreams = ["servers"]) {
	            const plan = statusInteraction.planAction(name, actionKey, { actionLabel });
	            if (!plan.enabled) return false;
	            await dispatchStatusInteraction({ type: "actionStarted", plan });
	            const started = getStatusView().actions.inFlight.some(action => action.operationId === plan.id);
	            if (!started) return false;
	            try {
	                const result = await work(plan);
	                await dispatchStatusInteraction({
	                    type: result === false ? "actionFailed" : "actionCompleted",
	                    operationId: plan.id,
	                    refreshStreams: result === false ? [] : refreshStreams
	                });
	                return result !== false;
	            } catch (error) {
	                await dispatchStatusInteraction({ type: "actionFailed", operationId: plan.id, refreshStreams: [] });
	                throw error;
	            }
	        }

	        function canRunUpdateAction(server) {
	            return !!serverPresentation(server)?.canRunUpdate;
	        }

	        function canRunAutoremoveAction(server) {
	            return !!serverPresentation(server)?.canRunAutoremove;
	        }

	        function canRunSudoersAction(server) {
	            return !!serverPresentation(server)?.canRunSudoers;
	        }

	        function canRefreshFactsAction(server) {
	            return !!serverPresentation(server)?.canRefreshFacts;
	        }

        function updateSelectPageState() {
            const selectAll = document.getElementById('select-all');
            if (!selectAll) return;
            const rowCheckboxes = Array.from(document.querySelectorAll('#servers-table tbody tr[data-name] .row-select'));
            const selectedOnPage = rowCheckboxes.filter(cb => cb.checked).length;
            const allSelected = rowCheckboxes.length > 0 && selectedOnPage === rowCheckboxes.length;
            const partiallySelected = selectedOnPage > 0 && selectedOnPage < rowCheckboxes.length;
            selectAll.checked = allSelected;
            selectAll.indeterminate = partiallySelected;
            selectAll.setAttribute('aria-checked', partiallySelected ? "mixed" : allSelected ? "true" : "false");
            selectAll.dataset.selectionState = partiallySelected ? "mixed" : allSelected ? "checked" : "unchecked";
        }

        function scheduleSelectPageStateUpdate() {
            updateSelectPageState();
            window.setTimeout(updateSelectPageState, 0);
        }


        function openDrawer(name, tab = "logs") {
            const previousDrawer = getStatusView().drawer;
            if (previousDrawer.serverName !== name) {
                drawerLogScrollTop = 0;
                drawerPendingScrollTop = 0;
            }
            dispatchStatusInteraction({ type: "drawerOpened", name, tab });
            renderDrawer();
            statusDrawerAdapter.open(
                document.getElementById('status-drawer'),
                document.getElementById('status-drawer-backdrop')
            );
        }

        function closeDrawer() {
            dispatchStatusInteraction({ type: "drawerClosed" });
            const drawer = document.getElementById('status-drawer');
            const backdrop = document.getElementById('status-drawer-backdrop');
            statusDrawerAdapter.close(drawer, backdrop);
        }

        function setDrawerTab(tab) {
            if (tab !== "logs" && tab !== "pending") return;
            dispatchStatusInteraction({ type: "drawerTabChanged", tab });
            renderDrawer();
        }

	        function renderDrawer() {
            const drawerState = getStatusView().drawer;
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

            if (!drawerState.open) {
                drawer.classList.remove('open');
                backdrop.classList.remove('open');
                drawer.setAttribute('aria-hidden', 'true');
                return;
            }

            const server = getServerByName(drawerState.serverName);
            if (!server) {
                closeDrawer();
                return;
            }

            const safeStatus = safeStatusClass(server.status);
            const safeStatusText = escapeHtml(server.status || "unknown");
            const isPendingApproval = server.status === "pending_approval";
            const hasPending = hasPendingUpdates(server);
            const driftReason = pendingApprovalDriftReason(server);
            const triage = getServerApprovalTriage(server);
            const approvalCounts = getPendingApprovalCounts(server);
            const keptBackSecurityCount = Number(triage.kept_back_security_updates ?? approvalCounts.keptBackSecurity ?? 0);
            const canApproveKeptBackSecurity = !!triage.can_approve_kept_back_security;
            const securityApprovalLabel = approvalCounts.security === null
                ? "Standard security (?)"
                : `Standard security (${approvalCounts.security})`;
            if (drawerState.tab === "pending" && !hasPending) {
                drawerPendingScrollTop = 0;
            }

            title.textContent = server.name || "Server details";
	            statusContainer.innerHTML = `
	                <span class="status-pill status-${safeStatus}">${safeStatusText}</span>
	                ${driftReason ? `<span class="drawer-status-note pending-drift-note" title="${escapeHtml(driftReason)}">${escapeHtml(driftReason)}</span>` : ""}
	            `;
	            approvalActions.classList.toggle('hidden', !isPendingApproval);
	            drawerApproveAllBtn.textContent = `Approve (${approvalCounts.standard})`;
	            drawerApproveAllBtn.disabled = !triage.can_approve_all;
	            drawerApproveSecurityBtn.textContent = securityApprovalLabel;
	            drawerApproveSecurityBtn.disabled = !triage.can_approve_security;
            drawerApproveKeptBackSecurityBtn.textContent = `Kept-back security (${keptBackSecurityCount})`;
            drawerApproveKeptBackSecurityBtn.disabled = !canApproveKeptBackSecurity;
            drawerApproveKeptBackSecurityBtn.classList.toggle('hidden', !canApproveKeptBackSecurity);
            drawerApproveFullBtn.textContent = `Full upgrade (${approvalCounts.full})`;
            drawerApproveFullBtn.disabled = !triage.can_approve_full;
            drawerApproveFullBtn.classList.toggle('hidden', !triage.can_approve_full);

            pendingTabBtn.disabled = !hasPending;
            pendingTabBtn.classList.toggle('active', drawerState.tab === "pending");
            logsTabBtn.classList.toggle('active', drawerState.tab === "logs");

            logsPanel.classList.toggle('active', drawerState.tab === "logs");
            pendingPanel.classList.toggle('active', drawerState.tab === "pending");

            if (drawerState.tab === "logs") {
                logsEl.innerHTML = formatLogsHtml(server.logs || "");
                if (drawerState.logFollow) {
                    logsEl.scrollTop = logsEl.scrollHeight;
                } else {
                    logsEl.scrollTop = drawerLogScrollTop;
                }
                logsHint.textContent = drawerState.logFollow ? "Live auto-scroll" : "Auto-scroll paused";
            }

            if (drawerState.tab === "pending") {
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

        function hidePhaseTooltip() {
            const tooltip = document.getElementById('phase-tooltip');
            if (!tooltip) return;
            if (activePhaseTooltipTarget) {
                activePhaseTooltipTarget.removeAttribute('aria-describedby');
            }
            activePhaseTooltipTarget = null;
            tooltip.classList.add('hidden');
            tooltip.innerHTML = "";
        }

        function positionPhaseTooltip(target) {
            const tooltip = document.getElementById('phase-tooltip');
            if (!tooltip || !target) return;
            const rect = target.getBoundingClientRect();
            const tooltipRect = tooltip.getBoundingClientRect();
            const margin = 10;
            const left = Math.min(
                window.innerWidth - tooltipRect.width - margin,
                Math.max(margin, rect.left + rect.width / 2 - tooltipRect.width / 2)
            );
            const preferredTop = rect.top - tooltipRect.height - 8;
            const top = preferredTop > margin ? preferredTop : rect.bottom + 8;
            tooltip.style.left = `${Math.round(left)}px`;
            tooltip.style.top = `${Math.round(Math.min(window.innerHeight - tooltipRect.height - margin, Math.max(margin, top)))}px`;
        }

        function showPhaseTooltip(target) {
            const tooltip = document.getElementById('phase-tooltip');
            if (!tooltip || !target) return;
            const label = target.dataset.phaseLabel || "Phase";
            const state = target.dataset.phaseState || "unknown";
            const summary = target.dataset.phaseSummary || "No phase detail";
            const time = target.dataset.phaseTime || "";
            const progress = target.dataset.phaseProgress || "0";
            tooltip.innerHTML = `
                <strong>${escapeHtml(label)} · ${escapeHtml(state)}</strong>
                <span>${escapeHtml(summary)}</span>
                <span>${escapeHtml([`${progress}%`, time].filter(Boolean).join(" · "))}</span>
            `;
            tooltip.classList.remove('hidden');
            activePhaseTooltipTarget = target;
            target.setAttribute('aria-describedby', 'phase-tooltip');
            positionPhaseTooltip(target);
        }

        function renderTable(options = {}) {
            hidePhaseTooltip();
            refreshDashboardPresentation();
            const tbody = document.querySelector('#servers-table tbody');
            tbody.innerHTML = '';
            const view = getStatusView();
            const totalFiltered = view.visibleServers.length;
            const selectedNames = new Set(view.selectedNames);
            document.getElementById('page-info').textContent = `Page ${view.page} of ${view.totalPages} (${pluralize(totalFiltered, "host")})`;
            document.querySelector('.pagination')?.classList.toggle('single-page', view.totalPages <= 1);
            document.getElementById('prev-page').disabled = view.page <= 1;
            document.getElementById('next-page').disabled = view.page >= view.totalPages;
            setText(
                "table-summary",
                allServers.length === 0
                    ? "Waiting for status data"
                    : `${pluralize(totalFiltered, "host")} visible · ${pluralize(allServers.length, "host")} loaded`
            );
            dashboardPresentation.groups.forEach(group => {
                if (group.key) {
                    const groupRow = document.createElement('tr');
                    groupRow.className = 'group-row';
                    groupRow.innerHTML = `<td colspan="11">${escapeHtml(group.key)}</td>`;
                    tbody.appendChild(groupRow);
                }
                group.items.forEach(presentation => {
                    const server = presentation.server;
                    const row = document.createElement('tr');
                    row.dataset.name = server.name;
                    const rowSelected = view.primaryServerName === server.name;
                    row.setAttribute("aria-selected", rowSelected ? "true" : "false");
                    if (rowSelected) {
                        row.classList.add('row-selected');
                    }
                    if (hoveredName === server.name) {
                        row.classList.add('row-hover');
                    }
	                    const isBusy = isServerActionBusy(server);
                    const safeNameHtml = escapeHtml(server.name);
                    const safeStatusText = escapeHtml(statusLabel(server.status));
                    const safeStatus = safeStatusClass(server.status);
                    const safeDataName = escapeHtml(server.name);
	                    const intelligence = presentation.intelligence;
	                    const timeline = presentation.timeline;
	                    const triage = presentation.triage;
	                    const lastUpdate = intelligence?.last_update;
	                    const nextRun = intelligence?.next_run;
	                    const lastUpdateLabel = lastUpdate ? `${formatRelativeTimestamp(lastUpdate.finished_at)} · ${formatDuration(lastUpdate.duration_ms)}` : "No history";
	                    const nextRunLabel = nextRun?.state === "scheduled"
	                        ? (nextRun.scheduled_for_display || nextRun.scheduled_for_utc || "Scheduled")
	                        : "None";
	                    const timelineWindow = timeline?.updated_at_display || timeline?.updated_at || (nextRun?.state === "scheduled" ? nextRunLabel : lastUpdateLabel);
	                    const timelineSummary = timeline.summary || timelineWindow || "No activity";
	                    const approvalCounts = presentation.approvalCounts;
	                    const keptBackSecurityCount = Number(triage.kept_back_security_updates ?? approvalCounts.keptBackSecurity ?? 0);
	                    const canApproveKeptBackSecurity = !!triage.can_approve_kept_back_security;
	                    const canApproveAll = !!triage.can_approve_all;
	                    const canApproveSecurity = !!triage.can_approve_security;
	                    const canUpdate = presentation.canRunUpdate;
	                    const failureReason = presentation.failureReason;
	                    const driftReason = presentation.driftReason;
	                    const failureReasonIsDuplicate = failureReason && String(failureReason).trim().toLowerCase() === String(timelineSummary).trim().toLowerCase();
	                    const failureReasonHtml = failureReason && !failureReasonIsDuplicate
	                        ? `<span class="failure-reason" title="${escapeHtml(failureReason)}">${escapeHtml(failureReason)}</span>`
	                        : "";
	                    const driftReasonHtml = driftReason
	                        ? `<span class="pending-drift-row" title="${escapeHtml(driftReason)}">${escapeHtml(driftReason)}</span>`
	                        : "";
                    const securityApprovalLabel = approvalCounts.security === null
                        ? "Standard security (?)"
                        : `Standard security (${approvalCounts.security})`;
                    const keptBackSecurityButton = canApproveKeptBackSecurity
                        ? `<button type="button" class="btn-security" data-action="approve-security-kept-back" data-name="${safeDataName}" title="Approve only kept-back security updates">Kept-back security (${keptBackSecurityCount})</button>`
                        : "";
                    const fullApprovalButton = triage.can_approve_full
                        ? `<button type="button" class="btn-full-upgrade" data-action="approve-full" data-name="${safeDataName}" title="Run apt full-upgrade">Full upgrade (${approvalCounts.full})</button>`
                        : "";
                    const actionButtons = server.status === 'pending_approval'
	                        ? `
	                            <div class="actions-grid timeline-actions">
	                                <button type="button" data-action="approve-all" data-name="${safeDataName}" ${buttonStateAttrs(canApproveAll, "Approve standard updates", "No standard updates are eligible")}>Approve (${approvalCounts.standard})</button>
	                                <button type="button" class="btn-security" data-action="approve-security" data-name="${safeDataName}" ${buttonStateAttrs(canApproveSecurity, "Approve only standard security updates", "No standard security updates are eligible")}>${securityApprovalLabel}</button>
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
		                        : driftReason
		                            ? `
		                                <div class="actions-grid timeline-actions timeline-actions-note">
		                                    <span class="action-note pending-drift-note" title="${escapeHtml(driftReason)}">Runtime not pending</span>
		                                    ${canUpdate ? `<button type="button" data-action="update-server" data-name="${safeDataName}" title="Run fresh update checks">Update</button>` : ""}
		                                    ${hasPendingUpdates(server) ? `<button type="button" class="btn-ghost" data-action="open-drawer" data-name="${safeDataName}" data-tab="pending">Packages</button>` : ""}
		                                    <button type="button" class="btn-ghost" data-action="open-drawer" data-name="${safeDataName}" data-tab="logs">Logs</button>
		                                </div>
	                              `
	                        : `
	                            <div class="actions-grid timeline-actions">
	                                <button type="button" data-action="update-server" data-name="${safeDataName}" ${canUpdate ? "" : "disabled"} title="${canUpdate ? "Run update checks" : "Host cannot run update checks right now"}">Update</button>
	                                <button type="button" class="btn-ghost" data-action="open-drawer" data-name="${safeDataName}" data-tab="logs">Logs</button>
	                            </div>
	                          `;
                    row.innerHTML = `
	                        <td class="select-col"><input type="checkbox" class="row-select" data-name="${safeDataName}" aria-label="Select ${safeNameHtml}" ${selectedNames.has(server.name) ? "checked" : ""}></td>
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
	                            <span>${escapeHtml(timelineSummary)}</span>
	                            ${failureReasonHtml}
	                            ${driftReasonHtml}
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
	                    dispatchStatusInteraction({ type: "selectionChanged", name, selected: e.target.checked });
	                    updateBulkActionState();
	                });
	            });
		            updateBulkActionState();
		            updateSortIndicators();
		            if (options.refreshPanels === false) {
		                renderTableDependentPanels();
		            } else {
		                renderDashboardPanels();
		            }
	        }

        function getServerByName(name) {
            return allServers.find(server => server.name === name);
        }

	        function selectServer(name) {
		            dispatchStatusInteraction({ type: "primaryServerSelected", name: name || "" });
		            renderTable({ refreshPanels: false });
		        }

        async function copyLogs(name = "") {
            name = name || getStatusView().drawer.serverName;
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

        function downloadLogs(name = "") {
            name = name || getStatusView().drawer.serverName;
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
	            const phaseDot = e.target.closest('.timeline-dot');
	            if (phaseDot) {
	                showPhaseTooltip(phaseDot);
	            }
	            const row = e.target.closest('tr[data-name]');
	            if (!row) return;
	            hoveredName = row.dataset.name || null;
	            applyHoverClass();
	        });
	        tbodyHover.addEventListener('mouseout', (e) => {
	            const phaseDot = e.target.closest('.timeline-dot');
	            if (phaseDot && !phaseDot.contains(e.relatedTarget)) {
	                hidePhaseTooltip();
	            }
	        });
	        tbodyHover.addEventListener('focusin', (e) => {
	            const phaseDot = e.target.closest('.timeline-dot');
	            if (phaseDot) {
	                showPhaseTooltip(phaseDot);
	            }
	        });
	        tbodyHover.addEventListener('focusout', (e) => {
	            const phaseDot = e.target.closest('.timeline-dot');
	            if (phaseDot && !phaseDot.contains(e.relatedTarget)) {
	                hidePhaseTooltip();
	            }
	        });
	        tbodyHover.addEventListener('mouseleave', () => {
	            hoveredName = null;
	            applyHoverClass();
	            hidePhaseTooltip();
	        });
	        window.addEventListener('scroll', hidePhaseTooltip, true);
	        window.addEventListener('resize', () => {
	            if (activePhaseTooltipTarget) {
	                positionPhaseTooltip(activePhaseTooltipTarget);
	            }
	        });

        const triageTable = document.getElementById('approval-triage-table');
	        if (triageTable) {
	            triageTable.addEventListener('click', (e) => {
	                const button = e.target.closest('button[data-action]');
	                if (button) {
	                    handleServerAction(button.dataset.action || "", button.dataset.name || "", button.dataset.tab || "logs");
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
	                    renderApprovalTriage();
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
	                    applyFleetQuickFilter(filterButton.dataset.fleetFilter || "");
	                    return;
	                }
	                const tagButton = e.target.closest('button[data-fleet-tag]');
	                if (tagButton) {
	                    dispatchStatusInteraction({ type: "filtersChanged", patch: { tag: tagButton.dataset.fleetTag || "" } });
	                    renderTable({ refreshPanels: false });
	                }
	            });
	        }

	        const metricStrip = document.querySelector('.metric-strip');
	        if (metricStrip) {
	            metricStrip.addEventListener('click', (e) => {
	                const button = e.target.closest('button[data-metric-filter]');
	                if (!button) return;
	                applyFleetQuickFilter(button.dataset.metricFilter || "");
	            });
	        }

        const applySortFromHeader = (th) => {
            if (!th) return;
            if (Date.now() < suppressSortClickUntil) {
                return;
            }
            const key = th.dataset.sortKey;
            dispatchStatusInteraction({ type: "sortChanged", key });
            updateSortIndicators();
            renderTable({ refreshPanels: false });
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

        document.getElementById('search').addEventListener('input', (event) => {
            dispatchStatusInteraction({ type: "filtersChanged", patch: { search: event.target.value } });
            renderTable({ refreshPanels: false });
        });
        document.getElementById('status-filter').addEventListener('change', (event) => {
            dispatchStatusInteraction({ type: "filtersChanged", patch: { status: event.target.value } });
            renderTable({ refreshPanels: false });
        });
        document.getElementById('auth-filter').addEventListener('change', (event) => {
            dispatchStatusInteraction({ type: "filtersChanged", patch: { auth: event.target.value } });
            renderTable({ refreshPanels: false });
        });
        document.getElementById('group-by').addEventListener('change', (event) => {
            dispatchStatusInteraction({ type: "filtersChanged", patch: { groupBy: event.target.value } });
            renderTable({ refreshPanels: false });
        });
        document.getElementById('page-size').addEventListener('change', (event) => {
            dispatchStatusInteraction({ type: "filtersChanged", patch: { pageSize: event.target.value } });
            renderTable({ refreshPanels: false });
        });

        document.getElementById('prev-page').addEventListener('click', () => {
            dispatchStatusInteraction({ type: "pageChanged", delta: -1 });
            renderTable({ refreshPanels: false });
        });
        document.getElementById('next-page').addEventListener('click', () => {
            dispatchStatusInteraction({ type: "pageChanged", delta: 1 });
            renderTable({ refreshPanels: false });
        });

        document.getElementById('select-all').addEventListener('change', (e) => {
	            dispatchStatusInteraction({ type: "pageSelectionChanged", selected: e.target.checked });
	            renderTable({ refreshPanels: false });
	            updateBulkActionState();
	        });


        async function postServerAction(url, fallbackMessage, options = {}) {
            try {
                const response = await fetch(url, { method: 'POST', ...options });
                if (!response.ok) {
                    statusActionAdapter.notify(await parseErrorResponse(response, fallbackMessage));
                    return false;
                }
                return true;
            } catch (error) {
                statusActionAdapter.notify(error?.message || fallbackMessage);
                return false;
            }
        }

	        async function updateServer(name) {
	            await runSingleHostAction(name, "update", "update", () => (
	                postServerAction(`/api/update/${encodeURIComponent(name)}`, 'Failed to start update.')
	            ));
	        }

	        async function runAutoremove(name) {
	            await runSingleHostAction(name, "autoremove", "autoremove", () => (
	                postServerAction(`/api/autoremove/${encodeURIComponent(name)}`, 'Failed to start apt autoremove.')
	            ));
	        }

	        async function enablePasswordlessApt(name) {
	            if (!canRunSudoersAction(getServerByName(name))) {
	                statusActionAdapter.notify("Host cannot change passwordless apt while another action is active.");
	                return;
	            }
	            await runSingleHostAction(name, "enable_apt", "enable apt", async () => {
	                let password;
	                try {
	                    password = await promptPassword(`Enter sudo password for ${name}`);
	                } catch {
	                    return false;
	                }
	                if (!password) return false;
	                return postServerAction(`/api/sudoers/${encodeURIComponent(name)}`, 'Failed to enable passwordless apt.', {
	                    headers: { 'Content-Type': 'application/json' },
	                    body: JSON.stringify({ password })
	                });
	            });
	        }

	        async function disablePasswordlessApt(name) {
	            if (!canRunSudoersAction(getServerByName(name))) {
	                statusActionAdapter.notify("Host cannot change passwordless apt while another action is active.");
	                return;
	            }
	            await runSingleHostAction(name, "disable_apt", "disable apt", async () => {
	                let password;
	                try {
	                    password = await promptPassword(`Enter sudo password to disable for ${name}`);
	                } catch {
	                    return false;
	                }
	                if (!password) return false;
	                return postServerAction(`/api/sudoers/disable/${encodeURIComponent(name)}`, 'Failed to disable passwordless apt.', {
	                    headers: { 'Content-Type': 'application/json' },
	                    body: JSON.stringify({ password })
	                });
	            });
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
            if (!getStatusView().drawer.open) return false;
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
            const bulkReviewBackdrop = document.getElementById('bulk-review-modal');
            if (bulkReviewBackdrop && bulkReviewBackdrop.classList.contains('active')) {
                if (e.key === 'Tab') {
                    if (trapBulkReviewModalFocus(e)) {
                        e.stopImmediatePropagation();
                    }
                    return;
                }
                if (e.key === 'Escape') {
                    e.preventDefault();
                    e.stopImmediatePropagation();
                    closeBulkReviewModal(false);
                    return;
                }
            }
            if (e.key === 'Tab' && trapDrawerFocus(e)) {
                e.stopImmediatePropagation();
                return;
            }
            if (e.key === 'Escape' && getStatusView().drawer.open) {
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
            const name = getStatusView().drawer.serverName;
            if (!name) return;
            approveAllUpdates(name);
        });
        document.getElementById('drawer-approve-security').addEventListener('click', () => {
            const name = getStatusView().drawer.serverName;
            if (!name) return;
            approveSecurityUpdates(name);
        });
        document.getElementById('drawer-approve-security-kept-back').addEventListener('click', () => {
            const name = getStatusView().drawer.serverName;
            if (!name) return;
            approveKeptBackSecurityUpdates(name);
        });
        document.getElementById('drawer-approve-full').addEventListener('click', () => {
            const name = getStatusView().drawer.serverName;
            if (!name) return;
            approveFullUpgrade(name);
        });

        const drawerLogsElement = document.getElementById('drawer-logs');
        drawerLogsElement.addEventListener('scroll', () => {
            drawerLogScrollTop = drawerLogsElement.scrollTop;
            const logFollow = isNearBottom(drawerLogsElement);
            dispatchStatusInteraction({ type: "drawerLogFollowChanged", value: logFollow });
            document.getElementById('drawer-logs-hint').textContent = logFollow ? "Live auto-scroll" : "Auto-scroll paused";
        });

        const drawerPendingElement = document.getElementById('drawer-panel-pending');
        drawerPendingElement.addEventListener('scroll', () => {
            if (getStatusView().drawer.tab === "pending") {
                drawerPendingScrollTop = drawerPendingElement.scrollTop;
            }
        });

        document.addEventListener('pointerdown', (event) => {
            if (isServerActionControl(event.target)) {
                beginActionInteraction();
            }
        }, true);
        document.addEventListener('pointerup', () => {
            if (getStatusView().sync.interactionDepth > 0) {
                endActionInteraction();
            }
        }, true);
        document.addEventListener('pointercancel', () => {
            if (getStatusView().sync.interactionDepth > 0) {
                endActionInteraction();
            }
        }, true);
	        document.addEventListener('keydown', (event) => {
	            if (event.key === "Escape") {
	                hidePhaseTooltip();
	            }
	            if (event.repeat || (event.key !== "Enter" && event.key !== " ")) return;
	            if (isServerActionControl(event.target)) {
	                beginActionInteraction();
            }
        }, true);
        document.addEventListener('keyup', (event) => {
            if (event.key !== "Enter" && event.key !== " ") return;
            if (getStatusView().sync.interactionDepth > 0) {
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
	            await runSingleHostAction(name, "approve_all", "approve", async () => {
	                const server = getServerByName(name);
	                if (!getServerApprovalTriage(server, { ignoreInFlight: true }).can_approve_all) {
	                    statusActionAdapter.notify("No standard updates are eligible for approval.");
	                    return false;
	                }
	                return postServerAction(`/api/approve/${encodeURIComponent(name)}`, 'Failed to approve updates.');
	            });
	        }

	        async function approveSecurityUpdates(name) {
	            await runSingleHostAction(name, "approve_security", "approve security", async () => {
	                const server = getServerByName(name);
	                if (!getServerApprovalTriage(server, { ignoreInFlight: true }).can_approve_security) {
	                    statusActionAdapter.notify("No standard security updates are eligible for approval.");
	                    return false;
	                }
	                return postServerAction(`/api/approve-security/${encodeURIComponent(name)}`, 'Failed to approve security updates.');
	            });
	        }

	        async function approveKeptBackSecurityUpdates(name) {
	            await runSingleHostAction(name, "approve_security_kept_back", "approve kept-back security", async () => {
	                const server = getServerByName(name);
	                const counts = getPendingApprovalCounts(server);
	                const triage = getServerApprovalTriage(server, { ignoreInFlight: true });
	                if (!triage.can_approve_kept_back_security) {
	                    if (!counts.keptBackSecurityPlanAvailable) {
	                        statusActionAdapter.notify("Run a fresh package scan before approving kept-back security updates.");
	                        return false;
	                    }
	                    statusActionAdapter.notify("No kept-back security updates are eligible for approval.");
	                    return false;
	                }
	                if (!counts.keptBackSecurityPlanAvailable) {
	                    statusActionAdapter.notify("Run a fresh package scan before approving kept-back security updates.");
	                    return false;
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
	                if (!await statusActionAdapter.confirm(confirmText)) return false;
	                const body = removed.length ? { confirm_removals: true } : {};
	                return postServerAction(`/api/approve-security-kept-back/${encodeURIComponent(name)}`, 'Failed to approve kept-back security updates.', {
	                    headers: { 'Content-Type': 'application/json' },
	                    body: JSON.stringify(body)
	                });
	            });
	        }

	        async function approveFullUpgrade(name) {
	            await runSingleHostAction(name, "approve_full", "approve full upgrade", async () => {
	                const server = getServerByName(name);
	                const counts = getPendingApprovalCounts(server);
	                const triage = getServerApprovalTriage(server, { ignoreInFlight: true });
	                if (!triage.can_approve_full) {
	                    if (!counts.fullPlanAvailable) {
	                        statusActionAdapter.notify("Run a fresh package scan before approving full-upgrade.");
	                        return false;
	                    }
	                    statusActionAdapter.notify("No full-upgrade packages are eligible for approval.");
	                    return false;
	                }
	                if (!counts.fullPlanAvailable) {
	                    statusActionAdapter.notify("Run a fresh package scan before approving full-upgrade.");
	                    return false;
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
	                if (!await statusActionAdapter.confirm(confirmText)) return false;
	                const body = removed.length ? { confirm_removals: true } : {};
	                return postServerAction(`/api/approve-full/${encodeURIComponent(name)}`, 'Failed to approve full upgrade.', {
	                    headers: { 'Content-Type': 'application/json' },
	                    body: JSON.stringify(body)
	                });
	            });
	        }

	        async function cancelUpgrade(name) {
	            await runSingleHostAction(name, "cancel", "cancel", () => (
	                postServerAction(`/api/cancel/${encodeURIComponent(name)}`, 'Failed to cancel upgrade.')
	            ));
	        }

	        async function refreshHostFacts(name) {
	            await runSingleHostAction(name, "refresh_facts", "refresh facts", async () => {
	                try {
	                    const response = await fetch(`/api/servers/${encodeURIComponent(name)}/facts/refresh`, { method: 'POST' });
	                    if (!response.ok) {
	                        const payload = await response.json().catch(() => ({}));
	                        statusActionAdapter.notify(payload.error || "Failed to refresh host facts");
	                        return false;
	                    }
	                    return true;
	                } catch (err) {
	                    statusActionAdapter.notify(err?.message || "Failed to refresh host facts");
	                    return false;
	                }
	            }, ["servers", "dashboard"]);
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
            const hostName = details.dataset.name || getStatusView().primaryServerName;
            if (!hostName) return;
            if (details.open) {
                expandedHostFactsServers.add(hostName);
            } else {
                expandedHostFactsServers.delete(hostName);
            }
        }, true);

	        document.querySelectorAll('.operations-grid, .context-ops-grid, .priority-attention-strip').forEach((panel) => {
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
        statusTransport.start();
        fetchDashboardExtras();
        fetchServers();
