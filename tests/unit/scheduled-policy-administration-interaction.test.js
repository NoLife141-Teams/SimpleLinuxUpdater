const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

const { createStore } = require("../../static/js/scheduled-policy-administration-interaction.js");

function readyDraft(patch = {}) {
    return {
        name: "Nightly security", target_tag: "prod", cadence_kind: "weekly", weekdays: ["wed", "mon", "Monday"],
        time_local: "02:00", ...patch
    };
}

test("editor normalizes weekdays and projects a deterministic schedule summary", () => {
    const store = createStore();
    store.dispatch({ type: "editorChanged", patch: readyDraft() });
    const view = store.getView();
    assert.deepEqual(view.editor.draft.weekdays, ["mon", "wed"]);
    assert.match(view.editor.summary.body, /Every Mon, Wed at 02:00/);
});

test("policy and global-setting drafts expose normalized dirty, validation, discard, and save eligibility", () => {
    const store = createStore();
    assert.equal(store.getView().editor.dirty, false);
    assert.equal(store.getView().editor.valid, false);
    assert.equal(store.planCommand("savePolicy").enabled, false);

    store.dispatch({ type: "editorChanged", patch: readyDraft() });
    assert.equal(store.getView().editor.dirty, true);
    assert.equal(store.getView().editor.valid, true);
    assert.equal(store.planCommand("savePolicy").enabled, true);

    store.dispatch({ type: "editorDiscardRequested" });
    assert.equal(store.getView().editor.dirty, false);
    assert.equal(store.getView().editor.draft.name, "");

    store.dispatch({ type: "blackoutRowsReceived", kind: "global", rows: [
        { weekdays: ["fri"], start_time: "03:00", end_time: "04:00" },
    ] });
    assert.equal(store.getView().globalSettings.dirty, false);
    assert.equal(store.planCommand("saveGlobalSettings").enabled, false);

    store.dispatch({ type: "blackoutRowChanged", kind: "global", index: 0, field: "end_time", value: "05:00" });
    assert.equal(store.getView().globalSettings.dirty, true);
    assert.equal(store.getView().globalSettings.valid, true);
    assert.equal(store.planCommand("saveGlobalSettings").enabled, true);
    store.dispatch({ type: "blackoutRowsReceived", kind: "global", rows: [
        { weekdays: ["fri"], start_time: "03:00", end_time: "06:00" },
    ] });
    assert.equal(store.getView().editor.globalBlackouts[0].end_time, "05:00");
    assert.equal(store.getView().globalSettings.dirty, true);
    store.dispatch({ type: "globalSettingsDiscardRequested" });
    assert.equal(store.getView().editor.globalBlackouts[0].end_time, "06:00");
    assert.equal(store.getView().globalSettings.dirty, false);
});

test("policy context replacement requires confirmation only for meaningful changes", () => {
    const store = createStore();
    const policy = readyDraft({ id: "7", name: "Accepted policy" });
    let effects = store.dispatch({ type: "editorReplacementRequested", replacement: { type: "load", policy } });
    assert.equal(effects[0].type, "render");
    assert.equal(store.getView().editor.draft.id, "7");

    store.dispatch({ type: "editorChanged", patch: { name: "Unsaved name" } });
    effects = store.dispatch({ type: "editorReplacementRequested", replacement: { type: "reset" } });
    assert.equal(effects[0].type, "confirmEditorReplacement");
    assert.equal(store.getView().editor.draft.name, "Unsaved name");

    store.dispatch({ type: "editorReplacementConfirmed", replacement: effects[0].replacement });
    assert.equal(store.getView().editor.draft.name, "");
    assert.equal(store.getView().editor.dirty, false);
});

test("preview intent validates the draft and stale responses cannot overwrite the newest preview", () => {
    const store = createStore();
    store.dispatch({ type: "editorChanged", patch: readyDraft() });
    const first = store.dispatch({ type: "previewRequested" }).find(effect => effect.type === "fetchPreview");
    store.dispatch({ type: "editorChanged", patch: { name: "Later policy" } });
    const second = store.dispatch({ type: "previewRequested" }).find(effect => effect.type === "fetchPreview");
    store.dispatch({ type: "previewReceived", requestId: first.requestId, preview: { matched_servers: [{ name: "old" }] } });
    store.dispatch({ type: "previewReceived", requestId: second.requestId, preview: { matched_servers: [{ name: "new" }] } });
    assert.deepEqual(store.getView().editor.preview.data.matched_servers.map(server => server.name), ["new"]);
});

test("blackout JSON failures preserve the last accepted policy and global rows", () => {
    const store = createStore();
    store.dispatch({ type: "blackoutRowsReceived", kind: "policy", rows: [{ weekdays: ["monday"], start_time: "01:00", end_time: "02:00" }] });
    store.dispatch({ type: "blackoutWeekdayToggled", kind: "policy", index: 0, day: "wed" });
    store.dispatch({ type: "blackoutRowChanged", kind: "policy", index: 0, field: "end_time", value: "03:00" });
    const effects = store.dispatch({ type: "blackoutJSONApplied", kind: "policy", label: "Policy no-run windows", raw: "[{]" });
    assert.equal(effects[0].type, "blackoutJSONRejected");
    assert.equal(store.getView().editor.policyBlackouts[0].start_time, "01:00");
    assert.deepEqual(store.getView().editor.policyBlackouts[0].weekdays, ["mon", "wed"]);
    assert.equal(store.getView().editor.policyBlackouts[0].end_time, "03:00");
    store.dispatch({ type: "blackoutRowsReceived", kind: "global", rows: [{ weekdays: ["fri"], start_time: "03:00", end_time: "04:00" }] });
    assert.equal(store.getView().editor.globalBlackouts[0].weekdays[0], "fri");
});

test("snapshot streams order independently, retain data on failure, and keep calendar selection logical", () => {
    const store = createStore();
    const policies = store.dispatch({ type: "snapshotRequested", stream: "policies" }).find(effect => effect.type === "fetchSnapshot");
    const runs = store.dispatch({ type: "snapshotRequested", stream: "runs" }).find(effect => effect.type === "fetchSnapshot");
    store.dispatch({ type: "snapshotReceived", stream: "policies", requestId: policies.requestId, data: { items: [{ id: 7, name: "Nightly" }] } });
    store.dispatch({ type: "snapshotReceived", stream: "runs", requestId: runs.requestId, data: { items: [{ id: 1, job_id: "job-1" }] } });
    const nextPolicies = store.dispatch({ type: "snapshotRequested", stream: "policies" }).find(effect => effect.type === "fetchSnapshot");
    store.dispatch({ type: "snapshotFailed", stream: "policies", requestId: nextPolicies.requestId, error: "offline" });
    store.dispatch({ type: "calendarPolicySelected", policyID: "7" });
    assert.equal(store.getView().policies[0].name, "Nightly");
    assert.equal(store.getView().snapshots.policies.lastError, "offline");
    assert.equal(store.getView().selectedCalendarPolicyID, "7");
});

test("selected job detail is logical module state and closes without touching adapter concerns", () => {
    const store = createStore();
    const first = store.dispatch({ type: "jobSelected", jobID: "job-7" }).find(effect => effect.type === "fetchSnapshot");
    store.dispatch({ type: "jobSelected", jobID: "job-8" });
    const completion = store.dispatch({ type: "jobReceived", requestId: first.requestId, job: { id: "job-7", status: "done" } });
    const second = completion.find(effect => effect.type === "fetchSnapshot");
    store.dispatch({ type: "jobReceived", requestId: second.requestId, job: { id: "job-8", status: "running" } });
    store.dispatch({ type: "jobReceived", requestId: first.requestId, job: { id: "job-7", status: "done" } });
    assert.equal(store.getView().selectedJob.id, "job-8");
    store.dispatch({ type: "jobClosed" });
    assert.equal(store.getView().selectedJob, null);
});

test("command planning prevents competing policy and global-settings mutations", () => {
    const store = createStore();
    store.dispatch({ type: "editorChanged", patch: readyDraft({ id: "7" }) });
    const first = store.dispatch({ type: "commandRequested", command: "savePolicy" }).find(effect => effect.type === "executeCommand");
    const duplicate = store.dispatch({ type: "commandRequested", command: "savePolicy" });
    assert.equal(first.plan.command, "updatePolicy");
    assert.equal(duplicate[0].type, "commandRejected");
    store.dispatch({ type: "commandCompleted", plan: first.plan, message: "Policy updated." });
    store.dispatch({ type: "blackoutRowAdded", kind: "global" });
    store.dispatch({ type: "blackoutWeekdayToggled", kind: "global", index: 0, day: "sun" });
    const global = store.dispatch({ type: "commandRequested", command: "saveGlobalSettings" }).find(effect => effect.type === "executeCommand");
    assert.equal(store.dispatch({ type: "commandRequested", command: "saveGlobalSettings" })[0].type, "commandRejected");
    store.dispatch({ type: "commandFailed", plan: global.plan, message: "offline" });
    assert.equal(store.getView().commands.globalSettingsInFlight, false);
});

test("a new policy draft cannot start a competing create command", () => {
    const store = createStore();
    store.dispatch({ type: "editorChanged", patch: readyDraft() });
    const first = store.dispatch({ type: "commandRequested", command: "savePolicy" }).find(effect => effect.type === "executeCommand");
    assert.equal(first.plan.command, "createPolicy");
    assert.equal(store.dispatch({ type: "commandRequested", command: "savePolicy" })[0].type, "commandRejected");
    store.dispatch({ type: "commandCompleted", plan: first.plan });
    assert.equal(store.getView().commands.inFlightPolicyIDs.length, 0);
});

test("Admin no longer declares the legacy policy editor state globals", () => {
    const admin = fs.readFileSync(path.join(__dirname, "../../static/js/admin.js"), "utf8");
    assert.doesNotMatch(admin, /const\\s+scheduledPoliciesState\\s*=/);
    assert.doesNotMatch(admin, /const\\s+blackoutEditors\\s*=/);
    assert.doesNotMatch(admin, /const\\s+policyFormState\\s*=/);
    assert.doesNotMatch(admin, /policyPreviewRequestSeq/);
});
