(function initAdminPageInteraction(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.AdminPageInteraction = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function adminPageInteractionFactory() {
    "use strict";

    const streamNames = ["timezone", "notifications", "account", "metrics", "backup"];

    function clone(value) {
        if (Array.isArray(value)) return value.map(clone);
        if (!value || typeof value !== "object") return value;
        return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, clone(item)]));
    }

    function uniqueStrings(values) {
        const seen = new Set();
        return (Array.isArray(values) ? values : []).map(value => String(value || "").trim()).filter(value => value && !seen.has(value) && seen.add(value));
    }

    function emptyStream() {
        return { nextRequestID: 1, activeRequestID: null, freshness: "unavailable", error: "", accepted: false };
    }

    function normalizeTimezone(data = {}) {
        const configured = String(data.editable_timezone ?? data.editableTimezone ?? data.timezone ?? "").trim();
        const resolved = String(data.resolved_timezone ?? data.resolvedTimezone ?? data.timezone ?? "").trim();
        return { configured, resolved: resolved || configured || "UTC" };
    }

    function normalizeNotifications(data = {}) {
        return {
            enabled: Boolean(data.enabled),
            webhookURL: String(data.webhook_url ?? data.webhookURL ?? "").trim(),
            eventTypes: uniqueStrings(data.event_types ?? data.eventTypes),
            supportedEvents: uniqueStrings(data.supported_events ?? data.supportedEvents),
            lastDelivery: clone(data.last_delivery ?? data.lastDelivery ?? null)
        };
    }

    function normalizeBackup(data = {}) {
        return {
            blocked: Boolean(data.blocked),
            reason: String(data.reason || data.maintenance_reason || "").trim(),
            status: clone(data)
        };
    }

    function createStore(options = {}) {
        const scheduled = options.scheduled || null;
        const streams = Object.fromEntries(streamNames.map(name => [name, emptyStream()]));
        const inFlight = new Map();
        let timezone = { configured: "", resolved: "UTC", draft: "" };
        let notifications = { enabled: false, webhookURL: "", eventTypes: [], supportedEvents: [], lastDelivery: null };
        let account = { sessionCount: 0 };
        let metrics = { enabled: false, revealedToken: "" };
        let backup = { blocked: false, reason: "", status: null, selectedFile: null };
        const feedback = Object.fromEntries(streamNames.map(name => [name, { message: "", error: false }]));

        function effect(type, details = {}) { return { type, ...details }; }
        function feedbackScope(command) {
            if (command === "saveTimezone") return "timezone";
            if (command === "saveNotifications" || command === "testNotification") return "notifications";
            if (command === "changePassword" || command === "clearSessions") return "account";
            if (command.includes("MetricsToken")) return "metrics";
            if (command.includes("Backup")) return "backup";
            return "account";
        }
        function commandKey(command) { return command; }

        function request(stream) {
            const state = streams[stream];
            if (!state) return [];
            const requestID = state.nextRequestID++;
            state.activeRequestID = requestID;
            state.freshness = "refreshing";
            state.error = "";
            return [effect("fetchSnapshot", { stream, requestID })];
        }

        function accept(stream, requestID) {
            const state = streams[stream];
            if (!state) return false;
            if (state.activeRequestID !== null && state.activeRequestID !== requestID) return false;
            if (state.activeRequestID === null && requestID) return false;
            state.activeRequestID = null;
            state.accepted = true;
            state.freshness = "fresh";
            state.error = "";
            return true;
        }

        function fail(stream, requestID, error) {
            const state = streams[stream];
            if (!state || (requestID && state.activeRequestID !== requestID)) return [];
            state.activeRequestID = null;
            state.error = String(error || "Failed to refresh.");
            state.freshness = state.accepted ? "stale" : "unavailable";
            feedback[stream] = { message: state.error, error: true };
            return [effect("render", { area: stream })];
        }

        function applyTimezone(data) {
            const normalized = normalizeTimezone(data);
            timezone = { ...timezone, ...normalized, draft: normalized.configured };
        }
        function applyNotifications(data) { notifications = normalizeNotifications(data); }
        function applyAccount(data = {}) { account = { sessionCount: Math.max(0, Number(data.count ?? data.session_count ?? 0) || 0) }; }
        function applyMetrics(data = {}) {
            metrics = { enabled: Boolean(data.enabled ?? data.configured ?? data.has_token), revealedToken: String(data.token ?? metrics.revealedToken ?? "") };
        }
        function applyBackup(data) { backup = { ...backup, ...normalizeBackup(data) }; }

        function planCommand(command, payload = {}) {
            if (String(command).startsWith("scheduled:") && scheduled && typeof scheduled.planCommand === "function") {
                return clone(scheduled.planCommand(String(command).slice(10), payload));
            }
            const key = commandKey(command);
            if (inFlight.has(key)) return { enabled: false, command, key, reason: "This Admin action is already in progress." };
            switch (command) {
                case "saveTimezone": {
                    const value = String(payload.timezone ?? timezone.draft ?? "").trim();
                    return { enabled: true, command, key, payload: { timezone: value } };
                }
                case "saveNotifications":
                    return { enabled: true, command, key, payload: { enabled: notifications.enabled, webhook_url: notifications.webhookURL, event_types: clone(notifications.eventTypes) } };
                case "testNotification": return { enabled: true, command, key, payload: {} };
                case "changePassword": {
                    if (!payload.hasCurrentPassword || !payload.hasNewPassword || !payload.passwordsMatch) return { enabled: false, command, key, reason: "Current password and matching new passwords are required." };
                    return { enabled: true, command, key, payload: {} };
                }
                case "clearSessions": return { enabled: true, command, key, payload: {} };
                case "rotateMetricsToken": case "disableMetricsToken": return { enabled: true, command, key, payload: {} };
                case "copyMetricsToken": return metrics.revealedToken ? { enabled: true, command, key, payload: { token: metrics.revealedToken } } : { enabled: false, command, key, reason: "No revealed metrics token is available." };
                case "exportBackup":
                    if (backup.blocked) return { enabled: false, command, key, reason: backup.reason || "Backup is unavailable." };
                    if (!payload.passphraseValid || !payload.passwordsMatch) return { enabled: false, command, key, reason: "A valid matching backup passphrase is required." };
                    return { enabled: true, command, key, payload: { includeKnownHosts: Boolean(payload.includeKnownHosts) } };
                case "verifyBackup": case "restoreBackup":
                    if (backup.blocked) return { enabled: false, command, key, reason: backup.reason || "Backup is unavailable." };
                    if (!backup.selectedFile || !payload.passphraseValid) return { enabled: false, command, key, reason: !backup.selectedFile ? "Choose a backup file." : "A valid backup passphrase is required." };
                    return { enabled: true, command, key, payload: { file: clone(backup.selectedFile) } };
                default: return { enabled: true, command, key, payload: clone(payload) };
            }
        }

        function complete(plan, data, message, failed) {
            if (!plan) return [];
            inFlight.delete(plan.key);
            const scope = feedbackScope(plan.command);
            feedback[scope] = { message: String(message || ""), error: Boolean(failed) };
            if (plan.command === "saveTimezone" && !failed) applyTimezone(data || plan.payload);
            if (plan.command === "saveNotifications" && !failed) applyNotifications(data || plan.payload);
            if (plan.command === "testNotification" && data && data.last_delivery) notifications.lastDelivery = clone(data.last_delivery);
            if (plan.command === "changePassword" && !failed) account = { ...account };
            if (plan.command === "clearSessions" && !failed) applyAccount(data || {});
            if (plan.command === "rotateMetricsToken" && !failed) applyMetrics(data || {});
            if (plan.command === "disableMetricsToken" && !failed) metrics = { enabled: false, revealedToken: "" };
            const effects = [effect("render", { area: scope })];
            if (plan.command === "saveTimezone" && !failed) effects.push(effect("reconcileSchedule"));
            if (["saveNotifications", "clearSessions", "rotateMetricsToken", "disableMetricsToken", "exportBackup", "verifyBackup", "restoreBackup"].includes(plan.command) && !failed) {
                effects.push(effect("refreshSnapshot", { stream: scope === "account" ? "account" : scope }));
            }
            return effects;
        }

        function dispatch(event = {}) {
            switch (event.type) {
                case "snapshotRequested": return request(event.stream);
                case "snapshotFailed": return fail(event.stream, event.requestID, event.error);
                case "timezoneSnapshotReceived": if (accept("timezone", event.requestID)) { applyTimezone(event.data); return [effect("render", { area: "timezone" }), effect("reconcileSchedule")]; } return [];
                case "timezoneDraftChanged": timezone.draft = String(event.timezone || "").trim(); feedback.timezone = { message: "", error: false }; return [effect("render", { area: "timezone" })];
                case "notificationSnapshotReceived": if (accept("notifications", event.requestID)) { applyNotifications(event.data); return [effect("render", { area: "notifications" })]; } return [];
                case "notificationDraftChanged": notifications = { ...notifications, ...(event.patch || {}) }; notifications.eventTypes = uniqueStrings(notifications.eventTypes); feedback.notifications = { message: "", error: false }; return [effect("render", { area: "notifications" })];
                case "accountSnapshotReceived": if (accept("account", event.requestID)) { applyAccount(event.data); return [effect("render", { area: "account" })]; } return [];
                case "passwordDraftChanged": {
                    const plan = planCommand("changePassword", event);
                    return [effect("passwordDraftPlanned", { valid: plan.enabled, reason: plan.reason || "" })];
                }
                case "metricsSnapshotReceived": if (accept("metrics", event.requestID)) { applyMetrics(event.data); return [effect("render", { area: "metrics" })]; } return [];
                case "metricsTokenHidden": metrics.revealedToken = ""; return [effect("render", { area: "metrics" })];
                case "backupSnapshotReceived": if (accept("backup", event.requestID)) { applyBackup(event.data); return [effect("render", { area: "backup" })]; } return [];
                case "backupFileSelected": backup.selectedFile = event.file ? { name: String(event.file.name || ""), size: Number(event.file.size) || 0 } : null; return [effect("render", { area: "backup" })];
                case "commandRequested": {
                    const plan = planCommand(event.command, event.payload || event);
                    if (!plan.enabled) return [effect("commandRejected", plan)];
                    inFlight.set(plan.key, true);
                    feedback[feedbackScope(plan.command)] = { message: "", error: false };
                    return [effect("executeCommand", { plan })];
                }
                case "commandCompleted": return complete(event.plan, event.data, event.message, false);
                case "commandFailed": return complete(event.plan, event.data, event.message, true);
                case "scheduledEvent": return scheduled && typeof scheduled.dispatch === "function" ? scheduled.dispatch(event.event || {}) : [];
                default: return [];
            }
        }

        function getView() {
            return clone({
                timezone,
                notifications,
                account,
                metrics,
                backup,
                feedback,
                streams,
                commands: { inFlight: Array.from(inFlight.keys()) },
                scheduled: scheduled && typeof scheduled.getView === "function" ? scheduled.getView() : null
            });
        }

        return Object.freeze({ dispatch, getView, planCommand: (command, payload) => clone(planCommand(command, payload)) });
    }

    return Object.freeze({ createStore });
}));
