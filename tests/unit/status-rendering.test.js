const test = require("node:test");
const assert = require("node:assert/strict");

const StatusRendering = require("../../static/js/status-rendering.js");

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
