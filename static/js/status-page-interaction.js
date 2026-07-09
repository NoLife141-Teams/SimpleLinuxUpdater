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
        let dashboardServers = [];
        let dashboardByName = new Map();

        function replaceServers(items) {
            servers = normalizeNamedItems(items);
            serversByName = new Map(servers.map(server => [server.name, server]));
        }

        function replaceDashboard(snapshot) {
            const items = normalizeNamedItems(snapshot && snapshot.servers);
            dashboardServers = items;
            dashboardByName = new Map(items.map(server => [server.name, server]));
        }

        function dispatch(event) {
            if (!event || typeof event !== "object") return [];
            if (event.type === "serversSnapshotReceived") {
                replaceServers(event.servers);
            } else if (event.type === "dashboardSnapshotReceived") {
                replaceDashboard(event.snapshot);
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

        function getView() {
            return {
                servers: cloneValue(servers),
                dashboardServers: cloneValue(dashboardServers)
            };
        }

        return Object.freeze({
            dispatch,
            getAction,
            getDashboardServer,
            getServer,
            getView
        });
    }

    return Object.freeze({
        createStore,
        normalizeCanonicalAction
    });
}));
