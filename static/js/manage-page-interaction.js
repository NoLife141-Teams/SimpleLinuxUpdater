(function initManagePageInteraction(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.ManagePageInteraction = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function managePageInteractionFactory() {
    "use strict";

    const pageSizes = new Set([10, 20, 25, 50, 100]);
    const streamNames = ["inventory", "globalKey", "audit", "policyContext", "hostKey"];

    function clone(value) {
        if (Array.isArray(value)) return value.map(clone);
        if (!value || typeof value !== "object") return value;
        return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, clone(item)]));
    }

    function normalizePort(value, fallback = 22) {
        const parsed = Number.parseInt(value, 10);
        return Number.isFinite(parsed) && parsed > 0 && parsed <= 65535 ? parsed : fallback;
    }

    function normalizeTags(value) {
        const seen = new Set();
        const items = Array.isArray(value) ? value : String(value || "").split(",");
        return items.map(item => String(item || "").trim()).filter(Boolean).filter(item => {
            const key = item.toLowerCase();
            if (seen.has(key)) return false;
            seen.add(key);
            return true;
        });
    }

    function normalizeServer(server) {
        if (!server || typeof server !== "object" || !String(server.name || "").trim()) return null;
        return { ...clone(server), name: String(server.name).trim(), tags: normalizeTags(server.tags), port: normalizePort(server.port, 22) };
    }

    function emptyStream() {
        return { nextRequestID: 1, inFlight: null, queued: null, lastError: "", data: null };
    }

    function defaultFilters() {
        return { search: "", tag: "", auth: "", group: "", pageSize: 20 };
    }

    function createStore() {
        let inventory = [];
        let globalKeyAvailable = false;
        let filters = defaultFilters();
        let sort = { key: "name", direction: "asc" };
        let page = 1;
        let editor = { sessionID: 0, open: false, originalName: "", draft: null, hostKey: null, policyContext: { policies: [], overrides: {} } };
        let audit = { query: { targetName: "", action: "", status: "", from: "", to: "", page: 1, pageSize: 20 }, items: [], total: 0, selectedID: "" };
        const inFlightCommands = new Set();
        const streams = Object.fromEntries(streamNames.map(name => [name, emptyStream()]));
        // Transitional adapter storage keeps browser-only mechanics out of the module projection.
        const adapterState = { editingServerName: null, auditEvents: [], auditPage: 1, auditPageSize: 20, auditTotal: 0, hostKeyModalPromise: null, hostKeyModalResolvers: [], editSaveInProgress: false, editKnownHostState: { host: "", port: 0, checked: false, alreadyTrusted: false, fingerprint: "" }, editKnownHostCheckPromise: null, editUpdatePolicies: [], editPolicyOverrideStates: new Map(), auditFetchHadError: false };

        function effect(type, props) { return { type, ...props }; }
        function request(stream, payload = {}) {
            const state = streams[stream];
            if (state.inFlight !== null) { state.queued = clone(payload); return []; }
            const requestID = state.nextRequestID++;
            state.inFlight = requestID;
            state.lastError = "";
            return [effect("fetchSnapshot", { stream, requestID, ...clone(payload) })];
        }
        function received(stream, requestID, data) {
            const state = streams[stream];
            if (!state || (requestID && state.inFlight !== requestID)) return [];
            state.inFlight = null;
            state.data = clone(data);
            const effects = [effect("render", { area: stream })];
            if (state.queued) { const queued = state.queued; state.queued = null; effects.push(...request(stream, queued)); }
            return effects;
        }
        function failed(stream, requestID, error) {
            const state = streams[stream];
            if (!state || (requestID && state.inFlight !== requestID)) return [];
            state.inFlight = null;
            state.lastError = String(error || "Failed to refresh.");
            const effects = [effect("render", { area: stream }), effect("announce", { scope: stream, message: state.lastError, error: true })];
            if (state.queued) { const queued = state.queued; state.queued = null; effects.push(...request(stream, queued)); }
            return effects;
        }
        function projectedInventory() {
            const search = filters.search.toLowerCase();
            const tag = filters.tag.toLowerCase();
            const filtered = inventory.filter(server => {
                const effectiveKey = !!server.has_key || globalKeyAvailable;
                if (filters.auth === "password" && !server.has_password) return false;
                if (filters.auth === "key" && !effectiveKey) return false;
                if (tag && !server.tags.join(" ").toLowerCase().includes(tag)) return false;
                return !search || [server.name, server.host, server.user, server.tags.join(" ")].join(" ").toLowerCase().includes(search);
            }).sort((left, right) => {
                const value = server => (sort.key === "tags" ? server.tags.join(",") : server[sort.key] || "").toString().toLowerCase();
                return value(left).localeCompare(value(right)) * (sort.direction === "asc" ? 1 : -1);
            });
            const totalPages = Math.max(1, Math.ceil(filtered.length / filters.pageSize));
            const safePage = Math.min(Math.max(1, page), totalPages);
            const items = filtered.slice((safePage - 1) * filters.pageSize, safePage * filters.pageSize);
            const groups = new Map();
            if (!filters.group) groups.set("", items);
            items.forEach(server => {
                if (!filters.group) return;
                const keys = filters.group === "tag" ? (server.tags.length ? server.tags : ["untagged"]) : [((server.has_key || globalKeyAvailable) ? (server.has_key ? "key" : "global key") : "no key") + " / " + (server.has_password ? "password" : "no password")];
                keys.forEach(key => { if (!groups.has(key)) groups.set(key, []); groups.get(key).push(server); });
            });
            return { allItems: clone(inventory), items: clone(items), groups: Array.from(groups.entries()).map(([key, value]) => ({ key, items: clone(value) })), total: filtered.length, page: safePage, totalPages };
        }
        function commandPlan(command, payload = {}) {
            const key = command === "auditPrune" || command.startsWith("globalKey") ? command : `${command}:${payload.serverName || editor.originalName || "new"}`;
            if (inFlightCommands.has(key)) return { enabled: false, reason: "This Manage action is already in progress." };
            if (command === "saveEditor") {
                const draft = editor.draft || {};
                const errors = [!String(draft.name || "").trim() && "name", !String(draft.host || "").trim() && "host", !String(draft.user || "").trim() && "user"].filter(Boolean);
                if (errors.length) return { enabled: false, reason: `${errors.join(", ")} required.`, invalidFields: errors };
                return { enabled: true, key, command, payload: { ...clone(draft), tags: normalizeTags(draft.tags), port: normalizePort(draft.port, 22), originalName: editor.originalName, sessionID: editor.sessionID } };
            }
            if (command === "trustHostKey") {
                const hostKey = editor.hostKey;
                if (!hostKey || !hostKey.fingerprint) return { enabled: false, reason: "Scan the current host key before trusting it." };
                return { enabled: true, key, command, payload: { ...clone(hostKey), sessionID: editor.sessionID } };
            }
            return { enabled: true, key, command, payload: clone(payload) };
        }
        function dispatch(event) {
            const input = event || {};
            switch (input.type) {
                case "inventorySnapshotReceived":
                    if (input.requestID && streams.inventory.inFlight !== input.requestID) return [];
                    inventory = (Array.isArray(input.items) ? input.items : []).map(normalizeServer).filter(Boolean);
                    return received("inventory", input.requestID, { items: inventory });
                case "globalKeySnapshotReceived": if (input.requestID && streams.globalKey.inFlight !== input.requestID) return []; globalKeyAvailable = !!input.hasKey; return received("globalKey", input.requestID, { hasKey: globalKeyAvailable });
                case "filtersChanged": filters = { ...filters, ...(input.patch || {}) }; filters.pageSize = pageSizes.has(Number(filters.pageSize)) ? Number(filters.pageSize) : 20; page = 1; return [effect("render", { area: "inventory" })];
                case "sortChanged": sort = sort.key === input.key ? { key: input.key, direction: sort.direction === "asc" ? "desc" : "asc" } : { key: input.key || "name", direction: "asc" }; return [effect("render", { area: "inventory" })];
                case "pageChanged": page = Math.max(1, Number(input.page) || 1); return [effect("render", { area: "inventory" })];
                case "editorOpened": { const server = inventory.find(item => item.name === input.name) || input.server || {}; editor = { sessionID: editor.sessionID + 1, open: true, originalName: String(server.name || input.name || ""), draft: { ...clone(server), tags: normalizeTags(server.tags) }, hostKey: null, policyContext: { policies: [], overrides: {} } }; return [effect("render", { area: "editor" })]; }
                case "editorChanged": if (editor.open) { editor.draft = { ...editor.draft, ...(input.patch || {}) }; editor.hostKey = null; } return [effect("render", { area: "editor" })];
                case "editorClosed": editor = { ...editor, sessionID: editor.sessionID + 1, open: false, hostKey: null, policyContext: { policies: [], overrides: {} } }; return [effect("render", { area: "editor" })];
                case "hostKeyReceived": if (editor.open && input.sessionID === editor.sessionID && input.host === String(editor.draft.host || "").trim() && normalizePort(input.port) === normalizePort(editor.draft.port)) editor.hostKey = clone(input.hostKey); return received("hostKey", input.requestID, input.hostKey);
                case "policyContextReceived": if (editor.open && input.sessionID === editor.sessionID) editor.policyContext = clone(input.context || editor.policyContext); return received("policyContext", input.requestID, input.context);
                case "auditQueryChanged": audit.query = { ...audit.query, ...(input.patch || {}) }; audit.query.page = Math.max(1, Number(audit.query.page) || 1); return [effect("render", { area: "audit" })];
                case "auditSnapshotReceived": { if (input.requestID && streams.audit.inFlight !== input.requestID) return []; const data = input.data || {}; audit.items = clone(Array.isArray(data.items) ? data.items : []); audit.total = Math.max(0, Number(data.total) || 0); const pages = Math.max(1, Math.ceil(audit.total / audit.query.pageSize)); if (audit.query.page > pages) { audit.query.page = pages; return [...received("audit", input.requestID, data), ...request("audit", { query: clone(audit.query) })]; } return received("audit", input.requestID, data); }
                case "auditDetailSelected": audit.selectedID = String(input.id || ""); return [effect("render", { area: "auditDetail" })];
                case "snapshotRequested": return request(input.stream, input.payload);
                case "snapshotFailed": return failed(input.stream, input.requestID, input.error);
                case "commandRequested": { const plan = commandPlan(input.command, input.payload); if (!plan.enabled) return [effect("commandRejected", plan)]; inFlightCommands.add(plan.key); return [effect("executeCommand", { plan })]; }
                case "commandCompleted": case "commandFailed": { const plan = input.plan || {}; inFlightCommands.delete(plan.key); const error = input.type === "commandFailed"; return [effect("announce", { message: input.message || (error ? "Manage action failed." : "Manage action completed."), error }), ...(error ? [] : ["inventory", "globalKey", "audit"].flatMap(stream => request(stream)))]; }
                default: return [];
            }
        }
        function getView() { return clone({ inventory: projectedInventory(), globalKeyAvailable, filters, sort, editor, audit, streams, commands: { inFlight: Array.from(inFlightCommands) } }); }
        return Object.freeze({ dispatch, getView, planCommand: (command, payload) => clone(commandPlan(command, payload)), adapterState });
    }
    return Object.freeze({ createStore, normalizePort, normalizeTags });
}));
