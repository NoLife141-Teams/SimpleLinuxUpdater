const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { createStore } = require("../../static/js/observability-page-interaction.js");

function effect(effects, type, source) {
    return effects.find(item => item.type === type && (!source || item.source === source));
}

test("visible page starts one concurrent full refresh and accepts either completion order", () => {
    const store = createStore();
    const effects = store.dispatch({ type: "pageShown" });
    const summary = effect(effects, "loadSource", "summary");
    const trends = effect(effects, "loadSource", "trends");
    assert.ok(summary);
    assert.ok(trends);
    assert.equal(summary.generation, trends.generation);

    store.dispatch({ type: "sourceSucceeded", source: "trends", requestID: trends.requestID, data: { servers: [{ name: "alpha" }] } });
    assert.deepEqual(store.getView().trends.data.servers, [{ name: "alpha" }]);
    assert.equal(store.getView().summary.status, "refreshing");
    store.dispatch({ type: "sourceSucceeded", source: "summary", requestID: summary.requestID, data: { totals: { updates_total: 2 } } });
    assert.equal(store.getView().summary.data.totals.updates_total, 2);
});

test("full refresh retains accepted data and ignores superseded source results", () => {
    const store = createStore();
    let effects = store.dispatch({ type: "pageShown" });
    const firstSummary = effect(effects, "loadSource", "summary");
    const firstTrends = effect(effects, "loadSource", "trends");
    store.dispatch({ type: "sourceSucceeded", source: "summary", requestID: firstSummary.requestID, data: { version: 1 } });
    store.dispatch({ type: "sourceSucceeded", source: "trends", requestID: firstTrends.requestID, data: { version: 1, servers: [] } });

    effects = store.dispatch({ type: "windowChanged", window: "30d" });
    const nextSummary = effect(effects, "loadSource", "summary");
    assert.equal(store.getView().summary.status, "refreshing");
    assert.equal(store.getView().summary.data.version, 1);
    store.dispatch({ type: "sourceSucceeded", source: "summary", requestID: firstSummary.requestID, data: { version: 0 } });
    assert.equal(store.getView().summary.data.version, 1);
    store.dispatch({ type: "sourceSucceeded", source: "summary", requestID: nextSummary.requestID, data: { version: 2 } });
    assert.equal(store.getView().summary.data.version, 2);
});

test("partial failure is source-specific and keeps accepted data", () => {
    const store = createStore();
    let effects = store.dispatch({ type: "pageShown" });
    let summary = effect(effects, "loadSource", "summary");
    let trends = effect(effects, "loadSource", "trends");
    store.dispatch({ type: "sourceSucceeded", source: "summary", requestID: summary.requestID, data: { version: 1 } });
    store.dispatch({ type: "sourceSucceeded", source: "trends", requestID: trends.requestID, data: { version: 1, servers: [] } });

    effects = store.dispatch({ type: "manualRefresh" });
    summary = effect(effects, "loadSource", "summary");
    trends = effect(effects, "loadSource", "trends");
    store.dispatch({ type: "sourceSucceeded", source: "summary", requestID: summary.requestID, data: { version: 2 } });
    store.dispatch({ type: "sourceFailed", source: "trends", requestID: trends.requestID, error: { kind: "http", status: 503 } });
    const view = store.getView();
    assert.equal(view.summary.status, "fresh");
    assert.equal(view.trends.status, "stale");
    assert.equal(view.trends.data.version, 1);
    assert.equal(view.trends.error.status, 503);
});

test("host selection refreshes trends only and filtered results preserve choices", () => {
    const store = createStore();
    let effects = store.dispatch({ type: "pageShown" });
    const trends = effect(effects, "loadSource", "trends");
    store.dispatch({ type: "sourceSucceeded", source: "trends", requestID: trends.requestID, data: { servers: [{ name: "beta" }, { name: "alpha" }] }, unfiltered: true });
    assert.deepEqual(store.getView().knownHosts, ["alpha", "beta"]);

    effects = store.dispatch({ type: "hostChanged", host: "alpha" });
    assert.equal(effects.filter(item => item.type === "loadSource").length, 1);
    const filtered = effect(effects, "loadSource", "trends");
    assert.equal(filtered.host, "alpha");
    store.dispatch({ type: "sourceSucceeded", source: "trends", requestID: filtered.requestID, data: { servers: [{ name: "alpha" }] }, unfiltered: false });
    assert.deepEqual(store.getView().knownHosts, ["alpha", "beta"]);
});

test("automatic refresh waits for settlement and visibility cancellation is silent", () => {
    const store = createStore({ refreshDelayMs: 15000 });
    let effects = store.dispatch({ type: "pageShown" });
    const summary = effect(effects, "loadSource", "summary");
    const trends = effect(effects, "loadSource", "trends");
    assert.equal(effect(store.dispatch({ type: "sourceSucceeded", source: "summary", requestID: summary.requestID, data: {} }), "scheduleRefresh"), undefined);
    effects = store.dispatch({ type: "sourceSucceeded", source: "trends", requestID: trends.requestID, data: { servers: [] }, unfiltered: true });
    assert.equal(effect(effects, "scheduleRefresh").delayMs, 15000);

    effects = store.dispatch({ type: "timerFired" });
    assert.equal(effects.filter(item => item.type === "loadSource").length, 2);
    effects = store.dispatch({ type: "pageHidden" });
    assert.ok(effect(effects, "cancelRefresh"));
    assert.ok(effects.some(item => item.type === "abortSource"));
    assert.equal(store.getView().pageVisible, false);
    assert.equal(store.getView().summary.error, null);
    effects = store.dispatch({ type: "pageShown" });
    assert.equal(effects.filter(item => item.type === "loadSource").length, 2);
});

test("Observability adapter does not restore interaction state globals", () => {
    const source = fs.readFileSync(path.join(__dirname, "../../static/js/observability.js"), "utf8");
    assert.doesNotMatch(source, /let\s+refreshIntervalId\s*=/);
    assert.doesNotMatch(source, /let\s+knownHealthTrendServers\s*=/);
});
