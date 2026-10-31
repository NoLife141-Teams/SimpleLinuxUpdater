const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

function workflow() {
    const elements = new Map(), requests = [];
    let poll;
    const document = {hidden: false, getElementById(id) {
        if (!elements.has(id)) elements.set(id, {innerHTML: "", querySelectorAll: () => []});
        return elements.get(id);
    }};
    const life = {init() {}, listen() {}, clearInterval() {}, destroy() {},
        setInterval(fn) {poll = fn; return 1;},
        fetch(url) {return new Promise(resolve => requests.push({url, reply(data, status = 200) {
            resolve({ok: status === 200, status, json: async () => data});
        }}));}
    };
    const root = {BrowserEffects: {create: () => life}, canUseScope: () => true};
    vm.runInNewContext(fs.readFileSync(path.join(__dirname, "../../static/js/remediation-workflow.js"), "utf8"), {window: root, document});
    root.RemediationWorkflow.select("example-host");
    return {requests, poll: () => poll(), html: () => document.getElementById("remediation-operation").innerHTML,
        error: () => document.getElementById("remediation-error").textContent};
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
