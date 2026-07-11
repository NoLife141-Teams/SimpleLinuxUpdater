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

test("backup administration owns eligibility but excludes passphrases and file contents", () => {
    const store = createStore();
    store.dispatch({ type: "backupSnapshotReceived", data: { blocked: true, reason: "Maintenance active" } });
    assert.equal(store.planCommand("exportBackup").enabled, false);
    store.dispatch({ type: "backupSnapshotReceived", data: { blocked: false } });
    store.dispatch({ type: "backupFileSelected", file: { name: "safe.slubkp", size: 42, contents: "never-store" } });
    const verify = store.planCommand("verifyBackup", { passphraseValid: true });
    assert.equal(verify.enabled, true);
    assert.deepEqual(verify.payload.file, { name: "safe.slubkp", size: 42 });
    const viewJSON = JSON.stringify(store.getView());
    assert.equal(viewJSON.includes("never-store"), false);
    assert.equal(JSON.stringify(verify).includes("passphrase"), false);
    assert.equal(JSON.stringify(verify).includes("binary"), false);
    const started = effect(store.dispatch({ type: "commandRequested", command: "verifyBackup", payload: { passphraseValid: true } }), "executeCommand");
    assert.equal(store.dispatch({ type: "commandRequested", command: "verifyBackup", payload: { passphraseValid: true } })[0].type, "commandRejected");
    store.dispatch({ type: "commandCompleted", plan: started.plan, message: "Backup verified." });
    assert.equal(store.getView().feedback.backup.message, "Backup verified.");
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

test("Admin adapter does not own accepted interaction state globals", () => {
    const source = fs.readFileSync(path.join(__dirname, "../../static/js/admin.js"), "utf8");
    assert.doesNotMatch(source, /let\s+appTimezoneSelection\s*=/);
    assert.doesNotMatch(source, /let\s+adminNotificationSettings\s*=/);
    assert.doesNotMatch(source, /let\s+adminMetricsToken\s*=/);
    assert.doesNotMatch(source, /let\s+adminBackupState\s*=/);
});
