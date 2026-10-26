const test = require("node:test");
const assert = require("node:assert/strict");

const StatusRendering = require("../../static/js/status-rendering.js");

test("a synchronous render shares one snapshot and releases it for the next state", () => {
    let reads = 0;
    let accepted = Object.freeze({ total: 1000, selected: "a", canApprove: true });
    const scope = StatusRendering.createSnapshotScope(() => { reads++; return accepted; });
    scope.run(() => {
        const first = scope.read();
        for (let index = 0; index < 1000; index++) assert.equal(scope.read(), first);
        accepted = Object.freeze({ total: 100, selected: "b", canApprove: false });
        scope.run(() => assert.equal(scope.read(), first));
        assert.equal(scope.read().canApprove, true);
    });
    assert.equal(reads, 1, "one domain projection per synchronous render regardless of row count");
    scope.run(() => assert.deepEqual(scope.read(), { total: 100, selected: "b", canApprove: false }));
    assert.equal(reads, 2);
    assert.throws(() => scope.run(() => { scope.read(); throw new Error("render failed"); }), /render failed/);
    accepted = Object.freeze({ total: 10, selected: "c", canApprove: false });
    assert.equal(scope.read(), accepted, "an exception cannot retain a stale snapshot");
    assert.equal(reads, 4);
    scope.read();
    assert.equal(reads, 5, "unscoped readers keep fresh domain snapshots");
});

test("Status rendering describes every degraded data source and its last good refresh", () => {
    const notice = StatusRendering.syncNotice({
        dashboard: { lastError: "HTTP 503", lastSuccessfulAt: "2026-08-05T03:00:00Z" },
        audit: { lastError: "offline", lastSuccessfulAt: "" },
        policies: { lastError: "", lastSuccessfulAt: "2026-08-05T03:01:00Z" }
    }, raw => raw ? `relative:${raw}` : "not yet successful");

    assert.deepEqual(notice, {
        visible: true,
        title: "Data freshness warning",
        summary: "Affected data: Dashboard and Audit trail. Visible maintenance data may be out of date.",
        lastSuccessText: "Last successful refresh: Dashboard relative:2026-08-05T03:00:00Z · Audit trail not yet successful"
    });
    assert.equal(StatusRendering.syncNotice({}, () => "unused").visible, false);
});
