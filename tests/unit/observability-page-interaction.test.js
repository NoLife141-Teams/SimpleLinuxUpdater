const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const {
    createStore,
    projectFleetHealth,
    projectDurationConfidence,
    projectFleetTrendSeries,
    projectHealthCollection,
    projectHealthTrendCSV,
    projectTrendChart,
    formatStorageKB,
} = require("../../static/js/observability-page-interaction.js");

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
    store.dispatch({
        type: "sourceSucceeded",
        source: "summary",
        requestID: firstSummary.requestID,
        data: { version: 0 },
        timeZone: "Asia/Tokyo",
    });
    assert.equal(store.getView().summary.data.version, 1);
    assert.equal(store.getView().timeZone, "UTC");
    store.dispatch({
        type: "sourceSucceeded",
        source: "summary",
        requestID: nextSummary.requestID,
        data: { version: 2 },
        timeZone: "America/Toronto",
    });
    assert.equal(store.getView().summary.data.version, 2);
    assert.equal(store.getView().timeZone, "America/Toronto");
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

test("validated shareable selections and source retry stay inside Observability Page Interaction", () => {
    assert.equal(createStore({ window: "90d" }).getView().selectedWindow, "7d");
    const store = createStore({
        window: "24h",
        host: "alpha",
        search: "prod",
        attention: "failures",
        sort: "freshness",
        page: 3,
    });
    let view = store.getView();
    assert.equal(view.selectedWindow, "24h");
    assert.equal(view.selectedHost, "alpha");
    assert.equal(view.search, "prod");
    assert.equal(view.attention, "failures");
    assert.equal(view.sort, "freshness");
    assert.equal(view.page, 3);

    let effects = store.dispatch({ type: "pageShown" });
    const summary = effect(effects, "loadSource", "summary");
    const trends = effect(effects, "loadSource", "trends");
    assert.equal(trends.queryWindow, "24h");
    assert.equal(trends.host, "");
    store.dispatch({ type: "sourceFailed", source: "summary", requestID: summary.requestID, error: { kind: "http", status: 503 } });
    store.dispatch({ type: "sourceSucceeded", source: "trends", requestID: trends.requestID, data: { generated_at: "2026-07-29T01:00:00Z", servers: [] }, unfiltered: true });
    assert.equal(store.getView().selectedHost, "");
    assert.equal(store.getView().page, 1);

    effects = store.dispatch({ type: "retrySource", source: "summary" });
    assert.equal(effects.filter(item => item.type === "loadSource").length, 1);
    assert.equal(effect(effects, "loadSource", "summary").window, "24h");
    assert.equal(store.getView().summary.status, "refreshing");

    store.dispatch({ type: "filtersChanged", search: "db", attention: "disk", sort: "disk", page: 4 });
    view = store.getView();
    assert.equal(view.search, "db");
    assert.equal(view.attention, "disk");
    assert.equal(view.sort, "disk");
    assert.equal(view.page, 1);
});

test("valid deep-linked host is verified by the unfiltered result before loading its 24h trend", () => {
    const store = createStore({ window: "24h", host: "alpha" });
    const effects = store.dispatch({ type: "pageShown" });
    const firstTrends = effect(effects, "loadSource", "trends");
    assert.equal(firstTrends.host, "");
    assert.equal(firstTrends.queryWindow, "24h");

    const settled = store.dispatch({
        type: "sourceSucceeded",
        source: "trends",
        requestID: firstTrends.requestID,
        data: { servers: [{ name: "alpha" }, { name: "beta" }] },
        unfiltered: true,
    });
    const filteredTrends = settled.filter(item => item.type === "loadSource" && item.source === "trends").at(-1);
    assert.equal(filteredTrends.host, "alpha");
    assert.equal(filteredTrends.queryWindow, "24h");
});

test("fleet trend projection combines staggered host samples into app-local daily buckets", () => {
    const series = projectFleetTrendSeries([
        {
            name: "alpha",
            points: [
                { captured_at: "2026-07-22T13:00:00Z", package_count: 2 },
                { captured_at: "2026-07-22T20:00:00Z", package_count: 3 },
                { captured_at: "2026-07-23T16:00:00Z", package_count: 4 },
            ],
        },
        {
            name: "beta",
            points: [
                { captured_at: "2026-07-22T15:00:00Z", package_count: 5 },
                { captured_at: "2026-07-23T14:00:00Z", package_count: 6 },
            ],
        },
    ], "packages", { window: "7d", timeZone: "America/Toronto" });

    assert.deepEqual(series, [
        {
            timestamp: "2026-07-22T04:00:00.000Z",
            lastObservedAt: "2026-07-22T20:00:00Z",
            value: 8,
            samples: 2,
            observations: 3,
            bucketKey: "2026-07-22",
            bucketUnit: "day",
        },
        {
            timestamp: "2026-07-23T04:00:00.000Z",
            lastObservedAt: "2026-07-23T16:00:00Z",
            value: 10,
            samples: 2,
            observations: 2,
            bucketKey: "2026-07-23",
            bucketUnit: "day",
        },
    ]);
});

test("failure trend projection sums events and preserves zeroes in hourly buckets", () => {
    const series = projectFleetTrendSeries([
        {
            name: "alpha",
            points: [
                { captured_at: "2026-07-29T10:05:00Z", last_update_status: "failure", last_scan_status: "error" },
                { captured_at: "2026-07-29T10:45:00Z", last_update_status: "interrupted" },
                { captured_at: "2026-07-29T11:15:00Z", last_update_status: "success" },
            ],
        },
        {
            name: "beta",
            points: [
                { captured_at: "2026-07-29T10:30:00Z", last_scan_status: "cancelled" },
                { captured_at: "2026-07-29T11:40:00Z", last_scan_status: "success" },
            ],
        },
    ], "failures", { window: "24h", timeZone: "UTC" });

    assert.deepEqual(series, [
        {
            timestamp: "2026-07-29T10:00:00.000Z",
            lastObservedAt: "2026-07-29T10:45:00Z",
            value: 4,
            samples: 2,
            observations: 3,
            bucketKey: "2026-07-29T10",
            bucketUnit: "hour",
        },
        {
            timestamp: "2026-07-29T11:00:00.000Z",
            lastObservedAt: "2026-07-29T11:40:00Z",
            value: 0,
            samples: 2,
            observations: 2,
            bucketKey: "2026-07-29T11",
            bucketUnit: "hour",
        },
    ]);
});

test("disk trend projection preserves unavailable buckets as chart gaps", () => {
    const servers = [{
        name: "alpha",
        points: [
            { captured_at: "2026-07-29T09:00:00Z", disk_free_kb: 1024 },
            { captured_at: "2026-07-29T10:00:00Z", disk_free_kb: 0 },
            { captured_at: "2026-07-29T11:00:00Z", disk_free_kb: 768 },
        ],
    }];
    const series = projectFleetTrendSeries(servers, "disk", { window: "24h", timeZone: "UTC" });
    assert.deepEqual(series.map(point => point.value), [1024, null, 768]);

    const chart = projectTrendChart(series);
    assert.deepEqual(chart.segments.map(segment => segment.map(point => point.value)), [[1024], [768]]);
});

test("health trend CSV projection exports every accepted point with explicit missing facts", () => {
    const projection = projectHealthTrendCSV([
        {
            name: "alpha",
            latest: { captured_at: "2026-07-29T11:00:00Z" },
            points: [
                {
                    captured_at: "2026-07-29T10:00:00Z",
                    captured_at_display: "Jul 29, 2026, 06:00 EDT",
                    source: "facts",
                    package_count: 4,
                    security_count: 2,
                    disk_free_kb: 1024,
                    disk_total_kb: 4096,
                    apt_status: "ok",
                    disk_status: "ok",
                    last_update_status: "success",
                    reboot_required: false,
                },
                {
                    captured_at: "2026-07-29T11:00:00Z",
                    captured_at_display: "Jul 29, 2026, 07:00 EDT",
                    source: "audit",
                    package_count: 3,
                    security_count: 1,
                    disk_free_kb: 0,
                    disk_total_kb: 0,
                    last_scan_status: "failure",
                    reboot_required: null,
                },
            ],
        },
        { name: "missing", samples: 0, points: [] },
    ]);

    assert.equal(projection.header[1], "Captured at app time");
    assert.equal(projection.header[2], "Captured at UTC");
    assert.equal(projection.rows.length, 3);
    assert.deepEqual(projection.rows[0].slice(0, 4), [
        "alpha",
        "Jul 29, 2026, 06:00 EDT",
        "2026-07-29T10:00:00Z",
        "facts",
    ]);
    assert.equal(projection.rows[0][6], 1024);
    assert.equal(projection.rows[0][12], "no");
    assert.equal(projection.rows[1][6], "");
    assert.equal(projection.rows[1][7], "");
    assert.equal(projection.rows[1][12], "");
    assert.deepEqual(projection.rows[2], [
        "missing", "", "", "", "", "", "", "", "", "", "", "", "",
    ]);
});

test("fleet buckets include only hosts observed during that period", () => {
    const series = projectFleetTrendSeries([
        {
            name: "alpha",
            points: [
                { captured_at: "2026-07-22T12:00:00Z", package_count: 4 },
                { captured_at: "2026-07-23T12:00:00Z", package_count: 5 },
            ],
        },
        {
            name: "beta",
            points: [
                { captured_at: "2026-07-22T13:00:00Z", package_count: 10 },
            ],
        },
    ], "packages", { window: "7d", timeZone: "UTC" });

    assert.deepEqual(series.map(point => [point.bucketKey, point.value, point.samples]), [
        ["2026-07-22", 14, 2],
        ["2026-07-23", 5, 1],
    ]);
});

test("hourly buckets keep both repeated DST fallback hours distinct", () => {
    const series = projectFleetTrendSeries([{
        name: "alpha",
        points: [
            { captured_at: "2026-11-01T05:30:00Z", package_count: 1 },
            { captured_at: "2026-11-01T06:30:00Z", package_count: 2 },
        ],
    }], "packages", { window: "24h", timeZone: "America/Toronto" });

    assert.deepEqual(series.map(point => [point.bucketKey, point.timestamp, point.value]), [
        ["2026-11-01T01", "2026-11-01T05:00:00.000Z", 1],
        ["2026-11-01T01", "2026-11-01T06:00:00.000Z", 2],
    ]);
});

test("daily trend buckets follow app-local midnight", () => {
    const localDays = projectFleetTrendSeries([{
        name: "alpha",
        points: [
            { captured_at: "2026-07-23T03:30:00Z", package_count: 1 },
            { captured_at: "2026-07-23T04:30:00Z", package_count: 2 },
        ],
    }], "packages", { window: "7d", timeZone: "America/Toronto" });
    assert.deepEqual(localDays.map(point => point.bucketKey), ["2026-07-22", "2026-07-23"]);
});

test("trend chart projection exposes a zero baseline and proportional time scale", () => {
    const chart = projectTrendChart([
        { timestamp: "2026-07-22T00:00:00Z", value: 0 },
        { timestamp: "2026-07-23T00:00:00Z", value: 18 },
        { timestamp: "2026-07-29T00:00:00Z", value: 4 },
    ], { integer: true });

    assert.deepEqual(chart.yTicks, [0, 10, 20]);
    assert.equal(chart.yMin, 0);
    assert.equal(chart.yMax, 20);
    assert.deepEqual(chart.xTicks, [
        "2026-07-22T00:00:00.000Z",
        "2026-07-25T12:00:00.000Z",
        "2026-07-29T00:00:00.000Z",
    ]);
    assert.deepEqual(chart.points.map(point => [point.xRatio, point.yRatio]), [
        [0, 0],
        [1 / 7, 0.9],
        [1, 0.2],
    ]);
});

test("storage labels adapt across KB, MB, GB, and TB", () => {
    assert.equal(formatStorageKB(512), "512 KB");
    assert.equal(formatStorageKB(1024), "1 MB");
    assert.equal(formatStorageKB(1024 * 1024), "1.0 GB");
    assert.equal(formatStorageKB(1024 * 1024 * 1024), "1.0 TB");
});

test("health projection clamps pages and identifies stale observations deterministically", () => {
    const result = projectHealthCollection([
        { name: "stale", latest: { captured_at: "2026-07-26T00:00:00Z", disk_free_kb: 100 } },
        { name: "fresh", latest: { captured_at: "2026-07-29T11:00:00Z", disk_free_kb: 100 } },
    ], {
        window: "24h",
        page: 99,
        sort: "name",
        nowMS: Date.parse("2026-07-29T12:00:00Z"),
    });

    assert.equal(result.page, 1);
    assert.deepEqual(result.staleNames, ["stale"]);
});

test("health projection distinguishes missing observations from stale observations", () => {
    const servers = [
        { name: "missing", samples: 0, points: [] },
        { name: "stale", latest: { captured_at: "2026-07-26T00:00:00Z", disk_free_kb: 100 } },
    ];
    const options = {
        window: "24h",
        nowMS: Date.parse("2026-07-29T12:00:00Z"),
    };

    assert.deepEqual(
        projectHealthCollection(servers, { ...options, attention: "missing" }).items.map(server => server.name),
        ["missing"]
    );
    assert.deepEqual(
        projectHealthCollection(servers, { ...options, attention: "stale" }).items.map(server => server.name),
        ["stale"]
    );
});

test("fleet severity and duration confidence project documented boundaries", () => {
    assert.deepEqual(projectFleetHealth({ updatesTotal: 0, successRate: 0 }), { state: "neutral", label: "No data" });
    assert.deepEqual(projectFleetHealth({ updatesTotal: 10, successRate: 95 }), { state: "healthy", label: "Healthy" });
    assert.deepEqual(projectFleetHealth({ updatesTotal: 10, successRate: 94.99 }), { state: "degraded", label: "Degraded" });
    assert.deepEqual(projectFleetHealth({ updatesTotal: 10, successRate: 80 }), { state: "degraded", label: "Degraded" });
    assert.deepEqual(projectFleetHealth({ updatesTotal: 10, successRate: 79.99 }), { state: "critical", label: "Critical" });

    assert.deepEqual(projectDurationConfidence({ samples: 0, total: 4 }), { state: "no-data", label: "No duration data" });
    assert.deepEqual(projectDurationConfidence({ samples: 1, total: 4 }), { state: "low", label: "Low confidence" });
    assert.deepEqual(projectDurationConfidence({ samples: 2, total: 4 }), { state: "representative", label: "Representative" });
    assert.deepEqual(projectDurationConfidence({ samples: 4, total: 4 }), { state: "representative", label: "Representative" });
});

test("Observability adapter does not restore interaction state globals", () => {
    const source = fs.readFileSync(path.join(__dirname, "../../static/js/observability.js"), "utf8");
    assert.doesNotMatch(source, /let\s+refreshIntervalId\s*=/);
    assert.doesNotMatch(source, /let\s+knownHealthTrendServers\s*=/);
    assert.doesNotMatch(source, /let\s+lastLifecycleAnnouncement\s*=/);
    assert.doesNotMatch(source, /let\s+lastFilteredHealthServers\s*=/);
    assert.doesNotMatch(source, /let\s+lastAcceptedSummary\s*=/);
    assert.doesNotMatch(source, /function\s+projectHealthServers\s*\(/);
    assert.doesNotMatch(source, /function\s+aggregateTrendSeries\s*\(/);
});
