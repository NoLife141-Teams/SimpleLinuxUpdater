const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { project, projectBulkReview, presentationFacts } = require("../../static/js/dashboard-projection-consumption.js");

function statusView(overrides = {}) {
    const servers = overrides.servers || [{ name: "alpha", status: "pending_approval", pending_updates: [{ security: true, cves: ["CVE-1"] }], has_key: true }];
    return {
        servers,
        visibleServers: servers,
        pageServers: servers,
        groups: [{ key: "", items: servers }],
        primaryServerName: servers[0]?.name || "",
        dashboardSnapshot: overrides.dashboardSnapshot || {},
        dashboardServers: overrides.dashboardServers || [],
        actionViews: overrides.actionViews || {},
        actions: { inFlightServerNames: [] },
        ...overrides
    };
}

test("canonical dashboard facts win and accepted action views remain authoritative", () => {
    const view = project({
        statusView: statusView({
            dashboardSnapshot: { fleet: { pending_packages: 41 } },
            dashboardServers: [{
                name: "alpha",
                timeline: { current_phase: "pending_approval", current_label: "Approval", state: "active", progress_pct: 25 },
                approval_triage: { pending_packages: 7, security_updates: 3, cve_count: 2, risk_level: "critical", risk_label: "Critical", risk_order: 9 },
                actions: { approve_all: { enabled: true } }
            }],
            actionViews: { alpha: { approve_all: { enabled: false, reason: "Contract blocks it" }, update: { enabled: true } } }
        })
    });

    assert.equal(view.fleet.pendingPackages, 41);
    assert.equal(view.selectedHost.risk.pendingPackages, 7);
    assert.equal(view.selectedHost.risk.label, "Critical");
    assert.equal(view.selectedHost.triage.can_approve_all, false);
    assert.equal(view.selectedHost.canRunUpdate, true);
    assert.equal(Object.isFrozen(view), true);
    assert.equal(Object.isFrozen(view.selectedHost), true);
});

test("missing and malformed canonical facts use deterministic compatibility fallbacks", () => {
    const view = project({ statusView: statusView({ dashboardSnapshot: { fleet: { pending_packages: "broken" } } }) });
    assert.equal(view.fleet.pendingPackages, 1);
    assert.equal(view.selectedHost.risk.securityUpdates, 1);
    assert.equal(view.selectedHost.risk.cveCount, 1);
    assert.equal(view.selectedHost.risk.label, "1 CVE");
});

test("unknown data stays explicit and optional extras degrade independently", () => {
    const server = { name: "unknown", status: "", pending_updates: [] };
    const view = project({ statusView: statusView({ servers: [server], dashboardServers: [{ name: "unknown", health: {} }] }), extras: { recentActivity: null, policySummary: null } });
    assert.equal(view.selectedHost.timeline.state, "idle");
    assert.equal(view.selectedHost.triage.facts_state, "unknown");
    assert.equal(view.selectedHost.risk.label, "Normal");
    assert.deepEqual(view.panels.recentActivity, []);
    assert.equal(view.summaries.policyCount, null);
});

test("kernel facts distinguish the running kernel from a newer installed kernel", () => {
    assert.equal(presentationFacts.kernelVersions("6.8.0-60-generic", "6.8.0-62-generic"), "6.8.0-60-generic → 6.8.0-62-generic");
    assert.equal(presentationFacts.kernelVersions("6.8.0-62-generic", "6.8.0-62-generic"), "6.8.0-62-generic");
    assert.equal(presentationFacts.kernelVersions("", "6.8.0-62-generic"), "Unknown → 6.8.0-62-generic");
    assert.equal(presentationFacts.kernelVersions("", ""), "Facts not collected");
});

test("fleet and attention projections use deterministic membership and ranking", () => {
    const servers = [
        { name: "beta", status: "error" },
        { name: "alpha", status: "error" },
        { name: "gamma", status: "idle" }
    ];
    const dashboardServers = [
        { name: "beta", approval_triage: { risk_order: 2 }, last_failed_update: { finished_at: "2026-01-01T00:00:00Z" } },
        { name: "alpha", approval_triage: { risk_order: 2 }, last_failed_update: { finished_at: "2026-01-02T00:00:00Z" }, health: { reboot_required: true } },
        { name: "gamma", approval_triage: { risk_level: "elevated", risk_order: 3 } }
    ];
    const view = project({ statusView: statusView({ servers, dashboardServers }) });
    assert.deepEqual(view.panels.failed.map(item => item.name), ["alpha", "beta"]);
    assert.deepEqual(view.panels.reboot.map(item => item.name), ["alpha"]);
    assert.deepEqual(view.panels.risk.map(item => item.name), ["gamma"]);
});

test("schedule, activity, and command history retain raw timestamps", () => {
    const servers = [{ name: "alpha", status: "idle" }];
    const view = project({
        statusView: statusView({
            servers,
            dashboardServers: [{ name: "alpha", next_run: { state: "scheduled", scheduled_for_utc: "2026-07-11T01:00:00Z" }, command_history: [
                { created_at: "2026-07-10T01:00:00Z", action: "older" },
                { created_at: "2026-07-10T03:00:00Z", action: "newer" }
            ] }]
        }),
        extras: { recentActivity: [
            { created_at: "2026-07-10T02:00:00Z", action: "older-audit", status: "success" },
            { created_at: "2026-07-10T04:00:00Z", action: "newer-audit", status: "success" }
        ] }
    });
    assert.equal(view.panels.scheduled[0].nextRun.scheduled_for_utc, "2026-07-11T01:00:00Z");
    assert.deepEqual(view.panels.commandHistory.map(item => item.action), ["newer", "older"]);
    assert.equal(view.panels.commandHistory[0].created_at, "2026-07-10T03:00:00Z");
    assert.deepEqual(view.panels.recentActivity.map(item => item.action), ["newer-audit", "older-audit"]);
    assert.equal(view.panels.recentActivity[0].createdAt, "2026-07-10T04:00:00Z");
});

test("bulk review projection owns labels and rows without planning commands", () => {
    const review = projectBulkReview({ actionLabel: "update", eligibleHosts: [{ name: "alpha" }], skippedHosts: [{ name: "beta", reason: "busy" }], warning: "Review" });
    assert.equal(review.title, "Review bulk update");
    assert.equal(review.summary, "1 eligible visible host will run. 1 host will be skipped.");
    assert.equal(review.canConfirm, true);
});

test("dashboard adapter cannot restore removed presentation derivation entry points", () => {
    const source = fs.readFileSync(path.join(__dirname, "../../static/js/index.js"), "utf8");
    assert.doesNotMatch(source, /function\s+compareRiskPriority\s*\(/);
    assert.doesNotMatch(source, /function\s+compareFailurePriority\s*\(/);
    assert.doesNotMatch(source, /function\s+getAuthPostureMetrics\s*\(/);
    assert.doesNotMatch(source, /dashboardSummary\?\.fleet/);
    assert.match(source, /dashboardConsumption\.project/);
});
