(function initAdminPageInteraction(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.AdminPageInteraction = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function adminPageInteractionFactory() {
    "use strict";

    const streamNames = ["timezone", "notifications", "account", "activity", "metrics", "backup"];
    const sectionDefinitions = [
        { id: "app-time", label: "App Time" },
        { id: "notifications", label: "Notifications" },
        { id: "account-security", label: "Account Security" },
        { id: "recent-activity", label: "Recent Activity" },
        { id: "scheduled-policies", label: "Policies" },
        { id: "scheduled-runs", label: "Scheduled Runs" },
        { id: "backup", label: "Backup" },
        { id: "metrics", label: "Metrics" }
    ];
    const sectionIDs = new Set(sectionDefinitions.map(section => section.id));
    const lazySectionIDs = new Set(["notifications", "recent-activity", "scheduled-policies", "scheduled-runs", "backup", "metrics"]);
    const destructiveCommands = new Set([
        "clearSessions",
        "clearOtherSessions",
        "revokeSession",
        "rotateMetricsToken",
        "disableMetricsToken",
        "restoreBackup"
    ]);

    function clone(value) {
        if (Array.isArray(value)) return value.map(clone);
        if (!value || typeof value !== "object") return value;
        return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, clone(item)]));
    }

    function sameValue(left, right) {
        return JSON.stringify(left) === JSON.stringify(right);
    }

    function uniqueStrings(values) {
        const seen = new Set();
        return (Array.isArray(values) ? values : []).map(value => String(value || "").trim()).filter(value => value && !seen.has(value) && seen.add(value));
    }

    function emptyStream() {
        return {
            nextRequestID: 1,
            activeRequestID: null,
            freshness: "unavailable",
            status: "unavailable",
            error: "",
            accepted: false,
            lastSuccessfulRefresh: ""
        };
    }

    function normalizeTimezoneChoice(value) {
        const normalized = String(value ?? "").trim();
        return normalized === "Local" ? "" : normalized;
    }

    function normalizeTimezone(data = {}) {
        const configured = normalizeTimezoneChoice(data.editable_timezone ?? data.editableTimezone ?? data.timezone);
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

    function normalizePasswordPolicy(data = {}) {
        const source = data.password_policy && typeof data.password_policy === "object"
            ? data.password_policy
            : data.passwordPolicy && typeof data.passwordPolicy === "object" ? data.passwordPolicy : {};
        return {
            minLength: Math.max(1, Number(source.min_length ?? source.minLength) || 10),
            maxLength: Math.max(1, Number(source.max_length ?? source.maxLength) || 64),
            requiresLetter: source.requires_letter ?? source.requiresLetter ?? true,
            requiresDigit: source.requires_digit ?? source.requiresDigit ?? true
        };
    }

    function normalizePasswordChangeOutcome(data = {}) {
        return {
            outcome: String(data.outcome || "").trim(),
            invalidationRequested: Boolean(data.invalidation_requested),
            invalidatedSessions: Math.max(0, Number(data.invalidated_sessions) || 0),
            preservedSessions: Math.max(0, Number(data.preserved_sessions) || 0),
            currentSessionPreserved: Boolean(data.current_session_preserved)
        };
    }

    function normalizeActivity(data = {}) {
        const items = (Array.isArray(data.items) ? data.items : []).map(item => {
            const id = Number(item?.id);
            return {
                id: Number.isSafeInteger(id) && id > 0 ? id : 0,
                createdAt: String(item?.created_at ?? item?.createdAt ?? "").trim(),
                createdAtDisplay: String(item?.created_at_display ?? item?.createdAtDisplay ?? "").trim(),
                actor: String(item?.actor || "unknown").trim() || "unknown",
                action: String(item?.action || "").trim(),
                status: String(item?.status || "").trim()
            };
        }).filter(item => item.id && item.action).slice(0, 8);
        return {
            items,
            total: Math.max(items.length, Number(data.total) || 0),
            limit: 8
        };
    }

    function normalizeBackup(data = {}) {
        return {
            blocked: Boolean(data.blocked),
            reason: String(data.reason || data.maintenance_reason || "").trim(),
            status: clone(data),
            recoveryHealth: normalizeRecoveryHealth(data.recovery_health)
        };
    }

    function normalizeRecoveryOperation(data = {}) {
        const state = normalizeRecoveryState(data.state);
        const rawSize = data.size_bytes;
        return {
            state,
            lastAttemptAt: String(data.last_attempt_at || "").trim(),
            lastSuccessAt: String(data.last_success_at || "").trim(),
            sizeBytes: rawSize === null || rawSize === undefined ? null : Math.max(0, Number(rawSize) || 0),
            message: String(data.message || "").trim()
        };
    }

    function normalizeRecoveryState(value) {
        const state = String(value || "").trim().toLowerCase();
        return ["healthy", "stale", "never", "failed", "unavailable"].includes(state) ? state : "unavailable";
    }

    function normalizeRecoveryHealth(data) {
        if (!data || typeof data !== "object") return null;
        const schedule = data.schedule && typeof data.schedule === "object" ? data.schedule : {};
        const retention = data.retention && typeof data.retention === "object" ? data.retention : {};
        return {
            state: normalizeRecoveryState(data.state),
            message: String(data.message || "").trim(),
            checkedAt: String(data.checked_at || "").trim(),
            staleAfterHours: Math.max(0, Number(data.stale_after_hours) || 0),
            export: normalizeRecoveryOperation(data.export),
            verification: normalizeRecoveryOperation(data.verification),
            schedule: {
                scheduled: Boolean(schedule.scheduled),
                nextBackupAt: String(schedule.next_backup_at || "").trim(),
                message: String(schedule.message || "").trim()
            },
            retention: {
                evidenceDays: Math.max(0, Number(retention.evidence_days) || 0),
                archiveRetainedByApp: Boolean(retention.archive_retained_by_app),
                automaticDeletion: Boolean(retention.automatic_deletion),
                evidenceDescription: String(retention.evidence_description || "").trim(),
                archiveDescription: String(retention.archive_description || "").trim()
            }
        };
    }

    function normalizeBackupReview(data = {}) {
        const archive = data.archive && typeof data.archive === "object" ? data.archive : {};
        const counts = data.safe_counts && typeof data.safe_counts === "object" ? data.safe_counts : {};
        const impact = data.impact && typeof data.impact === "object" ? data.impact : {};
        return {
            valid: Boolean(data.valid),
            compatible: Boolean(data.compatible),
            restoreReady: Boolean(data.restore_ready),
            archive: {
                format: String(archive.format || data.format || "").trim(),
                version: Number(archive.version ?? data.version) || 0,
                createdAt: String(archive.created_at || data.created_at || "").trim(),
                sizeBytes: Number(archive.size_bytes ?? data.archive_size_bytes) || 0
            },
            resources: clone(Array.isArray(data.resources) ? data.resources : []),
            missingResources: uniqueStrings(data.missing_resources),
            safeCounts: {
                servers: Number(counts.servers) || 0,
                policies: Number(counts.policies) || 0,
                jobs: Number(counts.jobs) || 0,
                sessions: Number(counts.sessions) || 0
            },
            impact: {
                sessionsInvalidated: Boolean(impact.sessions_invalidated),
                metricsAccessReplaced: Boolean(impact.metrics_access_replaced),
                maintenanceRequired: Boolean(impact.maintenance_required),
                downtimeExpected: Boolean(impact.downtime_expected),
                restartRequired: Boolean(impact.restart_required)
            },
            blockers: clone(Array.isArray(data.blockers) ? data.blockers : []),
            warnings: clone(Array.isArray(data.warnings) ? data.warnings : [])
        };
    }

    function createStore(options = {}) {
        const scheduled = options.scheduled || null;
        const streams = Object.fromEntries(streamNames.map(name => [name, emptyStream()]));
        const inFlight = new Map();
        let timezone = { configured: "", resolved: "UTC", draft: "" };
        let acceptedNotifications = { enabled: false, webhookURL: "", eventTypes: [], supportedEvents: [], lastDelivery: null };
        let notificationDraft = clone(acceptedNotifications);
        let account = {
            sessionCount: 0,
            sessions: [],
            otherSessionsExpanded: false,
            passwordPolicy: normalizePasswordPolicy(),
            passwordChangeOutcome: null
        };
        let activity = normalizeActivity();
        let metrics = { enabled: false, revealedToken: "" };
        let backup = {
            blocked: false,
            reason: "",
            status: null,
            recoveryHealth: null,
            selectedFile: null,
            credentialValid: false,
            credentialRevision: 0,
            review: null,
            reviewBinding: null
        };
        let activeSection = sectionDefinitions[0].id;
        let collapsedSections = [];
        const activatedSections = new Set();
        const feedback = Object.fromEntries(streamNames.map(name => [name, { message: "", error: false }]));

        function effect(type, details = {}) { return { type, ...details }; }
        function feedbackScope(command) {
            if (command === "saveTimezone") return "timezone";
            if (command === "saveNotifications" || command === "testNotification") return "notifications";
            if (command === "changePassword" || command === "clearSessions" || command === "clearOtherSessions" || command === "revokeSession") return "account";
            if (command.includes("MetricsToken")) return "metrics";
            if (command.includes("Backup")) return "backup";
            return "account";
        }
        function commandKey(command) { return command; }
        function currentBackupBinding() {
            return {
                file: clone(backup.selectedFile),
                credentialRevision: backup.credentialRevision
            };
        }
        function backupBindingMatches(binding) {
            return sameValue(binding || null, currentBackupBinding());
        }
        function scheduledDestructiveInFlight() {
            return Boolean(scheduled && typeof scheduled.getView === "function" && scheduled.getView()?.commands?.destructiveInFlight);
        }
        function destructiveInFlight() {
            return Array.from(inFlight.keys()).some(command => destructiveCommands.has(command)) || scheduledDestructiveInFlight();
        }
        function isDestructiveCommand(command) {
            return destructiveCommands.has(command) || command === "scheduled:deletePolicy";
        }

        function request(stream) {
            const state = streams[stream];
            if (!state) return [];
            const requestID = state.nextRequestID++;
            state.activeRequestID = requestID;
            state.freshness = "refreshing";
            state.status = "loading";
            state.error = "";
            return [effect("fetchSnapshot", { stream, requestID })];
        }

        function accept(stream, requestID, receivedAt) {
            const state = streams[stream];
            if (!state) return false;
            if (state.activeRequestID !== null && state.activeRequestID !== requestID) return false;
            if (state.activeRequestID === null && requestID) return false;
            state.activeRequestID = null;
            state.accepted = true;
            state.freshness = "fresh";
            state.status = "current";
            state.error = "";
            state.lastSuccessfulRefresh = String(receivedAt || state.lastSuccessfulRefresh || "");
            return true;
        }

        function fail(stream, requestID, error) {
            const state = streams[stream];
            if (!state || (requestID && state.activeRequestID !== requestID)) return [];
            state.activeRequestID = null;
            state.error = String(error || "Failed to refresh.");
            state.freshness = state.accepted ? "stale" : "unavailable";
            state.status = state.accepted ? "stale" : "failed";
            feedback[stream] = { message: state.error, error: true };
            return [effect("render", { area: stream }), effect("render", { area: "workspace" })];
        }

        function applyTimezone(data, preserveDraft = false) {
            const normalized = normalizeTimezone(data);
            timezone = { ...timezone, ...normalized, draft: preserveDraft ? timezone.draft : normalized.configured };
        }
        function notificationComparable(value) {
            return {
                enabled: Boolean(value.enabled),
                webhookURL: String(value.webhookURL || "").trim(),
                eventTypes: uniqueStrings(value.eventTypes).sort()
            };
        }
        function notificationsDirty() {
            return JSON.stringify(notificationComparable(notificationDraft)) !== JSON.stringify(notificationComparable(acceptedNotifications));
        }
        function notificationValidation() {
            const url = String(notificationDraft.webhookURL || "").trim();
            const validURL = !notificationDraft.enabled || /^https?:\/\/\S+$/i.test(url);
            return {
                valid: validURL,
                message: validURL ? "" : "An enabled webhook requires a valid HTTP or HTTPS URL."
            };
        }
        function applyNotifications(data, preserveDraft = false) {
            acceptedNotifications = normalizeNotifications(data);
            if (!preserveDraft) notificationDraft = clone(acceptedNotifications);
        }
        function applyAccount(data = {}) {
            const sessions = (Array.isArray(data.sessions) ? data.sessions : []).map(item => ({
                id: String(item?.id || "").trim(),
                current: Boolean(item?.current),
                createdAt: String(item?.created_at ?? item?.createdAt ?? "").trim(),
                lastSeenAt: String(item?.last_seen_at ?? item?.lastSeenAt ?? "").trim(),
                expiresAt: String(item?.expires_at ?? item?.expiresAt ?? "").trim(),
                clientIP: String(item?.client_ip ?? item?.clientIP ?? "").trim(),
                clientLabel: String(item?.client_label ?? item?.clientLabel ?? "").trim()
            })).filter(item => item.id);
            account = {
                sessionCount: Math.max(0, Number(data.count ?? data.session_count ?? sessions.length) || 0),
                sessions,
                otherSessionsExpanded: account.otherSessionsExpanded,
                passwordPolicy: normalizePasswordPolicy(data),
                passwordChangeOutcome: account.passwordChangeOutcome
            };
        }
        function applyActivity(data = {}) {
            activity = normalizeActivity(data);
        }
        function projectAccount() {
            const currentSession = account.sessions.find(item => item.current) || null;
            const otherSessions = account.sessions.filter(item => !item.current);
            return {
                ...account,
                currentSession,
                otherSessions,
                hiddenOtherSessionCount: account.otherSessionsExpanded ? 0 : Math.max(0, otherSessions.length - 3)
            };
        }
        function applyMetrics(data = {}) {
            metrics = { enabled: Boolean(data.enabled ?? data.configured ?? data.has_token), revealedToken: String(data.token ?? metrics.revealedToken ?? "") };
        }
        function applyBackup(data) { backup = { ...backup, ...normalizeBackup(data) }; }
        function normalizeSectionIDs(values) {
            const seen = new Set();
            return (Array.isArray(values) ? values : [])
                .map(value => String(value || "").trim())
                .filter(value => sectionIDs.has(value) && !seen.has(value) && seen.add(value));
        }
        function countSummary(count, singular, plural = `${singular}s`) {
            const value = Math.max(0, Number(count) || 0);
            return `${value} ${value === 1 ? singular : plural}`;
        }
        function freshnessSummary(summary, stale) {
            return stale ? `${summary} · Stale` : summary;
        }
        function backupSummary() {
            if (backup.blocked) return backup.reason || "Backup operations blocked";
            const state = backup.recoveryHealth?.state;
            if (!state) return "Backup operations ready";
            const labels = {
                healthy: "Recovery healthy",
                stale: "Recovery stale",
                never: "Recovery never verified",
                failed: "Recovery needs attention",
                unavailable: "Recovery unavailable"
            };
            return labels[state] || "Backup operations ready";
        }
        function scheduledStreamStatus(state) {
            if (!state) return { status: "unavailable", lastSuccessfulRefresh: "", error: "" };
            if (state.status) {
                return {
                    status: state.status,
                    lastSuccessfulRefresh: String(state.lastSuccessfulRefresh || ""),
                    error: String(state.lastError || "")
                };
            }
            if (state.inFlight !== null && state.inFlight !== undefined) return { status: "loading", lastSuccessfulRefresh: "", error: "" };
            if (state.lastError) return { status: state.data ? "stale" : "failed", lastSuccessfulRefresh: "", error: String(state.lastError) };
            if (state.data) return { status: "current", lastSuccessfulRefresh: "", error: "" };
            return { status: "unavailable", lastSuccessfulRefresh: "", error: "" };
        }
        function aggregateScheduledStatus(states) {
            const projected = states.map(scheduledStreamStatus);
            const timestamps = projected.map(state => state.lastSuccessfulRefresh).filter(Boolean).sort();
            const errorState = projected.find(state => state.error);
            let status = "current";
            if (projected.some(state => state.status === "loading")) status = "loading";
            else if (projected.some(state => state.status === "stale")) status = "stale";
            else if (projected.some(state => state.status === "failed")) status = "failed";
            else if (projected.some(state => state.status === "unavailable")) status = "unavailable";
            return {
                status,
                lastSuccessfulRefresh: timestamps.at(-1) || "",
                error: errorState?.error || ""
            };
        }
        function workspaceView(scheduledView) {
            const scheduledSnapshots = scheduledView?.snapshots || {};
            const policyItems = scheduledSnapshots.policies?.data?.items;
            const runItems = scheduledSnapshots.runs?.data?.items;
            const lifecycle = {
                "app-time": streams.timezone,
                notifications: streams.notifications,
                "account-security": streams.account,
                "recent-activity": streams.activity,
                "scheduled-policies": aggregateScheduledStatus([
                    scheduledSnapshots.policies,
                    scheduledSnapshots.settings,
                    scheduledSnapshots.calendar
                ]),
                "scheduled-runs": scheduledStreamStatus(scheduledSnapshots.runs),
                backup: streams.backup,
                metrics: streams.metrics
            };
            const summaries = {
                "app-time": streams.timezone.accepted ? freshnessSummary(timezone.resolved, streams.timezone.freshness === "stale") : "Timezone unavailable",
                notifications: streams.notifications.accepted ? freshnessSummary(acceptedNotifications.enabled ? "Webhook enabled" : "Webhook disabled", streams.notifications.freshness === "stale") : "Notification settings unavailable",
                "account-security": streams.account.accepted ? freshnessSummary(countSummary(account.sessionCount, "active session"), streams.account.freshness === "stale") : "Session status unavailable",
                "recent-activity": streams.activity.accepted ? freshnessSummary(countSummary(activity.items.length, "recent event"), streams.activity.freshness === "stale") : "Recent activity unavailable",
                "scheduled-policies": Array.isArray(policyItems) ? freshnessSummary(countSummary(policyItems.length, "saved policy", "saved policies"), Boolean(scheduledSnapshots.policies?.lastError)) : "Policy data unavailable",
                "scheduled-runs": Array.isArray(runItems) ? freshnessSummary(countSummary(runItems.length, "recent run"), Boolean(scheduledSnapshots.runs?.lastError)) : "Run history unavailable",
                backup: streams.backup.accepted ? freshnessSummary(backupSummary(), streams.backup.freshness === "stale") : "Backup status unavailable",
                metrics: streams.metrics.accepted ? freshnessSummary(metrics.enabled ? "Token enabled" : "Token disabled", streams.metrics.freshness === "stale") : "Metrics token status unavailable"
            };
            return {
                activeSection,
                collapsedSections: clone(collapsedSections),
                sections: sectionDefinitions.map(section => ({
                    ...section,
                    summary: summaries[section.id],
                    collapsed: collapsedSections.includes(section.id),
                    lifecycle: clone(lifecycle[section.id])
                }))
            };
        }

        function activateSectionData(sectionID, reason = "first-activation") {
            if (!lazySectionIDs.has(sectionID) || collapsedSections.includes(sectionID)) return [];
            if (reason !== "retry" && activatedSections.has(sectionID)) return [];
            activatedSections.add(sectionID);
            return [effect("loadSectionData", { sectionID, reason })];
        }

        function planCommand(command, payload = {}) {
            if (isDestructiveCommand(command) && destructiveInFlight()) {
                return {
                    enabled: false,
                    command,
                    key: commandKey(command),
                    reason: "Another destructive Admin action is already in progress."
                };
            }
            if (String(command).startsWith("scheduled:") && scheduled && typeof scheduled.planCommand === "function") {
                return clone(scheduled.planCommand(String(command).slice(10), payload));
            }
            const key = commandKey(command);
            if (inFlight.has(key)) return { enabled: false, command, key, reason: "This Admin action is already in progress." };
            switch (command) {
                case "saveTimezone": {
                    const value = normalizeTimezoneChoice(payload.timezone ?? timezone.draft);
                    if (value === normalizeTimezoneChoice(timezone.configured)) {
                        return { enabled: false, command, key, reason: "Choose a different timezone to save." };
                    }
                    return { enabled: true, command, key, payload: { timezone: value } };
                }
                case "saveNotifications":
                    if (!notificationsDirty()) return { enabled: false, command, key, reason: "Notification settings are unchanged." };
                    if (!notificationValidation().valid) return { enabled: false, command, key, reason: notificationValidation().message };
                    return { enabled: true, command, key, payload: { enabled: notificationDraft.enabled, webhook_url: notificationDraft.webhookURL, event_types: clone(notificationDraft.eventTypes) } };
                case "testNotification": return { enabled: true, command, key, payload: {} };
                case "changePassword": {
                    if (!payload.hasCurrentPassword) return { enabled: false, command, key, reason: "Current password is required." };
                    if (!payload.hasNewPassword) return { enabled: false, command, key, reason: "A new password is required." };
                    if (!payload.passwordsMatch) return { enabled: false, command, key, reason: "New passwords do not match." };
                    if (!payload.passwordValid) return { enabled: false, command, key, reason: "The new password does not meet the displayed requirements." };
                    return { enabled: true, command, key, payload: { invalidateOtherSessions: Boolean(payload.invalidateOtherSessions) } };
                }
                case "clearSessions": return { enabled: true, command, key, payload: {} };
                case "clearOtherSessions":
                    return account.sessions.filter(item => !item.current).length > 0
                        ? { enabled: true, command, key, payload: {} }
                        : { enabled: false, command, key, reason: "No other session is active." };
                case "revokeSession": {
                    const id = String(payload.id || "").trim();
                    return id
                        ? { enabled: true, command, key, payload: { id } }
                        : { enabled: false, command, key, reason: "Choose a session to revoke." };
                }
                case "rotateMetricsToken": case "disableMetricsToken": return { enabled: true, command, key, payload: {} };
                case "copyMetricsToken": return metrics.revealedToken ? { enabled: true, command, key, payload: { token: metrics.revealedToken } } : { enabled: false, command, key, reason: "No revealed metrics token is available." };
                case "exportBackup":
                    if (backup.blocked) return { enabled: false, command, key, reason: backup.reason || "Backup is unavailable." };
                    if (!payload.passphraseValid || !payload.passwordsMatch) return { enabled: false, command, key, reason: "A valid matching backup passphrase is required." };
                    return { enabled: true, command, key, payload: { includeKnownHosts: Boolean(payload.includeKnownHosts) } };
                case "verifyBackup":
                    if (backup.blocked) return { enabled: false, command, key, reason: backup.reason || "Backup is unavailable." };
                    if (!backup.selectedFile || !(payload.passphraseValid || backup.credentialValid)) return { enabled: false, command, key, reason: !backup.selectedFile ? "Choose a backup file." : "A valid backup passphrase is required." };
                    return { enabled: true, command, key, binding: currentBackupBinding(), payload: { file: clone(backup.selectedFile) } };
                case "restoreBackup":
                    if (backup.blocked) return { enabled: false, command, key, reason: backup.reason || "Backup is unavailable." };
                    if (!backup.selectedFile || !(payload.passphraseValid || backup.credentialValid)) return { enabled: false, command, key, reason: !backup.selectedFile ? "Choose a backup file." : "A valid backup passphrase is required." };
                    if (!backup.review || !backup.review.restoreReady || !backupBindingMatches(backup.reviewBinding)) {
                        return { enabled: false, command, key, reason: "Verify this exact backup and passphrase before restoring; the restore review is not ready." };
                    }
                    return { enabled: true, command, key, binding: currentBackupBinding(), payload: { file: clone(backup.selectedFile) } };
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
            if (plan.command === "testNotification" && data && data.last_delivery) {
                acceptedNotifications.lastDelivery = clone(data.last_delivery);
                notificationDraft.lastDelivery = clone(data.last_delivery);
            }
            if (plan.command === "changePassword" && !failed) {
                account = { ...account, passwordChangeOutcome: normalizePasswordChangeOutcome(data) };
            }
            if (plan.command === "clearSessions" && !failed) applyAccount(data || {});
            if (plan.command === "rotateMetricsToken" && !failed) applyMetrics(data || {});
            if (plan.command === "disableMetricsToken" && !failed) metrics = { enabled: false, revealedToken: "" };
            if (plan.command === "verifyBackup") {
                if (!failed && backupBindingMatches(plan.binding)) {
                    backup.review = normalizeBackupReview(data || {});
                    backup.reviewBinding = clone(plan.binding);
                } else if (!failed) {
                    feedback.backup = { message: "The restore review response was discarded because the archive or passphrase changed.", error: false };
                } else if (backupBindingMatches(plan.binding)) {
                    backup.review = null;
                    backup.reviewBinding = null;
                }
            }
            if (plan.command === "restoreBackup" && !failed) {
                backup.selectedFile = null;
                backup.review = null;
                backup.reviewBinding = null;
            }
            const effects = [effect("render", { area: scope })];
            if (plan.command === "saveTimezone" && !failed) effects.push(effect("reconcileSchedule"));
            if (["saveNotifications", "clearSessions", "clearOtherSessions", "revokeSession", "rotateMetricsToken", "disableMetricsToken", "exportBackup", "verifyBackup", "restoreBackup"].includes(plan.command) && !failed) {
                effects.push(effect("refreshSnapshot", { stream: scope === "account" ? "account" : scope }));
            }
            return effects;
        }

        function dispatch(event = {}) {
            switch (event.type) {
                case "snapshotRequested": return request(event.stream);
                case "snapshotFailed": return fail(event.stream, event.requestID, event.error);
                case "timezoneSnapshotReceived": {
                    const preserveDraft = streams.timezone.accepted
                        && normalizeTimezoneChoice(timezone.draft) !== normalizeTimezoneChoice(timezone.configured);
                    if (accept("timezone", event.requestID, event.receivedAt)) {
                        applyTimezone(event.data, preserveDraft);
                        return [effect("render", { area: "timezone" }), effect("render", { area: "workspace" }), effect("reconcileSchedule")];
                    }
                    return [];
                }
                case "timezoneDraftChanged": timezone.draft = normalizeTimezoneChoice(event.timezone); feedback.timezone = { message: "", error: false }; return [effect("render", { area: "timezone" })];
                case "notificationSnapshotReceived": {
                    const preserveDraft = streams.notifications.accepted && notificationsDirty();
                    if (accept("notifications", event.requestID, event.receivedAt)) {
                        applyNotifications(event.data, preserveDraft);
                        return [effect("render", { area: "notifications" }), effect("render", { area: "workspace" })];
                    }
                    return [];
                }
                case "notificationDraftChanged":
                    notificationDraft = { ...notificationDraft, ...(event.patch || {}) };
                    notificationDraft.webhookURL = String(notificationDraft.webhookURL || "").trim();
                    notificationDraft.eventTypes = uniqueStrings(notificationDraft.eventTypes);
                    feedback.notifications = { message: "", error: false };
                    return [effect("render", { area: "notifications" })];
                case "notificationDiscardRequested":
                    notificationDraft = clone(acceptedNotifications);
                    feedback.notifications = { message: "", error: false };
                    return [effect("render", { area: "notifications" })];
                case "timezoneDiscardRequested":
                    timezone.draft = timezone.configured;
                    feedback.timezone = { message: "", error: false };
                    return [effect("render", { area: "timezone" })];
                case "accountSnapshotReceived": if (accept("account", event.requestID, event.receivedAt)) { applyAccount(event.data); return [effect("render", { area: "account" }), effect("render", { area: "workspace" })]; } return [];
                case "activitySnapshotReceived": if (accept("activity", event.requestID, event.receivedAt)) { applyActivity(event.data); return [effect("render", { area: "activity" }), effect("render", { area: "workspace" })]; } return [];
                case "sessionListExpandedChanged": account.otherSessionsExpanded = Boolean(event.expanded); return [effect("render", { area: "account" })];
                case "passwordDraftChanged": {
                    const plan = planCommand("changePassword", event);
                    return [effect("passwordDraftPlanned", { valid: plan.enabled, reason: plan.reason || "" })];
                }
                case "metricsSnapshotReceived": if (accept("metrics", event.requestID, event.receivedAt)) { applyMetrics(event.data); return [effect("render", { area: "metrics" }), effect("render", { area: "workspace" })]; } return [];
                case "metricsTokenHidden": metrics.revealedToken = ""; return [effect("render", { area: "metrics" })];
                case "backupSnapshotReceived": if (accept("backup", event.requestID, event.receivedAt)) { applyBackup(event.data); return [effect("render", { area: "backup" }), effect("render", { area: "workspace" })]; } return [];
                case "backupFileSelected":
                    backup.selectedFile = event.file ? {
                        name: String(event.file.name || ""),
                        size: Number(event.file.size) || 0,
                        lastModified: Number(event.file.lastModified) || 0
                    } : null;
                    backup.review = null;
                    backup.reviewBinding = null;
                    feedback.backup = { message: backup.selectedFile ? "Verify the selected archive before restoring." : "", error: false };
                    return [effect("render", { area: "backup" })];
                case "backupPassphraseChanged":
                    backup.credentialValid = Boolean(event.valid);
                    backup.credentialRevision += 1;
                    backup.review = null;
                    backup.reviewBinding = null;
                    feedback.backup = {
                        message: backup.selectedFile ? "The restore review expired because the passphrase changed. Verify again." : "",
                        error: false
                    };
                    return [effect("render", { area: "backup" })];
                case "sectionPreferencesRestored":
                    collapsedSections = normalizeSectionIDs(event.collapsedSections);
                    return [effect("render", { area: "workspace" })];
                case "sectionNavigationRequested": {
                    const sectionID = String(event.sectionID || "").trim();
                    if (!sectionIDs.has(sectionID)) return [];
                    activeSection = sectionID;
                    collapsedSections = collapsedSections.filter(value => value !== sectionID);
                    return [
                        effect("render", { area: "workspace" }),
                        effect("persistSectionPreferences", { collapsedSections: clone(collapsedSections) }),
                        effect("focusSection", { sectionID }),
                        ...activateSectionData(sectionID)
                    ];
                }
                case "sectionCollapseToggled": {
                    const sectionID = String(event.sectionID || "").trim();
                    if (!sectionIDs.has(sectionID)) return [];
                    const expanding = collapsedSections.includes(sectionID);
                    collapsedSections = collapsedSections.includes(sectionID)
                        ? collapsedSections.filter(value => value !== sectionID)
                        : [...collapsedSections, sectionID];
                    return [
                        effect("render", { area: "workspace" }),
                        effect("persistSectionPreferences", { collapsedSections: clone(collapsedSections) }),
                        ...(expanding ? activateSectionData(sectionID) : [])
                    ];
                }
                case "sectionActivated": {
                    const sectionID = String(event.sectionID || "").trim();
                    if (!sectionIDs.has(sectionID) || collapsedSections.includes(sectionID)) return [];
                    const effects = activateSectionData(sectionID);
                    if (sectionID !== activeSection) {
                        activeSection = sectionID;
                        effects.unshift(effect("render", { area: "workspace" }));
                    }
                    return effects;
                }
                case "sectionRetryRequested": {
                    const sectionID = String(event.sectionID || "").trim();
                    if (!sectionIDs.has(sectionID)) return [];
                    return lazySectionIDs.has(sectionID)
                        ? activateSectionData(sectionID, "retry")
                        : [effect("loadSectionData", { sectionID, reason: "retry" })];
                }
                case "commandRequested": {
                    const plan = planCommand(event.command, event.payload || event);
                    if (!plan.enabled) return [effect("commandRejected", plan)];
                    inFlight.set(plan.key, true);
                    feedback[feedbackScope(plan.command)] = { message: "", error: false };
                    if (plan.command === "changePassword") account = { ...account, passwordChangeOutcome: null };
                    return [effect("executeCommand", { plan })];
                }
                case "commandCompleted": return complete(event.plan, event.data, event.message, false);
                case "commandFailed": return complete(event.plan, event.data, event.message, true);
                case "scheduledEvent": return scheduled && typeof scheduled.dispatch === "function" ? scheduled.dispatch(event.event || {}) : [];
                default: return [];
            }
        }

        function getView() {
            const scheduledView = scheduled && typeof scheduled.getView === "function" ? scheduled.getView() : null;
            const timezoneDirty = normalizeTimezoneChoice(timezone.draft) !== normalizeTimezoneChoice(timezone.configured);
            const notificationDirty = notificationsDirty();
            const dirtySections = [];
            if (timezoneDirty) dirtySections.push("app-time");
            if (notificationDirty) dirtySections.push("notifications");
            if (scheduledView?.editor?.dirty || scheduledView?.globalSettings?.dirty) dirtySections.push("scheduled-policies");
            return clone({
                timezone: { ...timezone, dirty: timezoneDirty },
                notifications: { ...notificationDraft, dirty: notificationDirty, ...notificationValidation() },
                account: projectAccount(),
                activity,
                metrics,
                backup,
                feedback,
                streams,
                commands: { inFlight: Array.from(inFlight.keys()), destructiveInFlight: destructiveInFlight() },
                scheduled: scheduledView,
                workspace: workspaceView(scheduledView),
                dirtySections,
                hasMeaningfulDirty: dirtySections.length > 0
            });
        }

        return Object.freeze({ dispatch, getView, planCommand: (command, payload) => clone(planCommand(command, payload)) });
    }

    return Object.freeze({ createStore });
}));
