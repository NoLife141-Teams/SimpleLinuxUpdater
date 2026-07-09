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

    function legacyAction(server, dashboardServer, key) {
        const triage = dashboardServer && dashboardServer.approval_triage;
        const field = legacyActionFields[key];
        let enabled;
        if (["update", "autoremove", "refresh_facts", "enable_apt", "disable_apt"].includes(key)) {
            const status = String(server && server.status || "").trim().toLowerCase();
            enabled = !!server && !transientBlockingStatuses.has(status);
        } else if (field && triage && typeof triage === "object" && typeof triage[field] === "boolean") {
            enabled = triage[field];
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

    function createStore() {
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
        let interaction = { depth: 0, releasePending: false, deferredRender: false };

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
                const cveCount = Number(dashboardServer && dashboardServer.approval_triage && dashboardServer.approval_triage.cve_count || 0);
                if (cveCount <= 0) return false;
            }
            if (filters.auth === "password" && !server.has_password) return false;
            if (filters.auth === "key" && !server.has_key && !globalKeyAvailable) return false;
            if (!filters.search) return true;
            const haystack = [server.name, server.host, server.port, server.user, ...(Array.isArray(server.tags) ? server.tags : [])]
                .filter(value => value !== undefined && value !== null)
                .join(" ")
                .toLowerCase();
            return haystack.includes(filters.search.toLowerCase());
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
                if (!interaction.deferredRender || interaction.depth > 0) return [];
                interaction.deferredRender = false;
                return [{ type: "render", scope: "serverState", priority: "deferable" }];
            } else if (event.type === "interactionReset") {
                interaction.depth = 0;
                interaction.releasePending = false;
                const effects = [{ type: "cancelInteractionRelease" }];
                if (interaction.deferredRender) {
                    interaction.deferredRender = false;
                    effects.push({ type: "render", scope: "serverState", priority: "deferable" });
                }
                return effects;
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

        function getAction(name, key) {
            const normalizedName = String(name || "");
            const server = serversByName.get(normalizedName) || null;
            const dashboardServer = dashboardByName.get(normalizedName) || null;
            const canonical = normalizeCanonicalAction(dashboardServer && dashboardServer.actions && dashboardServer.actions[key]);
            return cloneValue(canonical || legacyAction(server, dashboardServer, key));
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
            getView
        });
    }

    return Object.freeze({
        createStore,
        normalizeCanonicalAction
    });
}));
