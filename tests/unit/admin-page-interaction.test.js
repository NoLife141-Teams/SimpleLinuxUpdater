const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

const { createStore } = require("../../static/js/admin-page-interaction.js");

function effect(effects, type) {
    return effects.find(item => item.type === type);
}

test("timezone administration retains accepted facts and rejects stale responses", () => {
    const store = createStore();
    const first = effect(store.dispatch({ type: "snapshotRequested", stream: "timezone" }), "fetchSnapshot");
    store.dispatch({ type: "timezoneSnapshotReceived", requestID: first.requestID, data: { timezone: "America/Toronto", resolved_timezone: "EDT" } });
    const second = effect(store.dispatch({ type: "snapshotRequested", stream: "timezone" }), "fetchSnapshot");
    assert.equal(store.getView().timezone.configured, "America/Toronto");
    assert.equal(store.getView().streams.timezone.freshness, "refreshing");
    store.dispatch({ type: "timezoneSnapshotReceived", requestID: first.requestID, data: { timezone: "UTC" } });
    assert.equal(store.getView().timezone.configured, "America/Toronto");
    store.dispatch({ type: "snapshotFailed", stream: "timezone", requestID: second.requestID, error: "offline" });
    assert.equal(store.getView().streams.timezone.freshness, "stale");
    assert.equal(store.getView().timezone.configured, "America/Toronto");
});

test("unscoped Admin snapshots cannot bypass an active request", () => {
    const store = createStore();
    store.dispatch({ type: "notificationSnapshotReceived", data: { enabled: true, webhook_configured: true, webhook_url_masked: "https://new.example.test/••••" } });
    const active = effect(store.dispatch({ type: "snapshotRequested", stream: "notifications" }), "fetchSnapshot");

    store.dispatch({ type: "notificationSnapshotReceived", data: { enabled: false, webhook_configured: true, webhook_url_masked: "https://stale.example.test/••••" } });

    assert.equal(store.getView().notifications.enabled, true);
    assert.equal(store.getView().notifications.webhookURLMasked, "https://new.example.test/••••");
    assert.equal(store.getView().streams.notifications.freshness, "refreshing");
    store.dispatch({ type: "notificationSnapshotReceived", requestID: active.requestID, data: { enabled: false, webhook_configured: true, webhook_url_masked: "https://accepted.example.test/••••" } });
    assert.equal(store.getView().notifications.webhookURLMasked, "https://accepted.example.test/••••");
});

test("timezone save is deduplicated and requests schedule reconciliation", () => {
    const store = createStore();
    store.dispatch({ type: "timezoneDraftChanged", timezone: "Europe/Paris" });
    const first = effect(store.dispatch({ type: "commandRequested", command: "saveTimezone" }), "executeCommand");
    assert.deepEqual(first.plan.payload, { timezone: "Europe/Paris" });
    assert.equal(store.dispatch({ type: "commandRequested", command: "saveTimezone" })[0].type, "commandRejected");
    const completed = store.dispatch({ type: "commandCompleted", plan: first.plan, data: { timezone: "Europe/Paris", resolved_timezone: "CEST" }, message: "App timezone saved." });
    assert.equal(completed.some(item => item.type === "reconcileSchedule"), true);
    assert.equal(store.getView().timezone.resolved, "CEST");
    assert.equal(store.getView().feedback.timezone.message, "App timezone saved.");
});

test("timezone save is enabled only when the accepted choice changes", () => {
    const store = createStore();
    store.dispatch({
        type: "timezoneSnapshotReceived",
        data: { editable_timezone: "America/Toronto", resolved_timezone: "America/Toronto" },
    });

    assert.equal(store.getView().timezone.dirty, false);
    assert.equal(store.planCommand("saveTimezone").enabled, false);

    store.dispatch({ type: "timezoneDraftChanged", timezone: "Europe/Paris" });
    assert.equal(store.getView().timezone.dirty, true);
    assert.equal(store.planCommand("saveTimezone").enabled, true);

    store.dispatch({ type: "timezoneDraftChanged", timezone: "America/Toronto" });
    assert.equal(store.getView().timezone.dirty, false);
    assert.equal(store.planCommand("saveTimezone").enabled, false);
});

test("notification administration owns settings, delivery, and command lifecycle", () => {
    const store = createStore();
    store.dispatch({ type: "notificationSnapshotReceived", data: { enabled: true, webhook_configured: true, webhook_url_masked: "https://hooks.example.test/••••", event_types: ["update.complete", "update.complete"] } });
    store.dispatch({ type: "notificationDiagnosticsReceived", data: { last_attempt: { outcome: "succeeded", attempted_at: "2026-07-10T12:00:00Z", completed_at: "2026-07-10T12:00:01Z", duration_ms: 1000 } } });
    assert.deepEqual(store.getView().notifications.eventTypes, ["update.complete"]);
    store.dispatch({ type: "notificationDraftChanged", patch: { enabled: false } });
    const save = effect(store.dispatch({ type: "commandRequested", command: "saveNotifications" }), "executeCommand");
    assert.deepEqual(save.plan.payload, {
        enabled: false,
        webhook_url_intent: "preserve",
        event_types: ["update.complete"],
        discord: { enabled: false, webhook_url_intent: "preserve" },
        telegram: { enabled: false, credentials_intent: "preserve" },
    });
    store.dispatch({ type: "commandCompleted", plan: save.plan, data: { enabled: false, webhook_configured: true, webhook_url_masked: "https://hooks.example.test/••••", event_types: ["update.complete"] } });
    store.dispatch({ type: "notificationDraftChanged", patch: { webhookURLIntent: "replace", replacementProvided: true, replacementValid: true } });
    const replace = store.planCommand("saveNotifications");
    assert.deepEqual(replace.payload, {
        enabled: false,
        webhook_url_intent: "replace",
        event_types: ["update.complete"],
        discord: { enabled: false, webhook_url_intent: "preserve" },
        telegram: { enabled: false, credentials_intent: "preserve" },
    });
    assert.equal(JSON.stringify(store.getView()).includes("hooks.example.test/y"), false);
    const delivery = effect(store.dispatch({ type: "commandRequested", command: "testNotification" }), "executeCommand");
    store.dispatch({ type: "commandFailed", plan: delivery.plan, data: { last_attempt: { outcome: "failed", attempts: 3, consecutive_failures: 3, error: "Webhook delivery failed." } }, message: "Notification test failed." });
    assert.equal(store.getView().notificationDiagnostics.lastAttempt.attempts, 3);
    assert.equal(store.getView().notificationDiagnostics.lastAttempt.consecutiveFailures, 3);
    assert.equal(store.getView().feedback.notifications.error, true);
});

test("notification settings, diagnostics, and test outcomes retain accepted state independently", () => {
    const store = createStore();
    const settingsRequest = effect(store.dispatch({ type: "snapshotRequested", stream: "notifications" }), "fetchSnapshot");
    const diagnosticsRequest = effect(store.dispatch({ type: "snapshotRequested", stream: "notificationDiagnostics" }), "fetchSnapshot");
    store.dispatch({
        type: "notificationSnapshotReceived",
        requestID: settingsRequest.requestID,
        data: {
            enabled: true,
            webhook_configured: true,
            webhook_url_masked: "https://hooks.example.test/••••",
            event_types: ["update.complete"],
        },
    });
    store.dispatch({
        type: "snapshotFailed",
        stream: "notificationDiagnostics",
        requestID: diagnosticsRequest.requestID,
        error: "diagnostics offline",
    });
    assert.equal(store.getView().notifications.enabled, true);
    assert.equal(store.getView().streams.notifications.status, "current");
    assert.equal(store.getView().streams.notificationDiagnostics.status, "failed");

    const acceptedDiagnostics = effect(store.dispatch({ type: "snapshotRequested", stream: "notificationDiagnostics" }), "fetchSnapshot");
    store.dispatch({
        type: "notificationDiagnosticsReceived",
        requestID: acceptedDiagnostics.requestID,
        data: {
            last_attempt: {
                outcome: "retrying",
                event_type: "update.complete",
                attempted_at: "2026-07-10T12:00:00Z",
                completed_at: "2026-07-10T12:00:01Z",
                duration_ms: 1000,
                status_code: 503,
                consecutive_failures: 2,
                next_retry_at: "2026-07-10T12:00:03Z",
                headers: { authorization: "never-store" },
                response_body: "never-store",
            },
        },
    });
    assert.equal(store.getView().notificationDiagnostics.lastAttempt.outcome, "retrying");
    assert.equal(store.getView().notificationDiagnostics.lastAttempt.nextRetryAt, "2026-07-10T12:00:03Z");
    assert.equal(JSON.stringify(store.getView()).includes("never-store"), false);

    const failedSettingsRefresh = effect(store.dispatch({ type: "snapshotRequested", stream: "notifications" }), "fetchSnapshot");
    store.dispatch({ type: "snapshotFailed", stream: "notifications", requestID: failedSettingsRefresh.requestID, error: "settings offline" });
    assert.equal(store.getView().streams.notifications.status, "stale");
    assert.equal(store.getView().streams.notificationDiagnostics.status, "current");
    assert.equal(store.getView().notificationDiagnostics.lastAttempt.statusCode, 503);

    const testCommand = effect(store.dispatch({ type: "commandRequested", command: "testNotification" }), "executeCommand");
    store.dispatch({
        type: "commandFailed",
        plan: testCommand.plan,
        data: {
            last_attempt: {
                outcome: "failed",
                attempted_at: "2026-07-10T12:01:00Z",
                completed_at: "2026-07-10T12:01:01Z",
                consecutive_failures: 3,
                next_retry_at: "2026-07-10T12:01:03Z",
            },
        },
        message: "Notification test failed.",
    });
    assert.equal(store.getView().notificationDiagnostics.lastAttempt.outcome, "failed");
    assert.equal(store.getView().notificationDiagnostics.lastAttempt.nextRetryAt, "");
    assert.equal(store.getView().streams.notificationDiagnostics.status, "current");
    assert.equal(store.getView().streams.notifications.status, "stale");
    assert.equal(store.getView().feedback.notifications.error, true);
});

test("notification URL intents preserve accepted configuration and never retain replacement secrets", () => {
    const store = createStore();
    store.dispatch({ type: "notificationSnapshotReceived", data: {
        enabled: true,
        webhook_configured: true,
        webhook_url_masked: "https://hooks.example.test/••••",
        webhook_url: "https://hooks.example.test/path?token=server-secret",
        event_types: ["update.complete"],
    } });
    assert.equal(JSON.stringify(store.getView()).includes("server-secret"), false);

    store.dispatch({ type: "notificationDraftChanged", patch: { enabled: false } });
    assert.deepEqual(store.planCommand("saveNotifications").payload, {
        enabled: false,
        webhook_url_intent: "preserve",
        event_types: ["update.complete"],
        discord: { enabled: false, webhook_url_intent: "preserve" },
        telegram: { enabled: false, credentials_intent: "preserve" },
    });

    store.dispatch({ type: "notificationDraftChanged", patch: {
        webhookURLIntent: "replace",
        replacementProvided: false,
        replacementValid: true,
        webhookURL: "https://replacement.example.test/hook?token=browser-secret",
    } });
    assert.equal(store.planCommand("saveNotifications").enabled, false);
    assert.equal(store.planCommand("saveNotifications").reason, "Enter the replacement webhook URL.");
    assert.equal(JSON.stringify(store.getView()).includes("browser-secret"), false);
    store.dispatch({ type: "notificationDraftChanged", patch: { replacementProvided: true, replacementValid: false } });
    assert.equal(store.planCommand("saveNotifications").enabled, false);
    store.dispatch({ type: "notificationDraftChanged", patch: { replacementValid: true } });
    assert.equal(store.planCommand("saveNotifications").enabled, true);

    store.dispatch({ type: "notificationDraftChanged", patch: { webhookURLIntent: "clear", enabled: true } });
    assert.equal(store.planCommand("saveNotifications").reason, "Disable webhook delivery before clearing the configured URL.");
    store.dispatch({ type: "notificationDraftChanged", patch: { enabled: false } });
    assert.deepEqual(store.planCommand("saveNotifications").payload, {
        enabled: false,
        webhook_url_intent: "clear",
        event_types: ["update.complete"],
        discord: { enabled: false, webhook_url_intent: "preserve" },
        telegram: { enabled: false, credentials_intent: "preserve" },
    });
});

test("Discord and Telegram drafts validate independently and exclude credentials", () => {
    const store = createStore();
    store.dispatch({
        type: "notificationSnapshotReceived",
        data: {
            enabled: false,
            event_types: ["update.complete"],
            discord: {
                enabled: true,
                configured: true,
                webhook_url_masked: "https://discord.com/••••",
                webhook_url: "https://discord.com/api/webhooks/1/server-secret",
            },
            telegram: {
                enabled: false,
                configured: true,
                bot_token_masked: "123456:••••",
                chat_id_masked: "••••7890",
                bot_token: "server-token",
                chat_id: "-1001234567890",
            },
        },
    });
    assert.equal(JSON.stringify(store.getView()).includes("server-secret"), false);
    assert.equal(JSON.stringify(store.getView()).includes("server-token"), false);

    store.dispatch({ type: "notificationDraftChanged", patch: {
        discordWebhookURLIntent: "replace",
        discordReplacementProvided: true,
        discordReplacementValid: true,
        telegramEnabled: true,
    } });
    const plan = store.planCommand("saveNotifications");
    assert.equal(plan.enabled, true);
    assert.deepEqual(plan.payload.discord, { enabled: true, webhook_url_intent: "replace" });
    assert.deepEqual(plan.payload.telegram, { enabled: true, credentials_intent: "preserve" });

    store.dispatch({ type: "notificationDraftChanged", patch: {
        telegramCredentialsIntent: "replace",
        telegramReplacementProvided: true,
        telegramReplacementValid: false,
    } });
    assert.equal(store.planCommand("saveNotifications").enabled, false);
    assert.match(store.planCommand("saveNotifications").reason, /Telegram/i);

    const testPlan = store.planCommand("testNotification", { destination: "telegram" });
    assert.deepEqual(testPlan.payload, { destination: "telegram" });
});

test("section drafts normalize accepted values, discard independently, and exclude secrets", () => {
    const scheduled = {
        getView() {
            return {
                editor: { dirty: true },
                globalSettings: { dirty: false },
            };
        },
    };
    const store = createStore({ scheduled });
    store.dispatch({ type: "timezoneSnapshotReceived", data: { editable_timezone: "America/Toronto", resolved_timezone: "America/Toronto" } });
    store.dispatch({ type: "notificationSnapshotReceived", data: {
        enabled: true,
        webhook_configured: true,
        webhook_url_masked: "https://hooks.example.test/••••",
        event_types: ["update.complete"],
    } });

    assert.equal(store.getView().notifications.dirty, false);
    assert.equal(store.planCommand("saveNotifications").enabled, false);

    store.dispatch({ type: "notificationDraftChanged", patch: {
        webhookURL: "https://secret.example.test/path?token=never-store",
        webhookURLIntent: "replace",
        replacementProvided: true,
        replacementValid: true,
        eventTypes: ["update.complete", "update.complete"],
    } });
    assert.equal(store.getView().notifications.dirty, true);
    assert.equal(store.planCommand("saveNotifications").enabled, true);

    const notificationRefresh = effect(store.dispatch({ type: "snapshotRequested", stream: "notifications" }), "fetchSnapshot");
    store.dispatch({ type: "notificationSnapshotReceived", requestID: notificationRefresh.requestID, data: {
        enabled: true,
        webhook_configured: true,
        webhook_url_masked: "https://server-updated.example.test/••••",
        event_types: ["update.complete"],
    } });
    assert.equal(store.getView().notifications.webhookURLIntent, "replace");
    assert.equal(JSON.stringify(store.getView()).includes("never-store"), false);
    assert.equal(store.getView().notifications.dirty, true);

    store.dispatch({ type: "notificationDiscardRequested" });
    assert.equal(store.getView().notifications.webhookURLMasked, "https://server-updated.example.test/••••");
    assert.equal(store.getView().notifications.webhookURLIntent, "preserve");
    assert.equal(store.getView().notifications.dirty, false);

    store.dispatch({ type: "timezoneDraftChanged", timezone: "Europe/Paris" });
    store.dispatch({ type: "backupFileSelected", file: { name: "secret.slubkp", size: 20, contents: "never" } });
    store.dispatch({ type: "passwordDraftChanged", hasCurrentPassword: true, hasNewPassword: true, passwordsMatch: true, password: "never-store" });
    const view = store.getView();
    assert.deepEqual(view.dirtySections, ["app-time", "scheduled-policies"]);
    assert.equal(view.hasMeaningfulDirty, true);
    assert.equal(JSON.stringify(view).includes("never-store"), false);

    const timezoneRefresh = effect(store.dispatch({ type: "snapshotRequested", stream: "timezone" }), "fetchSnapshot");
    store.dispatch({ type: "timezoneSnapshotReceived", requestID: timezoneRefresh.requestID, data: {
        editable_timezone: "America/New_York",
        resolved_timezone: "America/New_York",
    } });
    assert.equal(store.getView().timezone.draft, "Europe/Paris");
    assert.equal(store.getView().timezone.dirty, true);

    store.dispatch({ type: "timezoneDiscardRequested" });
    assert.equal(store.getView().timezone.draft, "America/New_York");
    assert.equal(store.getView().timezone.dirty, false);
    assert.deepEqual(store.getView().dirtySections, ["scheduled-policies"]);
});

test("account and metrics administration excludes secrets and clears token reveal", () => {
    const store = createStore();
    store.dispatch({ type: "accountSnapshotReceived", data: {
        count: 4,
        password_policy: { min_length: 12, max_length: 80, requires_letter: true, requires_digit: true }
    } });
    assert.deepEqual(store.getView().account.passwordPolicy, {
        minLength: 12,
        maxLength: 80,
        requiresLetter: true,
        requiresDigit: true
    });
    assert.equal(store.planCommand("changePassword", {
        hasCurrentPassword: true,
        hasNewPassword: true,
        passwordsMatch: false,
        passwordValid: true
    }).enabled, false);
    assert.equal(store.planCommand("changePassword", {
        hasCurrentPassword: true,
        hasNewPassword: true,
        passwordsMatch: true,
        passwordValid: false
    }).enabled, false);
    const password = effect(store.dispatch({
        type: "commandRequested",
        command: "changePassword",
        payload: {
            hasCurrentPassword: true,
            hasNewPassword: true,
            passwordsMatch: true,
            passwordValid: true,
            invalidateOtherSessions: true
        }
    }), "executeCommand");
    assert.deepEqual(password.plan.payload, { invalidateOtherSessions: true });
    assert.equal(JSON.stringify(store.getView()).includes("secret"), false);
    store.dispatch({
        type: "commandCompleted",
        plan: password.plan,
        data: {
            outcome: "succeeded",
            invalidation_requested: true,
            invalidated_sessions: 3,
            preserved_sessions: 1,
            current_session_preserved: true
        },
        message: "Password changed."
    });
    assert.deepEqual(store.getView().account.passwordChangeOutcome, {
        outcome: "succeeded",
        invalidationRequested: true,
        invalidatedSessions: 3,
        preservedSessions: 1,
        currentSessionPreserved: true
    });
    store.dispatch({ type: "metricsSnapshotReceived", data: {
        enabled: true,
        lifecycle_state: "stale",
        created_at: "2026-05-01T12:00:00Z",
        rotated_at: "2026-05-15T12:00:00Z",
        last_used_at: "2026-05-16T12:00:00Z",
        last_used_origin_masked: "203.0.113.x",
        stale_after_days: 30,
        token: "snapshot-token-must-not-be-revealed",
        bearer: "snapshot-bearer-must-not-be-stored",
        last_used_origin: "203.0.113.44"
    } });
    assert.deepEqual(store.getView().metrics, {
        enabled: true,
        lifecycleState: "stale",
        createdAt: "2026-05-01T12:00:00Z",
        rotatedAt: "2026-05-15T12:00:00Z",
        lastUsedAt: "2026-05-16T12:00:00Z",
        lastUsedOriginMasked: "203.0.113.x",
        neverUsed: false,
        stale: true,
        staleAfterDays: 30,
        revealedToken: ""
    });
    assert.equal(JSON.stringify(store.getView()).includes("snapshot-token"), false);
    assert.equal(JSON.stringify(store.getView()).includes("snapshot-bearer"), false);
    assert.equal(JSON.stringify(store.getView()).includes("203.0.113.44"), false);
    const rotate = effect(store.dispatch({ type: "commandRequested", command: "rotateMetricsToken" }), "executeCommand");
    store.dispatch({ type: "commandCompleted", plan: rotate.plan, data: {
        enabled: true,
        lifecycle_state: "never_used",
        created_at: "2026-05-01T12:00:00Z",
        rotated_at: "2026-06-01T12:00:00Z",
        token: "one-time-token"
    } });
    assert.equal(store.getView().metrics.revealedToken, "one-time-token");
    assert.equal(store.getView().metrics.lifecycleState, "never_used");
    assert.equal(store.getView().metrics.lastUsedAt, "");
    assert.equal(store.planCommand("copyMetricsToken").enabled, true);
    store.dispatch({ type: "metricsTokenHidden" });
    assert.equal(store.getView().metrics.revealedToken, "");
    assert.equal(store.planCommand("copyMetricsToken").enabled, false);
});

test("session administration normalizes inventory and scopes destructive commands", () => {
    const store = createStore();
    store.dispatch({
        type: "accountSnapshotReceived",
        data: {
            session_count: 2,
            password_policy: { min_length: 10, max_length: 64, requires_letter: true, requires_digit: true },
            sessions: [
                { id: "current-id", current: true, client_label: "Chrome · Windows", client_ip: "192.168.4.x", full_ip: "192.168.4.55" },
                { id: "other-id", current: false, client_label: "Firefox · Linux", client_ip: "203.0.113.x" }
            ]
        }
    });
    assert.equal(store.getView().account.sessions.length, 2);
    assert.equal(store.getView().account.sessions[0].clientLabel, "Chrome · Windows");
    assert.equal(store.getView().account.sessions[0].clientIP, "192.168.4.x");
    assert.equal(JSON.stringify(store.getView()).includes("192.168.4.55"), false);
    assert.deepEqual(store.planCommand("revokeSession", { id: "other-id" }).payload, { id: "other-id" });
    assert.equal(store.planCommand("revokeSession", { id: "" }).enabled, false);
    assert.equal(store.planCommand("clearOtherSessions").enabled, true);
});

test("session administration pins current and progressively reveals other sessions", () => {
    const store = createStore();
    store.dispatch({
        type: "accountSnapshotReceived",
        data: {
            sessions: [
                { id: "current", current: true },
                { id: "other-1" },
                { id: "other-2" },
                { id: "other-3" },
                { id: "other-4" }
            ]
        }
    });
    assert.equal(store.getView().account.currentSession.id, "current");
    assert.deepEqual(store.getView().account.otherSessions.map(item => item.id), ["other-1", "other-2", "other-3", "other-4"]);
    assert.equal(store.getView().account.hiddenOtherSessionCount, 1);
    store.dispatch({ type: "sessionListExpandedChanged", expanded: true });
    assert.equal(store.getView().account.otherSessionsExpanded, true);
    assert.equal(store.getView().account.hiddenOtherSessionCount, 0);
});

test("backup administration owns eligibility but excludes passphrases and file contents", () => {
    const store = createStore();
    store.dispatch({ type: "backupSnapshotReceived", data: { blocked: true, reason: "Maintenance active" } });
    assert.equal(store.planCommand("exportBackup").enabled, false);
    store.dispatch({ type: "backupSnapshotReceived", data: { blocked: false } });
    store.dispatch({ type: "backupFileSelected", file: { name: "safe.slubkp", size: 42, lastModified: 100, contents: "never-store" } });
    store.dispatch({ type: "backupPassphraseChanged", valid: true });
    const verify = store.planCommand("verifyBackup", { passphraseValid: true });
    assert.equal(verify.enabled, true);
    assert.deepEqual(verify.payload.file, { name: "safe.slubkp", size: 42, lastModified: 100 });
    assert.equal(store.planCommand("restoreBackup").enabled, false);
    assert.match(store.planCommand("restoreBackup").reason, /verify/i);
    const viewJSON = JSON.stringify(store.getView());
    assert.equal(viewJSON.includes("never-store"), false);
    assert.equal(JSON.stringify(verify).includes("passphrase"), false);
    assert.equal(JSON.stringify(verify).includes("binary"), false);
    const started = effect(store.dispatch({ type: "commandRequested", command: "verifyBackup", payload: { passphraseValid: true } }), "executeCommand");
    assert.equal(store.dispatch({ type: "commandRequested", command: "verifyBackup", payload: { passphraseValid: true } })[0].type, "commandRejected");

    store.dispatch({ type: "backupFileSelected", file: { name: "changed.slubkp", size: 84, lastModified: 200 } });
    store.dispatch({ type: "commandCompleted", plan: started.plan, data: { restore_ready: true, blockers: [] }, message: "Backup verified." });
    assert.equal(store.getView().backup.review, null);
    assert.equal(store.planCommand("restoreBackup").enabled, false);

    const currentVerification = effect(store.dispatch({ type: "commandRequested", command: "verifyBackup", payload: { passphraseValid: true } }), "executeCommand");
    store.dispatch({
        type: "commandCompleted",
        plan: currentVerification.plan,
        data: {
            restore_ready: true,
            compatible: true,
            blockers: [],
            warnings: [{ code: "sessions_invalidated", message: "Active sessions will be invalidated." }],
            archive: { format: "simplelinuxupdater-backup", version: 1, size_bytes: 84 },
            safe_counts: { servers: 2, policies: 1, jobs: 4, sessions: 3 }
        },
        message: "Backup verified."
    });
    assert.equal(store.getView().backup.review.restoreReady, true);
    assert.equal(store.planCommand("restoreBackup").enabled, true);

    store.dispatch({ type: "backupPassphraseChanged", valid: true });
    assert.equal(store.getView().backup.review, null);
    assert.equal(store.planCommand("restoreBackup").enabled, false);
    assert.match(store.getView().feedback.backup.message, /review expired/i);
});

test("backup recovery health normalizes lifecycle states and retains accepted evidence when refresh fails", () => {
    const states = ["healthy", "stale", "never", "failed", "unavailable"];
    for (const state of states) {
        const store = createStore();
        const requested = store.dispatch({ type: "snapshotRequested", stream: "backup" })[0];
        store.dispatch({
            type: "backupSnapshotReceived",
            requestID: requested.requestID,
            receivedAt: "2026-07-28T05:00:00Z",
            data: {
                recovery_health: {
                    state,
                    message: `Recovery is ${state}.`,
                    checked_at: "2026-07-28T05:00:00Z",
                    stale_after_hours: 168,
                    export: {
                        state,
                        last_attempt_at: "2026-07-28T04:00:00Z",
                        last_success_at: state === "never" ? "" : "2026-07-27T05:00:00Z",
                        size_bytes: state === "never" ? null : 4096,
                        message: "Export evidence."
                    },
                    verification: { state, message: "Verification evidence." },
                    schedule: { scheduled: false, message: "No backup is scheduled." },
                    retention: {
                        evidence_days: 90,
                        archive_retained_by_app: false,
                        automatic_deletion: false,
                        evidence_description: "Evidence retained for up to 90 days.",
                        archive_description: "Archives are operator-managed."
                    }
                }
            }
        });
        const accepted = store.getView().backup.recoveryHealth;
        assert.equal(accepted.state, state);
        assert.equal(accepted.staleAfterHours, 168);
        assert.equal(accepted.schedule.scheduled, false);
        assert.equal(accepted.retention.evidenceDays, 90);
        assert.equal(accepted.retention.archiveRetainedByApp, false);
        assert.equal(accepted.retention.automaticDeletion, false);

        const refresh = store.dispatch({ type: "snapshotRequested", stream: "backup" })[0];
        store.dispatch({ type: "snapshotFailed", stream: "backup", requestID: refresh.requestID, error: "Recovery evidence unavailable." });
        assert.deepEqual(store.getView().backup.recoveryHealth, accepted);
        assert.equal(store.getView().streams.backup.status, "stale");
    }
});

test("incompatible backup reviews keep restore blocked while preserving safe review facts", () => {
    const store = createStore();
    store.dispatch({ type: "backupSnapshotReceived", data: { blocked: false } });
    store.dispatch({ type: "backupFileSelected", file: { name: "future.slubkp", size: 128, lastModified: 300 } });
    store.dispatch({ type: "backupPassphraseChanged", valid: true });
    const verify = effect(store.dispatch({ type: "commandRequested", command: "verifyBackup" }), "executeCommand");
    store.dispatch({
        type: "commandCompleted",
        plan: verify.plan,
        data: {
            restore_ready: false,
            compatible: false,
            archive: { format: "simplelinuxupdater-backup", version: 99, size_bytes: 128 },
            blockers: [{ code: "unsupported_version", message: "Backup version 99 is not supported." }],
            warnings: []
        }
    });
    assert.equal(store.getView().backup.review.archive.version, 99);
    assert.equal(store.getView().backup.review.blockers[0].code, "unsupported_version");
    assert.equal(store.planCommand("restoreBackup").enabled, false);
    assert.match(store.planCommand("restoreBackup").reason, /not ready/i);
});

test("accepted destructive commands block every competing destructive Admin command", () => {
    const scheduled = {
        getView: () => ({ commands: { destructiveInFlight: false } }),
        planCommand: () => ({ enabled: true })
    };
    const store = createStore({ scheduled });
    store.dispatch({
        type: "accountSnapshotReceived",
        data: { sessions: [{ id: "current", current: true }, { id: "other" }] }
    });
    store.dispatch({ type: "backupSnapshotReceived", data: { blocked: false } });
    store.dispatch({ type: "backupFileSelected", file: { name: "backup.slubkp", size: 42 } });

    const active = effect(store.dispatch({ type: "commandRequested", command: "clearOtherSessions" }), "executeCommand");
    assert.equal(store.getView().commands.destructiveInFlight, true);
    [
        store.planCommand("clearSessions"),
        store.planCommand("revokeSession", { id: "other" }),
        store.planCommand("disableMetricsToken"),
        store.planCommand("restoreBackup", { passphraseValid: true }),
        store.planCommand("scheduled:deletePolicy", "7")
    ].forEach(plan => {
        assert.equal(plan.enabled, false);
        assert.match(plan.reason, /destructive Admin action is already in progress/i);
    });

    store.dispatch({ type: "commandCompleted", plan: active.plan });
    assert.equal(store.getView().commands.destructiveInFlight, false);
    assert.equal(store.planCommand("disableMetricsToken").enabled, true);
});

test("scheduled administration is composed without copying its semantic state", () => {
    const scheduled = {
        dispatch(event) { return [{ type: "scheduledEffect", eventType: event.type }]; },
        getView() { return { policies: [{ id: 7 }] }; },
        planCommand(command) { return { enabled: true, command }; }
    };
    const store = createStore({ scheduled });
    assert.deepEqual(store.getView().scheduled, { policies: [{ id: 7 }] });
    assert.deepEqual(store.dispatch({ type: "scheduledEvent", event: { type: "snapshotRequested" } }), [{ type: "scheduledEffect", eventType: "snapshotRequested" }]);
    assert.equal(store.planCommand("scheduled:savePolicy").command, "savePolicy");
});

test("Admin workspace projects stable sections and accepted-fact summaries", () => {
    const scheduledView = {
        snapshots: {
            policies: { data: { items: [{ id: 1 }, { id: 2 }] }, lastError: "" },
            runs: { data: { items: [{ id: 10 }] }, lastError: "" }
        }
    };
    const store = createStore({ scheduled: { getView: () => scheduledView } });

    assert.deepEqual(store.getView().workspace.sections.map(section => section.id), [
        "app-time",
        "notifications",
        "account-security",
        "recent-activity",
        "scheduled-policies",
        "scheduled-runs",
        "backup",
        "metrics"
    ]);
    assert.equal(store.getView().workspace.activeSection, "app-time");
    assert.equal(store.getView().workspace.sections[0].summary, "Timezone unavailable");

    store.dispatch({ type: "timezoneSnapshotReceived", data: { resolved_timezone: "America/Toronto" } });
    store.dispatch({ type: "notificationSnapshotReceived", data: { enabled: true } });
    store.dispatch({ type: "accountSnapshotReceived", data: { session_count: 5 } });
    store.dispatch({ type: "activitySnapshotReceived", data: { items: [{ id: 9, action: "auth.login" }], total: 1 } });
    store.dispatch({ type: "backupSnapshotReceived", data: { blocked: false } });
    store.dispatch({ type: "metricsSnapshotReceived", data: { enabled: true } });

    const summaries = Object.fromEntries(store.getView().workspace.sections.map(section => [section.id, section.summary]));
    assert.deepEqual(summaries, {
        "app-time": "America/Toronto",
        "notifications": "1 destination enabled",
        "account-security": "5 active sessions",
        "recent-activity": "1 recent event",
        "scheduled-policies": "2 saved policies",
        "scheduled-runs": "1 recent run",
        "backup": "Backup operations ready",
        "metrics": "Token usage unknown"
    });

    const refresh = effect(store.dispatch({ type: "snapshotRequested", stream: "timezone" }), "fetchSnapshot");
    store.dispatch({ type: "snapshotFailed", stream: "timezone", requestID: refresh.requestID, error: "Timezone refresh failed." });
    scheduledView.snapshots.policies.lastError = "Policy refresh failed.";
    const staleSummaries = Object.fromEntries(store.getView().workspace.sections.map(section => [section.id, section.summary]));
    assert.equal(staleSummaries["app-time"], "America/Toronto · Stale");
    assert.equal(staleSummaries["scheduled-policies"], "2 saved policies · Stale");
});

test("Recent Admin Activity is bounded, ordered, and excludes sensitive audit fields", () => {
    const store = createStore();
    const request = effect(store.dispatch({ type: "snapshotRequested", stream: "activity" }), "fetchSnapshot");
    const items = Array.from({ length: 10 }, (_, index) => ({
        id: 100 - index,
        created_at: `2026-07-28T0${index}:00:00Z`,
        created_at_display: `Application time ${index}`,
        actor: index === 0 ? "admin" : "",
        action: index === 1 ? "" : "auth.login",
        status: index === 0 ? "success" : "failure",
        message: "must not be projected",
        meta_json: "{\"password\":\"secret\"}",
        request_id: "sensitive-request",
        client_ip: "203.0.113.44",
        target_name: "sensitive-target"
    }));

    store.dispatch({
        type: "activitySnapshotReceived",
        requestID: request.requestID,
        receivedAt: "2026-07-28T12:00:00Z",
        data: { items, total: 10 }
    });

    const activity = store.getView().activity;
    assert.deepEqual(activity.items.map(item => item.id), [100, 98, 97, 96, 95, 94, 93, 92]);
    assert.equal(activity.limit, 8);
    assert.equal(activity.total, 10);
    assert.equal(activity.items[0].createdAtDisplay, "Application time 0");
    assert.equal(activity.items[1].actor, "unknown");
    const serialized = JSON.stringify(activity);
    assert.doesNotMatch(serialized, /must not be projected|secret|sensitive-request|203\\.0\\.113\\.44|sensitive-target/);

    const refresh = effect(store.dispatch({ type: "snapshotRequested", stream: "activity" }), "fetchSnapshot");
    store.dispatch({ type: "snapshotFailed", stream: "activity", requestID: refresh.requestID, error: "offline" });
    assert.equal(store.getView().streams.activity.status, "stale");
    assert.deepEqual(store.getView().activity.items.map(item => item.id), [100, 98, 97, 96, 95, 94, 93, 92]);
});

test("Admin workspace validates persisted disclosure and navigation intents", () => {
    const store = createStore();

    let effects = store.dispatch({
        type: "sectionPreferencesRestored",
        collapsedSections: ["notifications", "backup", "not-a-section", "notifications"]
    });
    assert.deepEqual(store.getView().workspace.collapsedSections, ["notifications", "backup"]);
    assert.equal(effect(effects, "render").area, "workspace");

    effects = store.dispatch({ type: "sectionNavigationRequested", sectionID: "backup" });
    assert.equal(store.getView().workspace.activeSection, "backup");
    assert.deepEqual(store.getView().workspace.collapsedSections, ["notifications"]);
    assert.equal(effect(effects, "focusSection").sectionID, "backup");
    assert.deepEqual(effect(effects, "persistSectionPreferences").collapsedSections, ["notifications"]);

    effects = store.dispatch({ type: "sectionCollapseToggled", sectionID: "account-security" });
    assert.deepEqual(store.getView().workspace.collapsedSections, ["notifications", "account-security"]);
    assert.deepEqual(effect(effects, "persistSectionPreferences").collapsedSections, ["notifications", "account-security"]);

    const before = store.getView().workspace;
    assert.deepEqual(store.dispatch({ type: "sectionNavigationRequested", sectionID: "not-a-section" }), []);
    assert.deepEqual(store.getView().workspace, before);

    effects = store.dispatch({ type: "sectionActivated", sectionID: "scheduled-policies" });
    assert.equal(store.getView().workspace.activeSection, "scheduled-policies");
    assert.equal(effect(effects, "render").area, "workspace");
    assert.equal(effect(effects, "focusSection"), undefined);
});

test("Admin section data activates once, retries explicitly, and skips collapsed visibility", () => {
    const store = createStore();

    let effects = store.dispatch({
        type: "sectionPreferencesRestored",
        collapsedSections: ["notifications"]
    });
    assert.equal(effect(effects, "loadSectionData"), undefined);

    effects = store.dispatch({ type: "sectionActivated", sectionID: "notifications" });
    assert.equal(effect(effects, "loadSectionData"), undefined);

    effects = store.dispatch({ type: "sectionCollapseToggled", sectionID: "notifications" });
    assert.equal(effect(effects, "loadSectionData").sectionID, "notifications");
    assert.equal(effect(effects, "loadSectionData").reason, "first-activation");

    effects = store.dispatch({ type: "sectionActivated", sectionID: "notifications" });
    assert.equal(effect(effects, "loadSectionData"), undefined);

    effects = store.dispatch({ type: "sectionRetryRequested", sectionID: "notifications" });
    assert.equal(effect(effects, "loadSectionData").sectionID, "notifications");
    assert.equal(effect(effects, "loadSectionData").reason, "retry");

    effects = store.dispatch({ type: "sectionNavigationRequested", sectionID: "metrics" });
    assert.equal(effect(effects, "loadSectionData").sectionID, "metrics");
});

test("Admin streams expose lifecycle timestamps, preserve stale data, and reject late snapshots", () => {
    const store = createStore();
    assert.deepEqual(
        {
            status: store.getView().streams.metrics.status,
            lastSuccessfulRefresh: store.getView().streams.metrics.lastSuccessfulRefresh
        },
        { status: "unavailable", lastSuccessfulRefresh: "" }
    );

    const first = effect(store.dispatch({ type: "snapshotRequested", stream: "metrics" }), "fetchSnapshot");
    assert.equal(store.getView().streams.metrics.status, "loading");
    store.dispatch({
        type: "metricsSnapshotReceived",
        requestID: first.requestID,
        receivedAt: "2026-07-28T03:30:00Z",
        data: { enabled: true }
    });
    assert.equal(store.getView().streams.metrics.status, "current");
    assert.equal(store.getView().streams.metrics.lastSuccessfulRefresh, "2026-07-28T03:30:00Z");

    const second = effect(store.dispatch({ type: "snapshotRequested", stream: "metrics" }), "fetchSnapshot");
    store.dispatch({ type: "snapshotFailed", stream: "metrics", requestID: second.requestID, error: "offline" });
    assert.equal(store.getView().streams.metrics.status, "stale");
    assert.equal(store.getView().metrics.enabled, true);

    store.dispatch({
        type: "metricsSnapshotReceived",
        requestID: first.requestID,
        receivedAt: "2026-07-28T03:31:00Z",
        data: { enabled: false }
    });
    assert.equal(store.getView().metrics.enabled, true);
    assert.equal(store.getView().streams.metrics.lastSuccessfulRefresh, "2026-07-28T03:30:00Z");

    const notifications = effect(store.dispatch({ type: "snapshotRequested", stream: "notifications" }), "fetchSnapshot");
    store.dispatch({ type: "snapshotFailed", stream: "notifications", requestID: notifications.requestID, error: "offline" });
    assert.equal(store.getView().streams.notifications.status, "failed");
});

test("Admin adapter does not own accepted interaction state globals", () => {
    const source = fs.readFileSync(path.join(__dirname, "../../static/js/admin.js"), "utf8");
    assert.doesNotMatch(source, /let\s+appTimezoneSelection\s*=/);
    assert.doesNotMatch(source, /let\s+adminNotificationSettings\s*=/);
    assert.doesNotMatch(source, /let\s+adminMetricsToken\s*=/);
    assert.doesNotMatch(source, /let\s+adminBackupState\s*=/);
});
