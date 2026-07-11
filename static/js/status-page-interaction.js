(function initStatusPageInteraction(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) {
        module.exports = api;
    }
    if (root) {
        root.StatusPageInteraction = api;
        root.statusPageInteraction = root.statusPageInteraction || api.createStore();
    }
}(typeof globalThis !== "undefined" ? globalThis : this, function statusPageInteractionFactory() {
    "use strict";

    const transientBlockingStatuses = new Set([
        "updating",
        "pending_approval",
        "approved",
        "upgrading",
        "autoremove",
        "sudoers",
        "facts_refresh"
    ]);

    const legacyActionFields = Object.freeze({
        update: "can_run_checks",
        approve_all: "can_approve_all",
        approve_security: "can_approve_security",
        approve_security_kept_back: "can_approve_kept_back_security",
        approve_full: "can_approve_full",
        cancel: "can_cancel",
        refresh_facts: "can_refresh_facts"
    });

    const allowedStatusFilters = new Set(["", "idle", "updating", "pending_approval", "upgrading", "autoremove", "done", "error"]);
    const allowedAuthFilters = new Set(["", "password", "key"]);
    const allowedGroupings = new Set(["", "status", "tag"]);
    const allowedQuickFilters = new Set(["", "pending_approval", "active", "stale_facts", "high_risk"]);
    const dashboardActionKeys = ["update", "autoremove", "enable_apt", "disable_apt", "refresh_facts", "approve_all", "approve_security", "approve_security_kept_back", "approve_full", "cancel"];
    const allowedPageSizes = new Set([25, 50, 100]);
    const activeStatuses = new Set(["updating", "upgrading", "autoremove", "sudoers", "facts_refresh"]);
    const refreshPriority = Object.freeze({ deferable: 1, immediate: 2 });

    function cloneValue(value) {
        if (Array.isArray(value)) {
            return value.map(cloneValue);
        }
        if (!value || typeof value !== "object") {
            return value;
        }
        const cloned = {};
        Object.keys(value).forEach(key => {
            cloned[key] = cloneValue(value[key]);
        });
        return cloned;
    }

    function normalizeNamedItems(items) {
        if (!Array.isArray(items)) return [];
        return items
            .filter(item => item && typeof item === "object" && typeof item.name === "string" && item.name.trim())
            .map(item => cloneValue(item));
    }

    function normalizeCanonicalAction(action) {
        if (!action || typeof action !== "object" || Array.isArray(action) || typeof action.enabled !== "boolean") {
            return null;
        }
        const normalized = {
            enabled: action.enabled,
            reason: String(action.reason || ""),
            readiness: String(action.readiness || ""),
            blocking_status: String(action.blocking_status || "")
        };
        if (action.counts && typeof action.counts === "object" && !Array.isArray(action.counts)) {
            normalized.counts = cloneValue(action.counts);
        }
        return normalized;
    }

    function normalizedString(value, fallback, maxLength = 200) {
        return typeof value === "string" && value.length <= maxLength ? value : fallback;
    }

    function normalizedChoice(value, choices, fallback = "") {
        const normalized = String(value === undefined || value === null ? fallback : value);
        return choices.has(normalized) ? normalized : fallback;
    }

    function normalizedPageSize(value) {
        const parsed = Number.parseInt(value, 10);
        return allowedPageSizes.has(parsed) ? parsed : 25;
    }

    function normalizedRefreshPriority(value) {
        return value === "immediate" ? "immediate" : "deferable";
    }

    function pendingApprovalCounts(server) {
        const pending = Array.isArray(server && server.pending_updates) ? server.pending_updates : [];
        const plan = server && server.upgrade_plan && typeof server.upgrade_plan === "object" ? server.upgrade_plan : {};
        const standardPlanCount = Number(plan.standard_package_count);
        const keptBackPlanCount = Number(plan.kept_back_package_count);
        const standardSecurityPlanCount = Number(plan.standard_security_count);
        const totalSecurityPlanCount = Number(plan.total_security_count);
        const standard = Number.isFinite(standardPlanCount)
            ? Math.max(0, standardPlanCount)
            : pending.filter(update => !update.kept_back && !update.requires_full_upgrade).length;
        const keptBack = Number.isFinite(keptBackPlanCount)
            ? Math.max(0, keptBackPlanCount)
            : pending.filter(update => update.kept_back || update.requires_full_upgrade).length;
        const pendingSecurity = pending.filter(update => !!update.security);
        const pendingKeptSecurity = pendingSecurity.filter(update => update.kept_back || update.requires_full_upgrade).length;
        const standardSecurity = Number.isFinite(standardSecurityPlanCount)
            ? Math.max(0, standardSecurityPlanCount)
            : Math.max(0, pendingSecurity.length - pendingKeptSecurity);
        const totalSecurity = Number.isFinite(totalSecurityPlanCount)
            ? Math.max(0, totalSecurityPlanCount)
            : pendingSecurity.length;
        const keptBackSecurity = Math.max(0, totalSecurity - standardSecurity);
        return {
            standard,
            keptBack,
            standardSecurity,
            keptBackSecurity,
            full: Number(plan.full_upgrade_package_count || standard + keptBack),
            keptBackSecurityPlanAvailable: !!plan.kept_back_security_plan_available,
            fullPlanAvailable: !!plan.full_upgrade_plan_available,
            keptBackSecurityRemovedPackages: cloneValue(Array.isArray(plan.kept_back_security_removed_packages) ? plan.kept_back_security_removed_packages : []),
            keptBackSecurityNewPackages: cloneValue(Array.isArray(plan.kept_back_security_new_packages) ? plan.kept_back_security_new_packages : []),
            removedPackages: cloneValue(Array.isArray(plan.removed_packages) ? plan.removed_packages : []),
            newPackages: cloneValue(Array.isArray(plan.new_packages) ? plan.new_packages : [])
        };
    }

    function pluralized(count, singular) {
        return `${count} ${count === 1 ? singular : `${singular}s`}`;
    }

    function defaultActionReason(key, server, action) {
        if (action && action.reason) return action.reason;
        if (!server) return "Host is no longer loaded";
        const counts = pendingApprovalCounts(server);
        const enabled = !!(action && action.enabled);
        const status = String(server.status || "").toLowerCase();
        if (enabled) {
            if (key === "update") return "Ready to start update checks.";
            if (key === "approve_all") return `${pluralized(counts.standard, "standard update")} ready for approval.`;
            if (key === "approve_security") return `${pluralized(counts.standardSecurity, "standard security update")} ready for approval.`;
            if (key === "approve_security_kept_back") {
                const removalNote = counts.keptBackSecurityRemovedPackages.length > 0
                    ? " Package removals will be confirmed from the previewed plan."
                    : "";
                return `${pluralized(counts.keptBackSecurity, "kept-back security update")} ready for targeted approval.${removalNote}`;
            }
            if (key === "approve_full") return `${pluralized(counts.full, "full-upgrade package")} ready for approval.`;
            if (key === "cancel") return "Ready to cancel pending approval.";
            if (key === "autoremove") return "Ready to run apt autoremove.";
            if (key === "refresh_facts") return "Ready to refresh host facts.";
            return "Ready.";
        }
        if (["approve_all", "approve_security", "approve_security_kept_back", "approve_full", "cancel"].includes(key) && status !== "pending_approval") {
            return "Not waiting for approval";
        }
        if (key === "approve_all") return "No standard updates eligible";
        if (key === "approve_security") return "No standard security updates eligible";
        if (key === "approve_security_kept_back") return counts.keptBackSecurityPlanAvailable ? "No kept-back security updates eligible" : "Needs a fresh package scan";
        if (key === "approve_full") return counts.fullPlanAvailable ? "No full-upgrade packages eligible" : "Needs a fresh package scan";
        if (key === "cancel") return "Cannot cancel approval now";
        if (transientBlockingStatuses.has(status)) return `Current status ${status.replace(/_/g, " ")} blocks this action`;
        return "Action is unavailable";
    }

    function authLabel(server, globalKeyAvailable) {
        if (server && server.has_key && server.has_password) return "Server key + password";
        if (server && !server.has_key && globalKeyAvailable && server.has_password) return "Global SSH key + password";
        if (server && server.has_key) return "Server key";
        if (server && !server.has_key && globalKeyAvailable) return "Global SSH key";
        if (server && server.has_password) return "Password";
        return "No auth configured";
    }

    function legacyAction(server, dashboardServer, key) {
        const triage = dashboardServer && dashboardServer.approval_triage;
        const field = legacyActionFields[key];
        let enabled;
        if (["update", "autoremove", "refresh_facts", "enable_apt", "disable_apt"].includes(key)) {
            const status = String(server && server.status || "").trim().toLowerCase();
            enabled = !!server && !transientBlockingStatuses.has(status);
        } else if (field && triage && typeof triage === "object" && typeof triage[field] === "boolean") {
            enabled = triage[field];
        } else if (["approve_all", "approve_security", "approve_security_kept_back", "approve_full", "cancel"].includes(key)) {
            const counts = pendingApprovalCounts(server);
            const pending = String(server && server.status || "").toLowerCase() === "pending_approval";
            if (key === "approve_all") enabled = pending && counts.standard > 0;
            if (key === "approve_security") enabled = pending && counts.standardSecurity > 0;
            if (key === "approve_security_kept_back") enabled = pending && counts.keptBackSecurity > 0 && counts.keptBackSecurityPlanAvailable;
            if (key === "approve_full") enabled = pending && counts.full > 0 && counts.fullPlanAvailable;
            if (key === "cancel") enabled = pending;
        } else {
            return null;
        }

        const status = String(server && server.status || "").trim().toLowerCase();
        return {
            enabled,
            reason: "",
            readiness: "",
            blocking_status: enabled ? "" : status
        };
    }

    function createStore(options = {}) {
        const presentationFacts = options.presentationFacts && typeof options.presentationFacts === "object"
            ? options.presentationFacts
            : {};
        const canonicalApprovalCounts = typeof presentationFacts.approvalCounts === "function"
            ? presentationFacts.approvalCounts
            : pendingApprovalCounts;
        const canonicalAuthFacts = typeof presentationFacts.authFacts === "function"
            ? presentationFacts.authFacts
            : (server, globalKeyAvailable) => ({ label: authLabel(server, globalKeyAvailable) });
        let servers = [];
        let serversByName = new Map();
        let dashboardSnapshot = {};
        let dashboardServers = [];
        let dashboardByName = new Map();
        let globalKeyAvailable = false;
        let filters = {
            search: "",
            status: "",
            auth: "",
            groupBy: "",
            quick: "",
            tag: ""
        };
        let sort = { key: "name", dir: "asc" };
        let page = 1;
        let pageSize = 25;
        let primaryServerName = "";
        let selectedServerNames = new Set();
        let drawer = { open: false, serverName: "", tab: "logs", logFollow: true };
        let streams = {
            servers: { nextRequestId: 1, inFlight: null, queued: null, lastAcceptedRequestId: 0, lastError: "" },
            dashboard: { nextRequestId: 1, inFlight: null, queued: null, lastAcceptedRequestId: 0, lastError: "" }
        };
        let interaction = { depth: 0, releasePending: false, deferredRender: false, actionContext: null };
        let nextActionPlanId = 1;
        let inFlightActions = new Map();
        let bulkAction = null;

        function replaceServers(items) {
            servers = normalizeNamedItems(items);
            serversByName = new Map(servers.map(server => [server.name, server]));
        }

        function replaceDashboard(snapshot) {
            dashboardSnapshot = snapshot && typeof snapshot === "object" && !Array.isArray(snapshot) ? cloneValue(snapshot) : {};
            const items = normalizeNamedItems(snapshot && snapshot.servers);
            dashboardServers = items;
            dashboardByName = new Map(items.map(server => [server.name, server]));
        }

        function dashboardServerFor(server) {
            return dashboardByName.get(server && server.name) || null;
        }

        function isPendingApproval(server) {
            const dashboardServer = dashboardServerFor(server);
            return String(server && server.status || "").toLowerCase() === "pending_approval"
                || String(dashboardServer && dashboardServer.timeline && dashboardServer.timeline.current_phase || "").toLowerCase() === "pending_approval";
        }

        function hasPendingUpdates(server) {
            return isPendingApproval(server) && Array.isArray(server && server.pending_updates) && server.pending_updates.length > 0;
        }

        function matchesFilters(server) {
            const status = String(server.status || "").toLowerCase();
            const search = filters.search.trim().toLowerCase();
            const dashboardServer = dashboardServerFor(server);
            if (filters.status && status !== filters.status) return false;
            if (filters.tag) {
                const tags = Array.isArray(server.tags) && server.tags.length ? server.tags : ["untagged"];
                if (!tags.includes(filters.tag)) return false;
            }
            if (filters.quick === "pending_approval" && !isPendingApproval(server)) return false;
            if (filters.quick === "active") {
                const timelineState = String(dashboardServer && dashboardServer.timeline && dashboardServer.timeline.state || "").toLowerCase();
                if (!activeStatuses.has(status) && !["active", "queued"].includes(timelineState)) return false;
            }
            if (filters.quick === "stale_facts") {
                const factsState = String(dashboardServer && dashboardServer.approval_triage && dashboardServer.approval_triage.facts_state || "").toLowerCase();
                if (factsState !== "stale") return false;
            }
            if (filters.quick === "high_risk") {
                const dashboardCVECount = dashboardServer && dashboardServer.approval_triage && dashboardServer.approval_triage.cve_count;
                const fallbackCVECount = (Array.isArray(server.pending_updates) ? server.pending_updates : [])
                    .reduce((sum, update) => sum + (Array.isArray(update && update.cves) ? update.cves.length : 0), 0);
                const cveCount = dashboardCVECount === undefined || dashboardCVECount === null
                    ? fallbackCVECount
                    : Number(dashboardCVECount);
                if (cveCount <= 0) return false;
            }
            if (filters.auth === "password" && !server.has_password) return false;
            if (filters.auth === "key" && !server.has_key && !globalKeyAvailable) return false;
            if (!search) return true;
            const haystack = [server.name, server.host, server.port, server.user, ...(Array.isArray(server.tags) ? server.tags : [])]
                .filter(value => value !== undefined && value !== null)
                .join(" ")
                .toLowerCase();
            return haystack.includes(search);
        }

        function sortedVisibleServers() {
            const direction = sort.dir === "desc" ? -1 : 1;
            return servers.filter(matchesFilters).slice().sort((left, right) => {
                const leftValue = String(left[sort.key] || "").toLowerCase();
                const rightValue = String(right[sort.key] || "").toLowerCase();
                return leftValue.localeCompare(rightValue) * direction;
            });
        }

        function reconcileNavigation() {
            const previousPrimaryServerName = primaryServerName;
            const loadedNames = new Set(servers.map(server => server.name));
            selectedServerNames = new Set(Array.from(selectedServerNames).filter(name => loadedNames.has(name)));
            const visible = sortedVisibleServers();
            if (!visible.some(server => server.name === primaryServerName)) {
                primaryServerName = visible.length > 0 ? visible[0].name : "";
            }
            const totalPages = Math.max(1, Math.ceil(visible.length / pageSize));
            page = Math.max(1, Math.min(page, totalPages));
            if (drawer.open && !loadedNames.has(drawer.serverName)) {
                drawer = { open: false, serverName: "", tab: "logs", logFollow: true };
            } else if (drawer.open && drawer.tab === "pending" && !hasPendingUpdates(serversByName.get(drawer.serverName))) {
                drawer = { ...drawer, tab: "logs" };
            }
            return previousPrimaryServerName !== primaryServerName;
        }

        function persistenceValue() {
            return {
                search: filters.search,
                statusFilter: filters.status,
                authFilter: filters.auth,
                groupBy: filters.groupBy,
                pageSize: String(pageSize),
                fleetQuickFilter: filters.quick,
                fleetTagFilter: filters.tag,
                selectedServerName: primaryServerName
            };
        }

        function renderEffects(options = {}) {
            const priority = normalizedRefreshPriority(options.priority);
            if (options.scope === "serverState" && priority !== "immediate" && (interaction.depth > 0 || interaction.releasePending)) {
                interaction.deferredRender = true;
                return [];
            }
            return [{ type: "render", scope: options.scope || "navigation", priority }];
        }

        function stateEffects(options = {}) {
            const effects = [];
            if (options.persist) effects.push({ type: "persistFilters", value: persistenceValue() });
            if (options.render !== false) effects.push(...renderEffects(options));
            return effects;
        }

        function startRefresh(streamName, priority, reason) {
            const stream = streams[streamName];
            if (!stream) return [];
            const normalizedPriority = normalizedRefreshPriority(priority);
            if (stream.inFlight) {
                if (!stream.queued || refreshPriority[normalizedPriority] > refreshPriority[stream.queued.priority]) {
                    stream.queued = { priority: normalizedPriority, reason: String(reason || "refresh") };
                }
                return [];
            }
            const request = {
                requestId: stream.nextRequestId,
                priority: normalizedPriority,
                reason: String(reason || "refresh")
            };
            stream.nextRequestId += 1;
            stream.inFlight = request;
            stream.lastError = "";
            return [
                { type: "fetchSnapshot", stream: streamName, ...request },
                { type: "renderSyncState" }
            ];
        }

        function finishRefresh(streamName, requestId) {
            const stream = streams[streamName];
            if (!stream || !stream.inFlight || stream.inFlight.requestId !== requestId) return [];
            stream.lastAcceptedRequestId = Math.max(stream.lastAcceptedRequestId, requestId);
            stream.inFlight = null;
            if (!stream.queued) return [];
            const queued = stream.queued;
            stream.queued = null;
            return startRefresh(streamName, queued.priority, queued.reason);
        }

        function failRefresh(streamName, requestId, error) {
            const stream = streams[streamName];
            if (!stream || !stream.inFlight || stream.inFlight.requestId !== requestId) return [];
            stream.lastError = String(error || "Refresh failed");
            stream.inFlight = null;
            const effects = [{ type: "renderSyncState" }];
            if (stream.queued) {
                const queued = stream.queued;
                stream.queued = null;
                effects.push(...startRefresh(streamName, queued.priority, queued.reason));
            }
            return effects;
        }

        function restoreNavigation(value) {
            const saved = value && typeof value === "object" ? value : {};
            filters = {
                search: normalizedString(saved.search, ""),
                status: normalizedChoice(saved.statusFilter, allowedStatusFilters),
                auth: normalizedChoice(saved.authFilter, allowedAuthFilters),
                groupBy: normalizedChoice(saved.groupBy, allowedGroupings),
                quick: normalizedChoice(saved.fleetQuickFilter, allowedQuickFilters),
                tag: normalizedString(saved.fleetTagFilter, "", 100)
            };
            pageSize = normalizedPageSize(saved.pageSize);
            primaryServerName = normalizedString(saved.selectedServerName, "", 200);
            page = 1;
            reconcileNavigation();
        }

        function dispatch(event) {
            if (!event || typeof event !== "object") return [];
            if (event.type === "serversSnapshotReceived") {
                const requestId = Number(event.requestId || 0);
                const stream = streams.servers;
                if (requestId && (!stream.inFlight || stream.inFlight.requestId !== requestId || requestId < stream.lastAcceptedRequestId)) return [];
                const priority = requestId && stream.inFlight ? stream.inFlight.priority : normalizedRefreshPriority(event.priority);
                replaceServers(event.servers);
                const persistenceChanged = reconcileNavigation();
                return [
                    ...stateEffects({ persist: persistenceChanged, scope: "serverState", priority }),
                    { type: "renderSyncState" },
                    ...(requestId ? finishRefresh("servers", requestId) : [])
                ];
            } else if (event.type === "dashboardSnapshotReceived") {
                const requestId = Number(event.requestId || 0);
                const stream = streams.dashboard;
                if (requestId && (!stream.inFlight || stream.inFlight.requestId !== requestId || requestId < stream.lastAcceptedRequestId)) return [];
                const priority = requestId && stream.inFlight ? stream.inFlight.priority : normalizedRefreshPriority(event.priority);
                replaceDashboard(event.snapshot);
                const persistenceChanged = reconcileNavigation();
                return [
                    ...stateEffects({ persist: persistenceChanged, scope: "serverState", priority }),
                    { type: "renderSyncState" },
                    ...(requestId ? finishRefresh("dashboard", requestId) : [])
                ];
            } else if (event.type === "snapshotFailed") {
                return failRefresh(String(event.stream || ""), Number(event.requestId || 0), event.error);
            } else if (event.type === "refreshRequested") {
                const requestedStreams = Array.isArray(event.streams) ? event.streams : [event.stream];
                return requestedStreams.flatMap(stream => startRefresh(String(stream || ""), event.priority, event.reason));
            } else if (event.type === "interactionStarted") {
                if (interaction.depth === 0 && (!interaction.releasePending || !interaction.deferredRender)) {
                    const projection = projectView();
                    interaction.actionContext = {
                        serversByName: new Map(Array.from(serversByName.entries()).map(([name, server]) => [name, cloneValue(server)])),
                        dashboardByName: new Map(Array.from(dashboardByName.entries()).map(([name, server]) => [name, cloneValue(server)])),
                        selectedNames: cloneValue(projection.selectedNames),
                        visibleSelectedNames: cloneValue(projection.visibleSelectedNames),
                        hiddenSelectedNames: cloneValue(projection.hiddenSelectedNames)
                    };
                }
                interaction.depth += 1;
                const effects = [];
                if (interaction.releasePending) {
                    interaction.releasePending = false;
                    effects.push({ type: "cancelInteractionRelease" });
                }
                return effects;
            } else if (event.type === "interactionEnded") {
                interaction.depth = Math.max(0, interaction.depth - 1);
                if (interaction.depth > 0 || interaction.releasePending) return [];
                interaction.releasePending = true;
                return [{ type: "scheduleInteractionRelease", delayMs: Number(event.delayMs || 350) }];
            } else if (event.type === "interactionReleased") {
                interaction.releasePending = false;
                interaction.actionContext = null;
                if (!interaction.deferredRender || interaction.depth > 0) return [];
                interaction.deferredRender = false;
                return [{ type: "render", scope: "serverState", priority: "deferable" }];
            } else if (event.type === "interactionReset") {
                interaction.depth = 0;
                interaction.releasePending = false;
                interaction.actionContext = null;
                const effects = [{ type: "cancelInteractionRelease" }];
                if (interaction.deferredRender) {
                    interaction.deferredRender = false;
                    effects.push({ type: "render", scope: "serverState", priority: "deferable" });
                }
                return effects;
            } else if (event.type === "actionStarted") {
                const plan = event.plan && typeof event.plan === "object" ? event.plan : {};
                const names = Array.isArray(plan.serverNames) ? plan.serverNames.filter(name => !!actionServer(name)) : [];
                if (!plan.id || !plan.actionKey || names.length === 0 || (plan.kind === "bulk" && bulkAction)) {
                    return [{ type: "actionRejected", operationId: String(plan.id || ""), reason: "Action plan is no longer available" }];
                }
                const unavailable = names.find(name => {
                    const action = getAction(name, plan.actionKey);
                    return !action || !action.enabled;
                });
                if (unavailable) {
                    return [{
                        type: "actionRejected",
                        operationId: plan.id,
                        reason: defaultActionReason(plan.actionKey, actionServer(unavailable), getAction(unavailable, plan.actionKey))
                    }];
                }
                names.forEach(name => inFlightActions.set(name, {
                    operationId: plan.id,
                    actionKey: plan.actionKey,
                    actionLabel: plan.actionLabel,
                    kind: plan.kind
                }));
                if (plan.kind === "bulk") {
                    bulkAction = { operationId: plan.id, actionKey: plan.actionKey, actionLabel: plan.actionLabel, serverNames: cloneValue(names) };
                }
                return [{ type: "render", scope: "serverState", priority: "immediate" }];
            } else if (event.type === "actionCompleted" || event.type === "actionFailed") {
                const operationId = String(event.operationId || "");
                Array.from(inFlightActions.entries()).forEach(([name, action]) => {
                    if (action.operationId === operationId) inFlightActions.delete(name);
                });
                if (bulkAction && bulkAction.operationId === operationId) bulkAction = null;
                return actionLifecycleEffects(event, event.type === "actionCompleted" ? "completed" : "failed");
            } else if (event.type === "navigationRestored") {
                restoreNavigation(event.value);
                return stateEffects({ render: false });
            } else if (event.type === "filtersChanged") {
                const patch = event.patch && typeof event.patch === "object" ? event.patch : {};
                filters = {
                    search: Object.hasOwn(patch, "search") ? normalizedString(patch.search, "") : filters.search,
                    status: Object.hasOwn(patch, "status") ? normalizedChoice(patch.status, allowedStatusFilters) : filters.status,
                    auth: Object.hasOwn(patch, "auth") ? normalizedChoice(patch.auth, allowedAuthFilters) : filters.auth,
                    groupBy: Object.hasOwn(patch, "groupBy") ? normalizedChoice(patch.groupBy, allowedGroupings) : filters.groupBy,
                    quick: Object.hasOwn(patch, "quick") ? normalizedChoice(patch.quick, allowedQuickFilters) : filters.quick,
                    tag: Object.hasOwn(patch, "tag") ? normalizedString(patch.tag, "", 100) : filters.tag
                };
                if (Object.hasOwn(patch, "pageSize")) pageSize = normalizedPageSize(patch.pageSize);
                page = 1;
                reconcileNavigation();
                return stateEffects({ persist: true });
            } else if (event.type === "sortChanged") {
                const key = normalizedString(event.key, "name", 50);
                if (sort.key === key) {
                    sort = { key, dir: sort.dir === "asc" ? "desc" : "asc" };
                } else {
                    sort = { key, dir: "asc" };
                }
                reconcileNavigation();
                return stateEffects();
            } else if (event.type === "pageChanged") {
                page = Number.isFinite(Number(event.page)) ? Number(event.page) : page + Number(event.delta || 0);
                reconcileNavigation();
                return stateEffects();
            } else if (event.type === "selectionChanged") {
                const name = String(event.name || "");
                if (serversByName.has(name)) {
                    if (event.selected) selectedServerNames.add(name);
                    else selectedServerNames.delete(name);
                }
                return stateEffects();
            } else if (event.type === "pageSelectionChanged") {
                const projection = projectView();
                projection.pageServers.forEach(server => {
                    if (event.selected) selectedServerNames.add(server.name);
                    else selectedServerNames.delete(server.name);
                });
                return stateEffects();
            } else if (event.type === "primaryServerSelected") {
                primaryServerName = serversByName.has(String(event.name || "")) ? String(event.name) : "";
                reconcileNavigation();
                return stateEffects({ persist: true });
            } else if (event.type === "drawerOpened") {
                const name = String(event.name || "");
                if (serversByName.has(name)) {
                    drawer = {
                        open: true,
                        serverName: name,
                        tab: event.tab === "pending" && hasPendingUpdates(serversByName.get(name)) ? "pending" : "logs",
                        logFollow: drawer.serverName === name ? drawer.logFollow : true
                    };
                }
                return stateEffects();
            } else if (event.type === "drawerClosed") {
                drawer = { ...drawer, open: false };
                return stateEffects();
            } else if (event.type === "drawerTabChanged") {
                const tab = event.tab === "pending" && hasPendingUpdates(serversByName.get(drawer.serverName)) ? "pending" : "logs";
                drawer = { ...drawer, tab };
                return stateEffects();
            } else if (event.type === "drawerLogFollowChanged") {
                drawer = { ...drawer, logFollow: !!event.value };
                return stateEffects({ render: false });
            } else if (event.type === "globalKeyAvailabilityChanged") {
                globalKeyAvailable = !!event.available;
                reconcileNavigation();
                return stateEffects();
            }
            return [];
        }

        function getServer(name) {
            const server = serversByName.get(String(name || ""));
            return server ? cloneValue(server) : null;
        }

        function getDashboardServer(name) {
            const server = dashboardByName.get(String(name || ""));
            return server ? cloneValue(server) : null;
        }

        function actionServer(name) {
            const normalizedName = String(name || "");
            if (interaction.actionContext && interaction.actionContext.serversByName.has(normalizedName)) {
                return interaction.actionContext.serversByName.get(normalizedName);
            }
            return serversByName.get(normalizedName) || null;
        }

        function actionDashboardServer(name) {
            const normalizedName = String(name || "");
            if (interaction.actionContext && interaction.actionContext.dashboardByName.has(normalizedName)) {
                return interaction.actionContext.dashboardByName.get(normalizedName);
            }
            return dashboardByName.get(normalizedName) || null;
        }

        function getAction(name, key, options = {}) {
            const normalizedName = String(name || "");
            const server = actionServer(normalizedName);
            const dashboardServer = actionDashboardServer(normalizedName);
            const canonical = normalizeCanonicalAction(dashboardServer && dashboardServer.actions && dashboardServer.actions[key]);
            const action = canonical || legacyAction(server, dashboardServer, key);
            if (!action) return null;
            if (!options.ignoreInFlight && inFlightActions.has(normalizedName)) {
                return {
                    ...cloneValue(action),
                    enabled: false,
                    reason: "Another action is already running for this host",
                    readiness: "in_progress"
                };
            }
            return cloneValue(action);
        }

        function nextPlanId(kind) {
            const id = `${kind}-${nextActionPlanId}`;
            nextActionPlanId += 1;
            return id;
        }

        function planAction(name, actionKey, options = {}) {
            const normalizedName = String(name || "");
            const server = actionServer(normalizedName);
            const action = getAction(normalizedName, actionKey) || {
                enabled: false,
                reason: "Action is unavailable",
                readiness: "blocked",
                blocking_status: String(server && server.status || "")
            };
            return {
                id: nextPlanId("action"),
                kind: "single",
                actionKey: String(actionKey || ""),
                actionLabel: String(options.actionLabel || actionKey || "action"),
                serverName: normalizedName,
                serverNames: normalizedName ? [normalizedName] : [],
                enabled: !!action.enabled,
                reason: defaultActionReason(actionKey, server, action),
                readiness: String(action.readiness || (action.enabled ? "ready" : "blocked")),
                blockingStatus: String(action.blocking_status || ""),
                payloadFacts: { counts: canonicalApprovalCounts(server) }
            };
        }

        function planBulkAction(actionKey, options = {}) {
            const projection = projectView();
            const selectedNames = interaction.actionContext ? interaction.actionContext.selectedNames : projection.selectedNames;
            const visibleSelectedNames = interaction.actionContext ? interaction.actionContext.visibleSelectedNames : projection.visibleSelectedNames;
            const hiddenSelectedNames = interaction.actionContext ? interaction.actionContext.hiddenSelectedNames : projection.hiddenSelectedNames;
            const eligibleNames = [];
            const eligibleHosts = [];
            const ineligible = [];
            visibleSelectedNames.forEach(name => {
                const server = actionServer(name);
                const action = getAction(name, actionKey) || { enabled: false, reason: "Action is unavailable", readiness: "blocked" };
                const reason = defaultActionReason(actionKey, server, action);
                if (action.enabled) {
                    eligibleNames.push(name);
                    eligibleHosts.push({ name, auth: canonicalAuthFacts(server, globalKeyAvailable).label, readiness: reason });
                } else {
                    ineligible.push({ name, auth: canonicalAuthFacts(server, globalKeyAvailable).label, reason });
                }
            });
            const hiddenHosts = hiddenSelectedNames.map(name => ({
                name,
                auth: canonicalAuthFacts(actionServer(name), globalKeyAvailable).label,
                reason: "Hidden by current filter or page"
            }));
            return {
                id: options.preview ? "" : nextPlanId("bulk"),
                kind: "bulk",
                actionKey: String(actionKey || ""),
                actionLabel: String(options.actionLabel || actionKey || "action"),
                selectedNames: cloneValue(selectedNames),
                visibleNames: cloneValue(visibleSelectedNames),
                hiddenNames: cloneValue(hiddenSelectedNames),
                eligibleNames,
                serverNames: cloneValue(eligibleNames),
                eligibleHosts,
                ineligible,
                skippedHosts: [...hiddenHosts, ...ineligible],
                payloadFacts: Object.fromEntries(eligibleNames.map(name => [name, { counts: canonicalApprovalCounts(actionServer(name)) }]))
            };
        }

        function actionLifecycleEffects(event, status) {
            const effects = [{ type: "render", scope: "serverState", priority: "immediate" }];
            const refreshStreams = Array.isArray(event.refreshStreams) ? event.refreshStreams : ["servers"];
            refreshStreams.forEach(stream => effects.push(...startRefresh(stream, "immediate", `action-${status}`)));
            if (event.message) effects.push({ type: "announceResult", status, message: String(event.message) });
            return effects;
        }

        function projectView() {
            const visibleServers = sortedVisibleServers();
            const totalPages = Math.max(1, Math.ceil(visibleServers.length / pageSize));
            const safePage = Math.max(1, Math.min(page, totalPages));
            const start = (safePage - 1) * pageSize;
            const pageServers = visibleServers.slice(start, start + pageSize);
            const pageNames = new Set(pageServers.map(server => server.name));
            const selectedNames = Array.from(selectedServerNames);
            const visibleSelectedNames = selectedNames.filter(name => pageNames.has(name));
            const hiddenSelectedNames = selectedNames.filter(name => !pageNames.has(name));
            const grouped = new Map();
            if (filters.groupBy === "status") {
                pageServers.forEach(server => {
                    const key = server.status || "unknown";
                    if (!grouped.has(key)) grouped.set(key, []);
                    grouped.get(key).push(server);
                });
            } else if (filters.groupBy === "tag") {
                pageServers.forEach(server => {
                    const tags = Array.isArray(server.tags) && server.tags.length ? server.tags : ["untagged"];
                    tags.forEach(tag => {
                        if (!grouped.has(tag)) grouped.set(tag, []);
                        grouped.get(tag).push(server);
                    });
                });
            }
            const groups = filters.groupBy
                ? Array.from(grouped.entries()).map(([key, items]) => ({ key, items: cloneValue(items) }))
                : [{ key: "", items: cloneValue(pageServers) }];
            return {
                servers: cloneValue(servers),
                dashboardSnapshot: cloneValue(dashboardSnapshot),
                dashboardServers: cloneValue(dashboardServers),
                actionViews: Object.fromEntries(servers.map(server => [
                    server.name,
                    Object.fromEntries(dashboardActionKeys.map(key => [key, getAction(server.name, key)]))
                ])),
                filters: cloneValue(filters),
                sort: cloneValue(sort),
                page: safePage,
                pageSize,
                totalPages,
                visibleServers: cloneValue(visibleServers),
                pageServers: cloneValue(pageServers),
                groups,
                primaryServerName,
                selectedNames,
                visibleSelectedNames,
                hiddenSelectedNames,
                visibleSelectedServers: cloneValue(pageServers.filter(server => selectedServerNames.has(server.name))),
                drawer: cloneValue(drawer),
                sync: {
                    streams: cloneValue(streams),
                    interactionDepth: interaction.depth,
                    interactionReleasePending: interaction.releasePending,
                    interactionActive: interaction.depth > 0 || interaction.releasePending,
                    deferredRender: interaction.deferredRender
                },
                actions: {
                    inFlightServerNames: Array.from(inFlightActions.keys()),
                    inFlight: Array.from(inFlightActions.entries()).map(([serverName, action]) => ({ serverName, ...cloneValue(action) })),
                    bulk: cloneValue(bulkAction)
                },
                persistence: persistenceValue()
            };
        }

        function getView() {
            reconcileNavigation();
            return projectView();
        }

        function getPersistence() {
            return {
                ...persistenceValue()
            };
        }

        return Object.freeze({
            dispatch,
            getAction,
            getDashboardServer,
            getPersistence,
            getServer,
            getView,
            planAction,
            planBulkAction
        });
    }

    return Object.freeze({
        createStore,
        normalizeCanonicalAction
    });
}));
