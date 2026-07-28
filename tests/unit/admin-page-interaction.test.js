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
    store.dispatch({ type: "notificationSnapshotReceived", data: { enabled: true, webhook_url: "https://new.example.test" } });
    const active = effect(store.dispatch({ type: "snapshotRequested", stream: "notifications" }), "fetchSnapshot");

    store.dispatch({ type: "notificationSnapshotReceived", data: { enabled: false, webhook_url: "https://stale.example.test" } });

    assert.equal(store.getView().notifications.enabled, true);
    assert.equal(store.getView().notifications.webhookURL, "https://new.example.test");
    assert.equal(store.getView().streams.notifications.freshness, "refreshing");
    store.dispatch({ type: "notificationSnapshotReceived", requestID: active.requestID, data: { enabled: false, webhook_url: "https://accepted.example.test" } });
    assert.equal(store.getView().notifications.webhookURL, "https://accepted.example.test");
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
    store.dispatch({ type: "notificationSnapshotReceived", data: { enabled: true, webhook_url: " https://hooks.example.test/x ", event_types: ["update.complete", "update.complete"], last_delivery: { success: true, delivered_at: "2026-07-10T12:00:00Z" } } });
    assert.deepEqual(store.getView().notifications.eventTypes, ["update.complete"]);
    store.dispatch({ type: "notificationDraftChanged", patch: { enabled: false, webhookURL: "https://hooks.example.test/y" } });
    const save = effect(store.dispatch({ type: "commandRequested", command: "saveNotifications" }), "executeCommand");
    assert.deepEqual(save.plan.payload, { enabled: false, webhook_url: "https://hooks.example.test/y", event_types: ["update.complete"] });
    store.dispatch({ type: "commandCompleted", plan: save.plan, data: { enabled: false, webhook_url: "https://hooks.example.test/y", event_types: ["update.complete"] } });
    const delivery = effect(store.dispatch({ type: "commandRequested", command: "testNotification" }), "executeCommand");
    store.dispatch({ type: "commandFailed", plan: delivery.plan, data: { last_delivery: { success: false, attempts: 3 } }, message: "Notification test failed." });
    assert.equal(store.getView().notifications.lastDelivery.attempts, 3);
    assert.equal(store.getView().feedback.notifications.error, true);
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
        webhook_url: "https://hooks.example.test/current",
        event_types: ["update.complete"],
    } });

    assert.equal(store.getView().notifications.dirty, false);
    assert.equal(store.planCommand("saveNotifications").enabled, false);

    store.dispatch({ type: "notificationDraftChanged", patch: {
        webhookURL: " https://hooks.example.test/replacement ",
        eventTypes: ["update.complete", "update.complete"],
    } });
    assert.equal(store.getView().notifications.dirty, true);
    assert.equal(store.planCommand("saveNotifications").enabled, true);

    const notificationRefresh = effect(store.dispatch({ type: "snapshotRequested", stream: "notifications" }), "fetchSnapshot");
    store.dispatch({ type: "notificationSnapshotReceived", requestID: notificationRefresh.requestID, data: {
        enabled: true,
        webhook_url: "https://hooks.example.test/server-updated",
        event_types: ["update.complete"],
    } });
    assert.equal(store.getView().notifications.webhookURL, "https://hooks.example.test/replacement");
    assert.equal(store.getView().notifications.dirty, true);

    store.dispatch({ type: "notificationDiscardRequested" });
    assert.equal(store.getView().notifications.webhookURL, "https://hooks.example.test/server-updated");
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
    store.dispatch({ type: "accountSnapshotReceived", data: { count: 4 } });
    const password = effect(store.dispatch({ type: "commandRequested", command: "changePassword", payload: { hasCurrentPassword: true, hasNewPassword: true, passwordsMatch: true } }), "executeCommand");
    assert.deepEqual(password.plan.payload, {});
    assert.equal(JSON.stringify(store.getView()).includes("secret"), false);
    store.dispatch({ type: "commandCompleted", plan: password.plan, message: "Password changed." });
    store.dispatch({ type: "metricsSnapshotReceived", data: { enabled: false } });
    const rotate = effect(store.dispatch({ type: "commandRequested", command: "rotateMetricsToken" }), "executeCommand");
    store.dispatch({ type: "commandCompleted", plan: rotate.plan, data: { enabled: true, token: "one-time-token" } });
    assert.equal(store.getView().metrics.revealedToken, "one-time-token");
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
            sessions: [
                { id: "current-id", current: true, client_label: "Chrome · Windows", client_ip: "192.168.4.x" },
                { id: "other-id", current: false, client_label: "Firefox · Linux", client_ip: "203.0.113.x" }
            ]
        }
    });
    assert.equal(store.getView().account.sessions.length, 2);
    assert.equal(store.getView().account.sessions[0].clientLabel, "Chrome · Windows");
    assert.equal(store.getView().account.sessions[0].clientIP, "192.168.4.x");
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
    store.dispatch({ type: "backupSnapshotReceived", data: { blocked: false } });
    store.dispatch({ type: "metricsSnapshotReceived", data: { enabled: true } });

    const summaries = Object.fromEntries(store.getView().workspace.sections.map(section => [section.id, section.summary]));
    assert.deepEqual(summaries, {
        "app-time": "America/Toronto",
        "notifications": "Webhook enabled",
        "account-security": "5 active sessions",
        "scheduled-policies": "2 saved policies",
        "scheduled-runs": "1 recent run",
        "backup": "Backup operations ready",
        "metrics": "Token enabled"
    });

    const refresh = effect(store.dispatch({ type: "snapshotRequested", stream: "timezone" }), "fetchSnapshot");
    store.dispatch({ type: "snapshotFailed", stream: "timezone", requestID: refresh.requestID, error: "Timezone refresh failed." });
    scheduledView.snapshots.policies.lastError = "Policy refresh failed.";
    const staleSummaries = Object.fromEntries(store.getView().workspace.sections.map(section => [section.id, section.summary]));
    assert.equal(staleSummaries["app-time"], "America/Toronto · Stale");
    assert.equal(staleSummaries["scheduled-policies"], "2 saved policies · Stale");
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
