const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

function workflow() {
    const elements = new Map(), requests = [], listeners = new Map(), evidenceRefreshes = [];
    let poll;
    const document = {hidden: false, getElementById(id) {
        if (!elements.has(id)) elements.set(id, {id, value: "", checked: false, innerHTML: "", querySelectorAll: () => []});
        return elements.get(id);
    }};
    const life = {init() {}, listen(target, type, fn) {listeners.set(`${target.id}:${type}`, fn);}, clearInterval() {}, destroy() {},
        setInterval(fn) {poll = fn; return 1;},
        fetch(url) {return new Promise(resolve => requests.push({url, reply(data, status = 200) {
            resolve({ok: status === 200, status, json: async () => data});
        }}));}
    };
    const root = {RemediationPage: {refreshEvidence: server => evidenceRefreshes.push(server)}, BrowserEffects: {create: () => life}, canUseScope: () => true};
    vm.runInNewContext(fs.readFileSync(path.join(__dirname, "../../static/js/remediation-workflow.js"), "utf8"), {window: root, document});
    root.RemediationWorkflow.select("example-host");
    return {requests, evidenceRefreshes, poll: () => poll(), html: () => document.getElementById("remediation-operation").innerHTML,
        error: () => document.getElementById("remediation-error").textContent,
        errorHidden: () => document.getElementById("remediation-error").hidden,
        click: id => listeners.get("remediation-operation:click")({target: {id}}),
        change(id, properties) {const target = document.getElementById(id); Object.assign(target, properties); listeners.get("remediation-operation:change")({target});},
        scope: () => document.getElementById("remediation-scope").value,
        removals: () => document.getElementById("remediation-removals").checked};
}
const flush = () => new Promise(resolve => setImmediate(resolve));
const prepared = {id: "plan", state: "prepared", job_id: "job", update_status: "waiting_approval"};
const verified = {...prepared, state: "verified", scan_id: "after", update_status: "succeeded"};
const history = plan => ({plans: [plan], reviews: []});
const detail = plan => ({plan, page: 1, pages: 1});

test("delayed prepared detail cannot overwrite a newer verification result", async () => {
    const w = workflow();
    w.requests[0].reply(history(prepared)); await flush();
    assert.equal(w.requests.length, 2);
    w.poll(); w.requests[2].reply(history(verified)); await flush();
    w.requests[3].reply(detail(verified)); await flush();
    assert.match(w.html(), /Operation: verified/);
    w.requests[1].reply(detail(prepared)); await flush();
    assert.match(w.html(), /Operation: verified/);
});

test("delayed history cannot roll verification back to an older plan state", async () => {
    const w = workflow();
    w.poll(); w.requests[1].reply(history(verified)); await flush();
    w.requests[2].reply(detail(verified)); await flush();
    w.requests[0].reply(history(prepared)); await flush();
    assert.equal(w.requests.length, 3);
    assert.match(w.html(), /Operation: verified/);
});

test("failed operation detail is retried when history stays unchanged", async () => {
    const w = workflow();
    w.requests[0].reply(history(prepared)); await flush();
    w.requests[1].reply(detail(prepared)); await flush();
    w.poll(); w.requests[2].reply(history(verified)); await flush();
    w.requests[3].reply({error: "Synthetic detail failure"}, 503); await flush();
    assert.equal(w.error(), "Synthetic detail failure");
    w.poll(); w.requests[4].reply(history(verified)); await flush();
    assert.equal(w.requests.length, 6);
    w.requests[5].reply(detail(verified)); await flush();
    assert.match(w.html(), /Operation: verified/);
});

test("polling refreshes a changed verification admission reason without a state change", async () => {
    const w = workflow();
    const queued = {...prepared, state: "queued", message: "Waiting for verification"};
    w.requests[0].reply(history(queued)); await flush();
    w.requests[1].reply(detail(queued)); await flush();
    const blocked = {...queued, message: "Server is disabled; verification remains queued"};
    w.poll(); w.requests[2].reply(history(blocked)); await flush();
    assert.equal(w.requests.length, 4, "changed admission facts must load new operation detail");
    w.requests[3].reply(detail(blocked)); await flush();
    assert.match(w.html(), /Server is disabled; verification remains queued/);
});

test("successful polling clears a recovered operation read error", async () => {
    const w = workflow();
    w.requests[0].reply(history(prepared)); await flush();
    w.requests[1].reply(detail(prepared)); await flush();
    w.poll(); w.requests[2].reply({error: "Temporary history failure"}, 503); await flush();
    assert.equal(w.errorHidden(), false);
    w.poll(); w.requests[3].reply(history(prepared)); await flush();
    assert.equal(w.errorHidden(), true, "successful unchanged history clears the obsolete read error");
});

test("successful background reads preserve a rejected verification command", async () => {
    const w = workflow();
    const failed = {...prepared, state: "verification_failed"};
    w.requests[0].reply(history(failed)); await flush();
    w.requests[1].reply(detail(failed)); await flush();
    w.click("remediation-retry");
    w.requests[2].reply({error: "Verification changed; refresh"}, 409); await flush();
    w.poll(); w.requests[3].reply({error: "Temporary history failure"}, 503); await flush();
    w.poll(); w.requests[4].reply(history(failed)); await flush();
    assert.equal(w.errorHidden(), false);
    assert.equal(w.error(), "Verification changed; refresh");
});

test("approval drafts survive operation reads but never carry consent to a changed plan identity", async () => {
    for (const changed of [{id: "replacement"}, {digest: "new-digest"}, {approval_generation: 2}]) {
        const w = workflow();
        const transaction = {status: {upgrade_plan: {package_deferrals: {}}, pending_updates: []},
            scopes: [{scope: "all", available: true}, {scope: "full_upgrade", available: true}]};
        const plan = {...prepared, digest: "old-digest", approval_generation: 1, transaction};
        w.requests[0].reply(history(plan)); await flush();
        w.requests[1].reply(detail(plan)); await flush();
        w.change("remediation-scope", {value: "full_upgrade"});
        w.change("remediation-removals", {checked: true});
        const updated = {...plan, message: "Updated operation facts"};
        w.poll(); w.requests[2].reply(history(updated)); await flush();
        w.requests[3].reply(detail(updated)); await flush();
        assert.equal(w.scope(), "full_upgrade");
        assert.equal(w.removals(), true);
        const replacement = {...updated, ...changed};
        w.poll(); w.requests[4].reply(history(replacement)); await flush();
        w.requests[5].reply(detail(replacement)); await flush();
        assert.equal(w.scope(), "all");
        assert.equal(w.removals(), false);
    }
});

test("completed independent verification refreshes recorded plan evidence only once", async () => {
    const w = workflow();
    w.requests[0].reply(history(prepared)); await flush();
    w.requests[1].reply(detail(prepared)); await flush();
    assert.deepEqual(w.evidenceRefreshes, []);
    w.poll(); w.requests[2].reply(history(verified)); await flush();
    w.requests[3].reply(detail(verified)); await flush();
    assert.deepEqual(w.evidenceRefreshes, ["example-host"]);
    w.poll(); w.requests[4].reply(history({...verified, message: "Same verification, new message"})); await flush();
    w.requests[5].reply(detail({...verified, message: "Same verification, new message"})); await flush();
    assert.deepEqual(w.evidenceRefreshes, ["example-host"]);
});
