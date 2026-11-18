const test = require("node:test");
const assert = require("node:assert/strict");
const { create } = require("../../static/js/browser-effects");
const { createController } = require("../../static/js/status-transport");
const deferred = () => {
    let resolve;
    const promise = new Promise((r) => {
        resolve = r;
    });
    return { promise, resolve };
};
function clock() {
    let next = 0;
    const pending = new Map();
    return {
        pending,
        setTimeout: (fn) => {
            pending.set(++next, fn);
            return next;
        },
        clearTimeout: (id) => pending.delete(id),
        setInterval: (fn) => {
            pending.set(++next, fn);
            return next;
        },
        clearInterval: (id) => pending.delete(id),
        requestAnimationFrame: (fn) => {
            pending.set(++next, fn);
            return next;
        },
        cancelAnimationFrame: (id) => pending.delete(id),
    };
}

test("mounted listeners detach, can be removed during a gesture, and mount once again", () => {
    const lifecycle = create(),
        target = new EventTarget();
    let calls = 0;
    const callback = () => calls++;
    lifecycle.init();
    lifecycle.init();
    lifecycle.listen(target, "change", callback);
    target.dispatchEvent(new Event("change"));
    assert.equal(calls, 1);
    lifecycle.unlisten(target, "change", callback);
    target.dispatchEvent(new Event("change"));
    assert.equal(calls, 1);
    lifecycle.listen(target, "change", callback);
    lifecycle.destroy();
    lifecycle.destroy();
    target.dispatchEvent(new Event("change"));
    assert.equal(calls, 1);
    lifecycle.init();
    lifecycle.listen(target, "change", callback);
    target.dispatchEvent(new Event("change"));
    assert.equal(calls, 2);
    lifecycle.destroy();
});
test("destroy clears all timers and suppresses callbacks queued before remount", () => {
    const environment = clock(),
        lifecycle = create(environment);
    let calls = 0;
    lifecycle.init();
    lifecycle.setInterval(() => calls++, 10);
    lifecycle.setTimeout(() => calls++, 20);
    lifecycle.requestAnimationFrame(() => calls++);
    const queued = [...environment.pending.values()];
    lifecycle.destroy();
    assert.equal(environment.pending.size, 0);
    lifecycle.init();
    queued.forEach((fn) => fn());
    assert.equal(calls, 0);
    lifecycle.destroy();
});
test("destroy aborts owned transport and ignores a late response even after reinitialization", async () => {
    const gate = deferred();
    let signal;
    const lifecycle = create({
        fetch: (_url, options) => {
            signal = options.signal;
            return gate.promise;
        },
    });
    lifecycle.init();
    const pending = lifecycle.fetch("/synthetic");
    lifecycle.destroy();
    assert.equal(signal.aborted, true);
    lifecycle.init();
    gate.resolve(new Response("{}"));
    await assert.rejects(pending, { name: "AbortError" });
    lifecycle.destroy();
});
test("late response bodies cannot enter an accepted view after destruction", async () => {
    const gate = deferred();
    const lifecycle = create({
        fetch: async () => ({ ok: true, json: () => gate.promise }),
    });
    lifecycle.init();
    const response = await lifecycle.fetch("/synthetic");
    const body = response.json();
    lifecycle.destroy();
    lifecycle.init();
    gate.resolve({ old: true });
    await assert.rejects(body, { name: "AbortError" });
    lifecycle.destroy();
});
test("an external cancellation signal also aborts owned requests", async () => {
    let signal;
    const lifecycle = create({
            fetch: async (_url, options) => {
                signal = options.signal;
                return new Response("{}");
            },
        }),
        external = new AbortController();
    lifecycle.init();
    await lifecycle.fetch("/synthetic", { signal: external.signal });
    external.abort();
    assert.equal(signal.aborted, true);
    lifecycle.destroy();
});
test("observers disconnect and reject callbacks from a former mount", () => {
    let callback,
        disconnects = 0;
    class Observer {
        constructor(fn) {
            callback = fn;
        }
        observe() {}
        disconnect() {
            disconnects++;
        }
    }
    const lifecycle = create();
    lifecycle.init();
    let calls = 0;
    const Type = lifecycle.observer(Observer);
    new Type(() => calls++).observe({});
    callback();
    assert.equal(calls, 1);
    lifecycle.destroy();
    lifecycle.init();
    callback();
    assert.equal(calls, 1);
    assert.equal(disconnects, 1);
    lifecycle.destroy();
});
test("Status transport starts once, closes each stream, clears polling and rejects superseded stream events", () => {
    const timers = clock(),
        sources = [];
    let logs = 0;
    class Source {
        constructor() {
            this.listeners = new Map();
            this.closed = 0;
            sources.push(this);
        }
        addEventListener(type, callback) {
            this.listeners.set(type, callback);
        }
        close() {
            this.closed++;
        }
    }
    const controller = createController({
        timers,
        EventSourceType: Source,
        onJobLogEvent: () => logs++,
    });
    controller.start();
    controller.start();
    assert.equal(sources.length, 1);
    sources[0].listeners.get("error")();
    const reconnect = [...timers.pending.values()].at(-1);
    controller.destroy();
    assert.equal(timers.pending.size, 0);
    reconnect();
    assert.equal(sources.length, 1);
    controller.start();
    assert.equal(sources.length, 2);
    sources[0].listeners.get("dashboard")({ data: '{"reason":"job.log"}' });
    assert.equal(logs, 0);
    sources[1].listeners.get("dashboard")({ data: '{"reason":"job.log"}' });
    assert.equal(logs, 1);
    controller.destroy();
    assert.equal(timers.pending.size, 0);
    assert.equal(sources[1].closed, 1);
});

test("Status resynchronizes audits without replaying every active log on a contiguous stream", () => {
    const timers = clock();
    const refreshes = [];
    let recoveries = 0;
    let source;
    class Source extends EventTarget {
        constructor() {
            super();
            source = this;
        }
        close() {}
    }
    const controller = createController({
        timers, EventSourceType: Source,
        onDashboardRefresh: streams => refreshes.push(streams),
        onReconnect: () => recoveries++
    });
    controller.start();
    source.dispatchEvent(new Event("open"));
    const send = payload => {
        source.dispatchEvent(new MessageEvent("dashboard", { data: JSON.stringify(payload) }));
        const [id, flush] = [...timers.pending.entries()].at(-1);
        timers.pending.delete(id);
        flush();
    };
    send({ reason: "server.update", revision: 1, audit: true });
    assert.equal(refreshes.length, 2);
    assert.ok(refreshes[1].includes("servers") && refreshes[1].includes("dashboard"));
    assert.equal(recoveries, 1);

    send({ reason: "server.update", revision: 3, audit: true });
    assert.equal(recoveries, 2, "a skipped revision still recovers followed logs");
    send({ reason: "unexpected.change", revision: 4 });
    assert.equal(recoveries, 3, "an unrecognized event still recovers followed logs");
    controller.destroy();
});

test("timer ownership handles equal animation and timeout identifiers independently", () => {
    const cleared = [];
    const lifecycle = create({
        setTimeout: () => 1,
        requestAnimationFrame: () => 1,
        clearTimeout: (id) => cleared.push(["timeout", id]),
        cancelAnimationFrame: (id) => cleared.push(["frame", id]),
    });
    lifecycle.init();
    lifecycle.setTimeout(() => {}, 0);
    lifecycle.requestAnimationFrame(() => {});
    lifecycle.destroy();
    assert.deepEqual(cleared, [
        ["timeout", 1],
        ["frame", 1],
    ]);
});
test("request completion and destruction detach external cancellation listeners", async () => {
    class Signal extends EventTarget {
        constructor() {
            super();
            this.count = 0;
            this.aborted = false;
        }
        addEventListener(...args) {
            this.count++;
            super.addEventListener(...args);
        }
        removeEventListener(...args) {
            this.count--;
            super.removeEventListener(...args);
        }
    }
    const signal = new Signal(),
        lifecycle = create({ fetch: async () => new Response("{}") });
    lifecycle.init();
    const response = await lifecycle.fetch("/synthetic", { signal });
    assert.equal(signal.count, 1);
    await response.json();
    assert.equal(signal.count, 0);
    await lifecycle.fetch("/synthetic", { signal });
    assert.equal(signal.count, 1);
    lifecycle.destroy();
    assert.equal(signal.count, 0);
});

test("discarded Status error responses do not accumulate across polling cycles", async () => {
    const vm = require("node:vm"),
        fs = require("node:fs"),
        signals = [];
    let cancelled = 0;
    const context = {
        BrowserEffects: {
            create: () => create({
                fetch: async (_url, options) => {
                    signals.push(options.signal);
                    return new Response(new ReadableStream({
                        cancel() { cancelled++; },
                    }), { status: 503 });
                },
            }),
        },
        window: { canUseScope: () => true },
        console: { error() {} },
        URLSearchParams,
    };
    vm.runInNewContext(fs.readFileSync(
        require.resolve("../../static/js/status-snapshot-adapter.js"), "utf8",
    ), context);
    const adapter = context.StatusSnapshotAdapter.create({
        dispatchStatusInteraction: () => [],
        encodeURIComponent,
    });
    adapter.init();
    for (let cycle = 0; cycle < 5; cycle++) {
        for (const stream of [
            "servers", "dashboard", "commandHistory", "audit",
            "observability", "policies", "globalKey",
        ]) {
            await adapter.executeStatusSnapshotFetch({ stream, requestId: cycle });
        }
        await adapter.executeJobLogFetch({ jobId: "synthetic", serverName: "host" });
    }
    assert.equal(signals.length, 40);
    adapter.destroy();
    assert.equal(signals.filter((signal) => signal.aborted).length, 0,
        "completed error responses must be released before page destruction");
    assert.equal(cancelled, 40, "discarded bodies must stop consuming transport");
});

test("Status log recovery bounds reads, starts the open job first and drops superseded jobs", async () => {
    const vm = require("node:vm"), fs = require("node:fs");
    const pending = new Map();
    const started = [];
    let peak = 0;
    let stale = false;
    const context = {
        BrowserEffects: {
            create: () => create({
                fetch: async (url) => {
                    const job = url.split("/")[3];
                    started.push(job);
                    const response = deferred();
                    pending.set(job, response);
                    peak = Math.max(peak, pending.size);
                    await response.promise;
                    pending.delete(job);
                    return new Response('{"fragments":[]}');
                },
            }),
        },
        window: { canUseScope: () => true },
        console,
        URLSearchParams,
    };
    vm.runInNewContext(fs.readFileSync(
        require.resolve("../../static/js/status-snapshot-adapter.js"), "utf8",
    ), context);
    const adapter = context.StatusSnapshotAdapter.create({
        dispatchStatusInteraction: () => [],
        encodeURIComponent,
        isOpenJobLog: (name) => name === "open-host",
        isCurrentJobLog: (_name, job) => job !== "job-10" || !stale,
    });
    adapter.init();
    const reads = Array.from({ length: 12 }, (_, index) =>
        adapter.executeJobLogFetch({ jobId: `job-${index}`, serverName: index === 11 ? "open-host" : `host-${index}` }));
    await new Promise(setImmediate);
    assert.equal(started[0], "job-11");
    assert.equal(started.length, 8);
    stale = true;
    for (const response of pending.values()) response.resolve();
    await new Promise(setImmediate);
    for (const response of pending.values()) response.resolve();
    await Promise.all(reads);
    assert.equal(peak, 8);
    assert.equal(started.length, 11);
    assert.equal(started.includes("job-10"), false);
    adapter.destroy();
});

test("Status recovers the exact batch after a lost response and rate-limited replay", async () => {
    const vm = require("node:vm"), fs = require("node:fs");
    const environment = clock(), previousLoaded = deferred(), outcomeLoaded = deferred();
    const buttons = new Map(), signals = [], completed = deferred();
    let jobStatus = "waiting_approval", rendered;
    let latestAvailable = false;
    const submissionIDs = [];
    const context = {
        BrowserEffects: {
            create: () => create({ ...environment, fetch: async (url, options) => {
                signals.push(options.signal);
                if (url.endsWith("/latest")) {
                    if (!latestAvailable) return new Response(null, { status: 503 });
                    return Response.json({
                        batch: { id: "previous", hosts: [{ name: "previous", state: "running" }] },
                    });
                }
                if (options.method === "POST") {
                    const { id } = JSON.parse(options.body);
                    submissionIDs.push(id);
                    if (submissionIDs.length === 1) return new Response("gateway timeout", { status: 504 });
                    return Response.json({ error: "Too many requests" }, { status: 429 });
                }
                if (url.endsWith(`/${submissionIDs[0]}`)) return Response.json({
                    batch: {
                        id: submissionIDs[0], hosts: [{
                            name: "synthetic", job_id: "job",
                            state: jobStatus === "succeeded" ? "completed" : jobStatus, job_status: jobStatus,
                        }],
                    },
                });
                return new Response(null, { status: 404 });
            } }),
        },
        window: {
            notifyApp() {}, canUseScope() { return true; }, location: { search: "" },
            crypto: require("node:crypto").webcrypto,
        },
        URLSearchParams,
        encodeURIComponent,
        document: { getElementById(id) {
            if (!buttons.has(id)) buttons.set(id, new EventTarget());
            return buttons.get(id);
        } },
        console,
    };
    vm.runInNewContext(fs.readFileSync(
        require.resolve("../../static/js/index-bulk-actions.js"), "utf8",
    ), context);
    const adapter = context.StatusBulkActions.create({
        getStatusView: () => ({ actions: { bulk: null, inFlight: [{ operationId: "bulk" }] } }),
        statusInteraction: { planBulkAction: () => ({
            id: "bulk", actionKey: "update", selectedNames: ["synthetic"],
            eligibleNames: ["synthetic"], skippedHosts: [],
            payloadFacts: { synthetic: { serverIdentity: "identity" } },
        }) },
        renderHTML(_, markup) {
            rendered = markup;
            if (markup.includes("previous")) previousLoaded.resolve();
            if (markup.includes("Succeeded")) outcomeLoaded.resolve();
        },
        escapeHtml: String,
        dispatchStatusInteraction: (event) => {
            if (event.type === "actionCompleted" || event.type === "actionFailed") completed.resolve(event);
        },
    });
    const runNextRead = () => {
        const [timer, callback] = environment.pending.entries().next().value;
        environment.pending.delete(timer);
        callback();
    };
    adapter.init();
    await new Promise(setImmediate);
    latestAvailable = true;
    assert.equal(environment.pending.size, 1, "reopening the latest batch retries a transient failure");
    runNextRead();
    await previousLoaded.promise;
    buttons.get("bulk-update").dispatchEvent(new Event("click"));
    assert.equal((await completed.promise).type, "actionCompleted");
    assert.match(rendered, />Accepted</);
    assert.match(rendered, />Waiting for approval</);
    assert.equal(environment.pending.size, 0);
    jobStatus = "succeeded";
    adapter.refreshWaitingBatch();
    assert.equal(environment.pending.size, 1);
    runNextRead();
    await outcomeLoaded.promise;
    adapter.destroy();
    assert.equal(submissionIDs.length, 2);
    assert.equal(submissionIDs[0], submissionIDs[1]);
    assert.equal(signals.filter(signal => signal.aborted).length, 0);
});

test("discarding an unused response preserves cancellation and stale rejection for a pending JSON body", async () => {
    const gate = deferred(), signals = [];
    const lifecycle = create({ fetch: async (_url, options) => {
        signals.push(options.signal);
        return signals.length === 1 ? new Response("unused") : { json: () => gate.promise };
    } });
    lifecycle.init();
    const unused = await lifecycle.fetch("/unused");
    lifecycle.discardResponse(unused);
    lifecycle.discardResponse(unused);
    const response = await lifecycle.fetch("/pending-json");
    const body = response.json();
    lifecycle.destroy();
    lifecycle.init();
    gate.resolve({ superseded: true });
    await assert.rejects(body, { name: "AbortError" });
    assert.equal(signals[0].aborted, false);
    assert.equal(signals[1].aborted, true);
    lifecycle.destroy();
});

test("Status snapshot adapter rejects the previous lifetime even when the same adapter initializes again", async () => {
    const vm = require("node:vm"),
        fs = require("node:fs");
    const gate = deferred(),
        events = [];
    const context = {
        BrowserEffects: { create: () => create({ fetch: () => gate.promise }) },
        window: { canUseScope: () => true },
        console,
    };
    vm.runInNewContext(
        fs.readFileSync(
            require.resolve("../../static/js/status-snapshot-adapter.js"),
            "utf8",
        ),
        context,
    );
    const adapter = context.StatusSnapshotAdapter.create({
        dispatchStatusInteraction: (event) => events.push(event),
        encodeURIComponent,
    });
    adapter.init();
    const pending = adapter.executeStatusSnapshotFetch({
        stream: "servers",
        requestId: "old",
    });
    adapter.destroy();
    adapter.init();
    gate.resolve(new Response("[]"));
    await pending;
    assert.deepEqual(events, []);
    adapter.destroy();
});
test("Admin notification adapter cannot render a previous request failure after destroy and reinitialization", async () => {
    const vm = require("node:vm"),
        fs = require("node:fs");
    const gate = deferred();
    let writes = 0;
    const context = {
        BrowserEffects: { create: () => create({ fetch: () => gate.promise }) },
        window: {},
        document: {
            getElementById(id) {
                if (id === "notification-history") return null;
                writes++;
                return {};
            },
            querySelectorAll: () => [],
        },
        console,
    };
    vm.runInNewContext(
        fs.readFileSync(
            require.resolve("../../static/js/admin-notifications-adapter.js"),
            "utf8",
        ),
        context,
    );
    const adapter = context.AdminNotificationsAdapter.create({
        beginAdminSnapshot: () => 1,
        adminPageInteraction: {
            dispatch: () => {
                writes++;
                return [];
            },
        },
        renderAdminWorkspace: () => writes++,
    });
    adapter.init();
    const pending = adapter.fetchNotificationSettings();
    adapter.destroy();
    adapter.init();
    gate.resolve(new Response("{}", { status: 503 }));
    await pending;
    assert.equal(writes, 0);
    adapter.destroy();
});
test("a delayed policy job error body cannot notify a subsequent adapter lifetime", async () => {
    const vm = require("node:vm"), fs = require("node:fs");
    const body = deferred(), reading = deferred(), notifications = [];
    const context = {
        BrowserEffects: {
            create: () => create({ fetch: async () => ({
                ok: false,
                json: () => {
                    reading.resolve();
                    return body.promise;
                },
            }) }),
        },
        window: { notifyApp: (message) => notifications.push(message) },
        console,
    };
    vm.runInNewContext(fs.readFileSync(
        require.resolve("../../static/js/admin-policies-adapter.js"), "utf8",
    ), context);
    const adapter = context.AdminPoliciesAdapter.create({
        scheduledPolicyAdministration: {
            dispatch: () => [{ type: "fetchSnapshot", requestId: "old" }],
        },
        encodeURIComponent,
        // Common error parsing deliberately turns a failed body into fallback
        // text, including when destruction aborted that body.
        parseErrorResponse: async (response, fallback) => {
            const data = await response.json().catch(() => ({}));
            return data.error || fallback;
        },
    });
    adapter.init();
    adapter.handleScheduledRunsTableClick({ target: {
        closest: () => ({ dataset: { action: "job-detail", jobId: "old-job" } }),
    } });
    await reading.promise;
    adapter.destroy();
    adapter.init();
    body.resolve({ error: "superseded job error" });
    await new Promise(setImmediate);
    assert.deepEqual(notifications, []);
    adapter.destroy();
});

test("an old account confirmation cannot admit a command after adapter destruction and reinitialization", async () => {
    const vm = require("node:vm"),
        fs = require("node:fs"),
        gate = deferred();
    let commands = 0;
    const context = {
        BrowserEffects: { create },
        window: { confirmAction: () => gate.promise },
        console,
    };
    vm.runInNewContext(
        fs.readFileSync(
            require.resolve("../../static/js/admin-account-adapter.js"),
            "utf8",
        ),
        context,
    );
    const adapter = context.AdminAccountAdapter.create({
        adminPageView: () => ({ account: { otherSessions: [{}] } }),
        beginAdminCommand: () => {
            commands++;
            return null;
        },
    });
    adapter.init();
    const pending = adapter.clearOtherAuthSessions();
    adapter.destroy();
    adapter.init();
    gate.resolve(true);
    await pending;
    assert.equal(commands, 0);
    adapter.destroy();
});
test("an old typed backup confirmation cannot admit restore or alter feedback in a new adapter lifetime", async () => {
    const vm = require("node:vm"),
        fs = require("node:fs"),
        gate = deferred();
    let commands = 0,
        feedback = 0;
    const controls = {
        "backup-restore-file": { files: [{ name: "synthetic.slubkp" }] },
        "backup-restore-passphrase": { value: "SyntheticOnly123!" },
    };
    const context = {
        BrowserEffects: { create },
        window: {
            confirmTypedAction: () => gate.promise,
            notifyApp: () => feedback++,
        },
        document: { getElementById: (id) => controls[id] },
        console,
    };
    // Load the shared identity transport used by the backup adapter.
    context.window.BrowserEffects = { create };
    vm.runInNewContext(fs.readFileSync(require.resolve('../../static/js/reauthentication.js'), 'utf8'), context);
    context.Reauthentication = context.window.Reauthentication;
    vm.runInNewContext(
        fs.readFileSync(
            require.resolve("../../static/js/admin-backup-adapter.js"),
            "utf8",
        ),
        context,
    );
    const adapter = context.AdminBackupAdapter.create({
        adminPageView: () => ({ backup: { review: { safeCounts: {} } } }),
        adminPageInteraction: { planCommand: () => ({ enabled: true }), dispatch() {} },
        beginAdminCommand: () => {
            commands++;
            return null;
        },
    });
    adapter.init();
    const pending = adapter.restoreBackup();
    adapter.destroy();
    adapter.init();
    gate.resolve(true);
    await pending;
    assert.equal(commands, 0);
    assert.equal(feedback, 0);
    adapter.destroy();
});
test("a policy deletion confirmation from a prior lifetime cannot reach Scheduled Policy Administration Interaction command admission", async () => {
    const vm = require("node:vm"),
        fs = require("node:fs"),
        gate = deferred();
    let commands = 0;
    const context = {
        BrowserEffects: { create },
        window: { confirmTypedAction: () => gate.promise },
        console,
    };
    vm.runInNewContext(
        fs.readFileSync(
            require.resolve("../../static/js/admin-policies-adapter.js"),
            "utf8",
        ),
        context,
    );
    const adapter = context.AdminPoliciesAdapter.create({
        scheduledPolicyView: () => ({
            policies: [{ id: 1, name: "Synthetic policy" }],
        }),
        scheduledPolicyAdministration: {
            planCommand: () => ({
                enabled: true,
                policy: { name: "Synthetic policy" },
            }),
            dispatch: () => {
                commands++;
                return [];
            },
        },
    });
    adapter.init();
    adapter.handleScheduledPolicyTableClick({
        target: {
            closest: () => ({ dataset: { action: "delete-policy", id: "1" } }),
        },
    });
    adapter.destroy();
    adapter.init();
    gate.resolve(true);
    await gate.promise;
    await new Promise(setImmediate);
    assert.equal(commands, 0);
    adapter.destroy();
});
