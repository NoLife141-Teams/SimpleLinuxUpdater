(function initScheduledPolicyAdministrationInteraction(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) {
        module.exports = api;
    }
    if (root) {
        root.ScheduledPolicyAdministrationInteraction = api;
    }
}(typeof globalThis !== "undefined" ? globalThis : this, function scheduledPolicyAdministrationInteractionFactory() {
    "use strict";

    const weekdayOrder = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"];
    const streamNames = ["policies", "settings", "runs", "calendar", "job"];

    function cloneValue(value) {
        if (Array.isArray(value)) return value.map(cloneValue);
        if (!value || typeof value !== "object") return value;
        return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, cloneValue(item)]));
    }

    function normalizeWeekdayToken(value) {
        const token = String(value || "").trim().toLowerCase();
        const aliases = {
            mon: "mon", monday: "mon", tue: "tue", tues: "tue", tuesday: "tue",
            wed: "wed", wednesday: "wed", thu: "thu", thur: "thu", thurs: "thu", thursday: "thu",
            fri: "fri", friday: "fri", sat: "sat", saturday: "sat", sun: "sun", sunday: "sun"
        };
        return aliases[token] || "";
    }

    function normalizeWeekdays(values) {
        const seen = new Set();
        return (Array.isArray(values) ? values : [])
            .map(normalizeWeekdayToken)
            .filter(Boolean)
            .filter(value => !seen.has(value) && seen.add(value))
            .sort((left, right) => weekdayOrder.indexOf(left) - weekdayOrder.indexOf(right));
    }

    function normalizeList(value) {
        const seen = new Set();
        const values = Array.isArray(value) ? value : String(value || "").split(",");
        return values.map(item => String(item || "").trim()).filter(Boolean).filter(item => {
            const key = item.toLowerCase();
            if (seen.has(key)) return false;
            seen.add(key);
            return true;
        });
    }

    function normalizeTime(value, fallback) {
        const normalized = String(value || "").trim();
        return /^\d{2}:\d{2}$/.test(normalized) ? normalized : fallback;
    }

    function createEmptyBlackoutRow() {
        return { weekdays: [], start_time: "00:00", end_time: "06:00" };
    }

    function normalizeBlackoutRow(row) {
        return {
            weekdays: normalizeWeekdays(row && row.weekdays),
            start_time: normalizeTime(row && row.start_time, "00:00"),
            end_time: normalizeTime(row && row.end_time, "06:00")
        };
    }

    function validateTime(value, label, index, field) {
        const raw = String(value || "").trim();
        const match = raw.match(/^(\d{2}):(\d{2})$/);
        if (!match) throw new Error(`${label} ${index + 1}: ${field} must use HH:MM.`);
        const hours = Number(match[1]);
        const minutes = Number(match[2]);
        if (hours > 23 || minutes > 59) throw new Error(`${label} ${index + 1}: ${field} must be a real 24-hour time.`);
        return raw;
    }

    function validateBlackoutRows(rows, label) {
        if (!Array.isArray(rows)) throw new Error(`${label} must be an array.`);
        return rows.map((row, index) => {
            if (!row || typeof row !== "object" || Array.isArray(row)) {
                throw new Error(`${label} ${index + 1}: each item must be an object.`);
            }
            if (!Array.isArray(row.weekdays)) throw new Error(`${label} ${index + 1}: weekdays must be an array.`);
            const invalid = row.weekdays.find(day => !normalizeWeekdayToken(day));
            if (invalid !== undefined) throw new Error(`${label} ${index + 1}: invalid weekday "${String(invalid).trim()}".`);
            const weekdays = normalizeWeekdays(row.weekdays);
            if (!weekdays.length) throw new Error(`${label} ${index + 1}: choose at least one weekday.`);
            return {
                weekdays,
                start_time: validateTime(row.start_time, label, index, "start_time"),
                end_time: validateTime(row.end_time, label, index, "end_time")
            };
        });
    }

    function parseBlackoutJSON(raw, label) {
        const text = String(raw || "").trim();
        if (!text) return [];
        let parsed;
        try {
            parsed = JSON.parse(text);
        } catch (_) {
            throw new Error(`${label} must be valid JSON.`);
        }
        return validateBlackoutRows(parsed, label);
    }

    function defaultDraft() {
        return {
            id: "", name: "", enabled: true, target_tag: "", include_tags: [], exclude_tags: [], target_servers: [],
            package_scope: "security", upgrade_mode: "standard", execution_mode: "scan_only", cadence_kind: "daily",
            time_local: "02:00", weekdays: [], approval_timeout_minutes: 720
        };
    }

    function normalizeDraft(draft) {
        const base = defaultDraft();
        const source = draft && typeof draft === "object" ? draft : {};
        const cadence = source.cadence_kind === "weekly" ? "weekly" : "daily";
        const execution = ["scan_only", "approval_required", "auto_apply"].includes(source.execution_mode)
            ? source.execution_mode : base.execution_mode;
        return {
            ...base,
            id: String(source.id || "").trim(),
            name: String(source.name || "").trim(),
            enabled: source.enabled !== false,
            target_tag: String(source.target_tag || "").trim(),
            include_tags: normalizeList(source.include_tags),
            exclude_tags: normalizeList(source.exclude_tags),
            target_servers: normalizeList(source.target_servers),
            package_scope: source.package_scope === "full" ? "full" : "security",
            upgrade_mode: source.upgrade_mode === "full" ? "full" : "standard",
            execution_mode: execution,
            cadence_kind: cadence,
            time_local: normalizeTime(source.time_local, base.time_local),
            weekdays: cadence === "weekly" ? normalizeWeekdays(source.weekdays) : [],
            approval_timeout_minutes: execution === "approval_required" ? (Number(source.approval_timeout_minutes) || 720) : 0
        };
    }

    function validatePolicyDraft(draft, blackouts) {
        const normalized = normalizeDraft(draft);
        const errors = {};
        if (!normalized.name) errors.name = "Policy name is required.";
        if (!normalized.target_tag && !normalized.include_tags.length && !normalized.target_servers.length) {
            errors.target_tag = "At least one target tag, included tag, or explicit server is required.";
        }
        if (normalized.cadence_kind === "weekly" && !normalized.weekdays.length) {
            errors.weekdays = "Weekly policies require at least one weekday.";
        }
        if (Object.keys(errors).length) return { ok: false, errors, message: "Policy name and at least one target tag, included tag, or explicit server are required." };
        try {
            return {
                ok: true,
                payload: { ...normalized, policy_blackouts: validateBlackoutRows(blackouts, "Policy no-run window") },
                errors: {}
            };
        } catch (error) {
            return { ok: false, errors: { blackouts: error.message }, message: error.message };
        }
    }

    function formatWeekdays(weekdays) {
        const labels = { mon: "Mon", tue: "Tue", wed: "Wed", thu: "Thu", fri: "Fri", sat: "Sat", sun: "Sun" };
        const normalized = normalizeWeekdays(weekdays);
        return normalized.length ? normalized.map(day => labels[day]).join(", ") : "No weekdays selected";
    }

    function summaryFor(draft, blackouts, timezone) {
        const schedule = draft.cadence_kind === "weekly"
            ? `Every ${formatWeekdays(draft.weekdays)} at ${draft.time_local}`
            : `Daily at ${draft.time_local}`;
        const targets = [];
        if (draft.target_tag) targets.push(`tag=${draft.target_tag}`);
        if (draft.include_tags.length) targets.push(`include=${draft.include_tags.join(", ")}`);
        if (draft.exclude_tags.length) targets.push(`exclude=${draft.exclude_tags.join(", ")}`);
        if (draft.target_servers.length) targets.push(`servers=${draft.target_servers.join(", ")}`);
        const execution = draft.execution_mode.replace(/_/g, " ");
        const scope = draft.package_scope === "full" ? "Full updates" : "Security updates";
        const upgrade = draft.upgrade_mode === "full" ? "Full upgrade" : "Standard upgrade";
        const timeout = draft.execution_mode === "approval_required" ? `, ${draft.approval_timeout_minutes || 720} minute approval window` : "";
        const windows = blackouts.length ? `${blackouts.length} no-run window${blackouts.length === 1 ? "" : "s"} configured` : "No policy no-run windows";
        return {
            title: draft.name || "Unnamed policy",
            body: `${schedule} (${timezone || "UTC"}), ${execution.charAt(0).toUpperCase()}${execution.slice(1)}, ${scope}, ${upgrade}${timeout}, ${targets.join("; ") || "no target"}. ${windows}.`
        };
    }

    function emptyStream() {
        return { nextRequestId: 1, inFlight: null, queued: null, lastAcceptedRequestId: 0, lastError: "", data: null };
    }

    function createStore() {
        let draft = defaultDraft();
        let policyBlackouts = [];
        let globalBlackouts = [];
        let validation = {};
        let timezone = "UTC";
        let preview = { nextRequestId: 1, activeRequestId: 0, data: null, message: "", loading: false };
        const streams = Object.fromEntries(streamNames.map(name => [name, emptyStream()]));
        let selectedCalendarPolicyID = "";
        let selectedJob = null;
        const inFlightPolicyIDs = new Set();
        let globalSettingsInFlight = false;

        function effect(type, payload) { return { type, ...payload }; }

        function requestStream(stream, payload = {}) {
            const state = streams[stream];
            if (!state) return [];
            if (state.inFlight !== null) {
                state.queued = cloneValue(payload);
                return [];
            }
            const requestId = state.nextRequestId++;
            state.inFlight = requestId;
            state.lastError = "";
            return [effect("fetchSnapshot", { stream, requestId, ...cloneValue(payload) })];
        }

        function receiveStream(stream, requestId, data) {
            const state = streams[stream];
            if (!state || (requestId && state.inFlight !== requestId)) return [];
            state.data = cloneValue(data);
            state.lastAcceptedRequestId = requestId || state.lastAcceptedRequestId + 1;
            state.inFlight = null;
            state.lastError = "";
            const effects = [effect("render", { area: stream })];
            if (state.queued) {
                const queued = state.queued;
                state.queued = null;
                effects.push(...requestStream(stream, queued));
            }
            return effects;
        }

        function failStream(stream, requestId, error) {
            const state = streams[stream];
            if (!state || (requestId && state.inFlight !== requestId)) return [];
            state.inFlight = null;
            state.lastError = String(error || "Failed to refresh.");
            const effects = [effect("render", { area: stream }), effect("announce", { scope: stream, message: state.lastError, error: true })];
            if (state.queued) {
                const queued = state.queued;
                state.queued = null;
                effects.push(...requestStream(stream, queued));
            }
            return effects;
        }

        function requestPreview() {
            const result = validatePolicyDraft(draft, policyBlackouts);
            validation = cloneValue(result.errors || {});
            if (!result.ok) {
                preview.loading = false;
                preview.message = result.message || "Complete policy fields to preview matching servers.";
                return [effect("render", { area: "editor" })];
            }
            const requestId = preview.nextRequestId++;
            preview.activeRequestId = requestId;
            preview.loading = true;
            preview.message = "Refreshing target preview...";
            return [effect("fetchPreview", { requestId, payload: result.payload })];
        }

        function commandPlan(command, policyIDInput) {
            if (command === "savePolicy") {
                const result = validatePolicyDraft(draft, policyBlackouts);
                validation = cloneValue(result.errors || {});
                if (!result.ok) return { enabled: false, reason: result.message };
                const policyID = result.payload.id;
                const operationKey = policyID || "__new_policy__";
                if (inFlightPolicyIDs.has(operationKey)) return { enabled: false, reason: "Policy action is already in progress." };
                return { enabled: true, command: policyID ? "updatePolicy" : "createPolicy", policyID, operationKey, payload: result.payload };
            }
            if (command === "deletePolicy") {
                const policyID = String(policyIDInput || "").trim();
                if (!policyID) return { enabled: false, reason: "Policy is no longer available." };
                if (inFlightPolicyIDs.has(policyID)) return { enabled: false, reason: "Policy action is already in progress." };
                const policies = Array.isArray(streams.policies.data && streams.policies.data.items) ? streams.policies.data.items : [];
                const policy = policies.find(item => String(item.id) === policyID);
                return policy ? { enabled: true, command: "deletePolicy", policyID, operationKey: policyID, policy: cloneValue(policy) } : { enabled: false, reason: "Policy is no longer available." };
            }
            if (command === "saveGlobalSettings") {
                if (globalSettingsInFlight) return { enabled: false, reason: "Global settings are already being saved." };
                try {
                    return { enabled: true, command: "saveGlobalSettings", payload: { global_blackouts: validateBlackoutRows(globalBlackouts, "Global no-run window") } };
                } catch (error) {
                    return { enabled: false, reason: error.message };
                }
            }
            return { enabled: false, reason: "Unknown scheduled policy command." };
        }

        function dispatch(event) {
            const input = event && typeof event === "object" ? event : {};
            switch (input.type) {
                case "editorReset":
                    draft = defaultDraft(); policyBlackouts = []; validation = {}; preview = { ...preview, data: null, message: "", loading: false };
                    return [effect("render", { area: "editor" })];
                case "editorLoaded":
                    draft = normalizeDraft(input.policy); policyBlackouts = (Array.isArray(input.policy && input.policy.policy_blackouts) ? input.policy.policy_blackouts : []).map(normalizeBlackoutRow); validation = {};
                    return [effect("render", { area: "editor" })];
                case "editorChanged":
                    draft = normalizeDraft({ ...draft, ...(input.patch || {}) }); validation = {};
                    return [effect("render", { area: "editor" })];
                case "policyWeekdayToggled":
                    draft = normalizeDraft({ ...draft, weekdays: draft.weekdays.includes(normalizeWeekdayToken(input.day)) ? draft.weekdays.filter(day => day !== normalizeWeekdayToken(input.day)) : [...draft.weekdays, normalizeWeekdayToken(input.day)] });
                    return [effect("render", { area: "editor" })];
                case "blackoutRowsReceived":
                    if (input.kind === "policy") policyBlackouts = (Array.isArray(input.rows) ? input.rows : []).map(normalizeBlackoutRow);
                    if (input.kind === "global") globalBlackouts = (Array.isArray(input.rows) ? input.rows : []).map(normalizeBlackoutRow);
                    return [effect("render", { area: input.kind === "policy" ? "editor" : "settings" })];
                case "blackoutRowAdded": {
                    const rows = input.kind === "policy" ? policyBlackouts : globalBlackouts;
                    rows.push(createEmptyBlackoutRow());
                    return [effect("render", { area: input.kind === "policy" ? "editor" : "settings" })];
                }
                case "blackoutRowRemoved": {
                    const rows = input.kind === "policy" ? policyBlackouts : globalBlackouts;
                    if (Number.isInteger(input.index) && input.index >= 0) rows.splice(input.index, 1);
                    return [effect("render", { area: input.kind === "policy" ? "editor" : "settings" })];
                }
                case "blackoutRowChanged": {
                    const rows = input.kind === "policy" ? policyBlackouts : globalBlackouts;
                    if (rows[input.index] && ["start_time", "end_time"].includes(input.field)) rows[input.index][input.field] = String(input.value || "");
                    return [effect("render", { area: input.kind === "policy" ? "editor" : "settings" })];
                }
                case "blackoutWeekdayToggled": {
                    const rows = input.kind === "policy" ? policyBlackouts : globalBlackouts;
                    const row = rows[input.index];
                    const day = normalizeWeekdayToken(input.day);
                    if (row && day) row.weekdays = row.weekdays.includes(day) ? row.weekdays.filter(item => item !== day) : normalizeWeekdays([...row.weekdays, day]);
                    return [effect("render", { area: input.kind === "policy" ? "editor" : "settings" })];
                }
                case "blackoutJSONApplied":
                    try {
                        const rows = parseBlackoutJSON(input.raw, input.label || "No-run windows");
                        if (input.kind === "policy") policyBlackouts = rows;
                        if (input.kind === "global") globalBlackouts = rows;
                        return [effect("blackoutJSONAccepted", { kind: input.kind, message: `${input.label || "No-run windows"} JSON applied to the editor.` }), effect("render", { area: input.kind === "policy" ? "editor" : "settings" })];
                    } catch (error) {
                        return [effect("blackoutJSONRejected", { kind: input.kind, message: error.message })];
                    }
                case "previewRequested": return requestPreview();
                case "previewReceived":
                    if (input.requestId !== preview.activeRequestId) return [];
                    preview = { ...preview, data: cloneValue(input.preview), loading: false, message: "" };
                    return [effect("render", { area: "preview" })];
                case "previewFailed":
                    if (input.requestId !== preview.activeRequestId) return [];
                    preview = { ...preview, loading: false, message: String(input.error || "Failed to preview scheduled policy.") };
                    return [effect("render", { area: "preview" })];
                case "timezoneReceived":
                    timezone = String(input.timezone || timezone || "UTC");
                    return [effect("render", { area: "editor" }), effect("render", { area: "runs" })];
                case "snapshotRequested": return requestStream(input.stream, input.payload);
                case "snapshotReceived": return receiveStream(input.stream, input.requestId, input.data);
                case "snapshotFailed": return failStream(input.stream, input.requestId, input.error);
                case "calendarPolicySelected":
                    selectedCalendarPolicyID = String(input.policyID || "").trim();
                    return [effect("render", { area: "calendarFilter" })];
                case "jobSelected":
                    selectedJob = null;
                    return requestStream("job", { jobID: String(input.jobID || "").trim() });
                case "jobClosed":
                    selectedJob = null;
                    return [effect("render", { area: "job" })];
                case "jobReceived":
                    if (input.requestId && streams.job.inFlight !== input.requestId) return [];
                    selectedJob = cloneValue(input.job || null);
                    return receiveStream("job", input.requestId, input.data || input.job || null);
                case "commandRequested": {
                    const plan = commandPlan(input.command, input.policyID);
                    if (!plan.enabled) return [effect("commandRejected", { command: input.command, message: plan.reason })];
                    if (plan.operationKey) inFlightPolicyIDs.add(plan.operationKey);
                    if (plan.command === "saveGlobalSettings") globalSettingsInFlight = true;
                    return [effect("executeCommand", { plan })];
                }
                case "commandCompleted":
                case "commandFailed": {
                    const plan = input.plan || {};
                    if (plan.operationKey || plan.policyID) inFlightPolicyIDs.delete(String(plan.operationKey || plan.policyID));
                    if (plan.command === "saveGlobalSettings") globalSettingsInFlight = false;
                    const error = input.type === "commandFailed";
                    const message = String(input.message || (error ? "Scheduled policy action failed." : "Scheduled policy action completed."));
                    const effects = [effect("announce", { scope: plan.command || "policy", message, error })];
                    if (!error) effects.push(...["policies", "settings", "runs", "calendar"].flatMap(stream => requestStream(stream)));
                    return effects;
                }
                default: return [];
            }
        }

        function getView() {
            const policies = streams.policies.data && Array.isArray(streams.policies.data.items) ? streams.policies.data.items : [];
            return cloneValue({
                editor: { draft, policyBlackouts, globalBlackouts, validation, summary: summaryFor(draft, policyBlackouts, timezone), preview },
                timezone,
                snapshots: streams,
                policies,
                runs: streams.runs.data && Array.isArray(streams.runs.data.items) ? streams.runs.data.items : [],
                calendar: streams.calendar.data,
                selectedCalendarPolicyID,
                selectedJob,
                commands: { inFlightPolicyIDs: Array.from(inFlightPolicyIDs), globalSettingsInFlight }
            });
        }

        return Object.freeze({ dispatch, getView, validatePolicyDraft: () => validatePolicyDraft(draft, policyBlackouts), planCommand: (command, policyID) => cloneValue(commandPlan(command, policyID)) });
    }

    return Object.freeze({ createStore, normalizeWeekdays, normalizeBlackoutRow, validateBlackoutRows, validatePolicyDraft });
}));
