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
    assert.equal(Object.hasOwn(first.payload, "id"), false);
    store.dispatch({ type: "editorChanged", patch: { name: "Later policy" } });
    const second = store.dispatch({ type: "previewRequested" }).find(effect => effect.type === "fetchPreview");
    store.dispatch({ type: "previewReceived", requestId: first.requestId, preview: { matched_servers: [{ name: "old" }] } });
    store.dispatch({ type: "previewReceived", requestId: second.requestId, preview: { matched_servers: [{ name: "new" }] } });
    assert.deepEqual(store.getView().editor.preview.data.matched_servers.map(server => server.name), ["new"]);
});

test("existing-policy preview sends its identifier as an API integer", () => {
    const store = createStore();
    store.dispatch({ type: "editorLoaded", policy: readyDraft({ id: 7 }) });
    const request = store.dispatch({ type: "previewRequested" }).find(effect => effect.type === "fetchPreview");
    assert.equal(request.payload.id, 7);
    assert.equal(typeof request.payload.id, "number");
});

test("preview accepts canonical occurrence facts and invalidates them when the application timezone changes", () => {
    const store = createStore();
    store.dispatch({ type: "editorChanged", patch: readyDraft() });
    const request = store.dispatch({ type: "previewRequested" }).find(effect => effect.type === "fetchPreview");
    const preview = {
        matched_servers: [{ name: "srv-prod", tags: ["prod"] }],
        upcoming_occurrences: [{
            local_civil_time: "2026-11-01 01:30",
            timezone: "America/Toronto",
            offset: "-04:00",
            abbreviation: "EDT",
            scheduled_for_utc: "2026-11-01T05:30:00.000000000Z",
            dst_status: "ambiguous",
            canonical_choice: "earlier_fallback_occurrence",
            matched_server_count: 1,
            applicable_no_run_windows: [],
            admission_outcome: "admitted",
        }],
        validation_errors: [],
        operational_warnings: [],
        informational_facts: [{ code: "dst_fallback_canonical_choice", message: "The scheduler uses the earlier occurrence." }],
    };
    store.dispatch({ type: "previewReceived", requestId: request.requestId, preview });

    const accepted = store.getView().editor.preview.data;
    assert.deepEqual(accepted.upcoming_occurrences, preview.upcoming_occurrences);
    assert.deepEqual(accepted.informational_facts, preview.informational_facts);
    preview.upcoming_occurrences[0].timezone = "mutated";
    assert.equal(store.getView().editor.preview.data.upcoming_occurrences[0].timezone, "America/Toronto");

    store.dispatch({ type: "timezoneReceived", timezone: "+05:30" });
    assert.equal(store.getView().editor.preview.data, null);
    assert.match(store.getView().editor.preview.message, /timezone changed/i);
});

test("advisory schedule conflicts remain accepted preview facts without blocking policy save", () => {
    const store = createStore();
    store.dispatch({ type: "editorChanged", patch: readyDraft() });
    const request = store.dispatch({ type: "previewRequested" }).find(effect => effect.type === "fetchPreview");
    const conflict = {
        policy_id: 7,
        policy_name: "Competing policy",
        overlap_kind: "partial",
        shared_servers: ["srv-prod"],
        occurrence_windows: [{
            local_civil_time: "2026-05-18 02:00",
            timezone: "America/Toronto",
            window_start_utc: "2026-05-18T06:00:00.000000000Z",
            window_end_utc: "2026-05-18T06:01:00.000000000Z",
            draft_admission_outcome: "admitted",
            competing_admission_outcome: "admitted",
            effective: true,
        }],
    };
    store.dispatch({
        type: "previewReceived",
        requestId: request.requestId,
        preview: {
            matched_servers: [{ name: "srv-prod" }],
            schedule_conflicts: [conflict],
            validation_errors: [],
            operational_warnings: [{
                code: "policy_schedule_overlap",
                message: "One or more enabled policies target shared servers during the same projected occurrence.",
            }],
            informational_facts: [],
            upcoming_occurrences: [],
        },
    });

    assert.deepEqual(store.getView().editor.preview.data.schedule_conflicts, [conflict]);
    assert.equal(store.getView().editor.valid, true);
    assert.equal(store.planCommand("savePolicy").enabled, true);
    conflict.shared_servers.push("mutated");
    assert.deepEqual(store.getView().editor.preview.data.schedule_conflicts[0].shared_servers, ["srv-prod"]);
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

test("scheduled snapshots expose freshness, retain accepted data, and reject superseded responses", () => {
    const store = createStore();
    assert.equal(store.getView().snapshots.runs.status, "unavailable");

    const first = store.dispatch({ type: "snapshotRequested", stream: "runs" }).find(effect => effect.type === "fetchSnapshot");
    assert.equal(store.getView().snapshots.runs.status, "loading");
    store.dispatch({
        type: "snapshotReceived",
        stream: "runs",
        requestId: first.requestId,
        receivedAt: "2026-07-28T03:30:00Z",
        data: { items: [{ id: 1 }] }
    });
    assert.equal(store.getView().snapshots.runs.status, "current");
    assert.equal(store.getView().snapshots.runs.lastSuccessfulRefresh, "2026-07-28T03:30:00Z");

    const second = store.dispatch({ type: "snapshotRequested", stream: "runs" }).find(effect => effect.type === "fetchSnapshot");
    store.dispatch({ type: "snapshotFailed", stream: "runs", requestId: second.requestId, error: "offline" });
    assert.equal(store.getView().snapshots.runs.status, "stale");
    assert.deepEqual(store.getView().runs.map(run => run.id), [1]);

    store.dispatch({
        type: "snapshotReceived",
        stream: "runs",
        requestId: first.requestId,
        receivedAt: "2026-07-28T03:31:00Z",
        data: { items: [{ id: 99 }] }
    });
    assert.deepEqual(store.getView().runs.map(run => run.id), [1]);
    assert.equal(store.getView().snapshots.runs.lastSuccessfulRefresh, "2026-07-28T03:30:00Z");

    const calendar = store.dispatch({ type: "snapshotRequested", stream: "calendar" }).find(effect => effect.type === "fetchSnapshot");
    store.dispatch({ type: "snapshotFailed", stream: "calendar", requestId: calendar.requestId, error: "offline" });
    assert.equal(store.getView().snapshots.calendar.status, "failed");
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
    const refreshes = store.dispatch({ type: "commandCompleted", plan: first.plan, message: "Policy updated." });
    assert.deepEqual(
        refreshes.filter(effect => effect.type === "fetchSnapshot").map(effect => effect.stream),
        ["policies", "settings", "calendar"]
    );
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

test("one accepted policy deletion blocks every competing deletion until settlement", () => {
    const store = createStore();
    const request = store.dispatch({ type: "snapshotRequested", stream: "policies" }).find(effect => effect.type === "fetchSnapshot");
    store.dispatch({
        type: "snapshotReceived",
        stream: "policies",
        requestId: request.requestId,
        data: { items: [{ id: 7, name: "One" }, { id: 8, name: "Two" }] }
    });

    const first = store.dispatch({ type: "commandRequested", command: "deletePolicy", policyID: "7" }).find(effect => effect.type === "executeCommand");
    assert.equal(store.getView().commands.destructiveInFlight, true);
    assert.equal(store.planCommand("deletePolicy", "8").enabled, false);
    assert.match(store.planCommand("deletePolicy", "8").reason, /destructive Admin action is already in progress/i);

    store.dispatch({ type: "commandFailed", plan: first.plan, message: "offline" });
    assert.equal(store.getView().commands.destructiveInFlight, false);
    assert.equal(store.planCommand("deletePolicy", "8").enabled, true);
});

test("Admin no longer declares the legacy policy editor state globals", () => {
    const admin = fs.readFileSync(path.join(__dirname, "../../static/js/admin.js"), "utf8");
    assert.doesNotMatch(admin, /const\\s+scheduledPoliciesState\\s*=/);
    assert.doesNotMatch(admin, /const\\s+blackoutEditors\\s*=/);
    assert.doesNotMatch(admin, /const\\s+policyFormState\\s*=/);
    assert.doesNotMatch(admin, /policyPreviewRequestSeq/);
});
