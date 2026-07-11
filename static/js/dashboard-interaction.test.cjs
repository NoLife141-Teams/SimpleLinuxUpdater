const test = require("node:test");
const assert = require("node:assert/strict");

const DashboardProjectionConsumption = require("./dashboard-projection-consumption.js");
const StatusPageInteraction = require("./status-page-interaction.js");
const StatusFormatting = require("./status-formatting.js");
const StatusTransport = require("./status-transport.js");

test("Status formatting presents shared operator values consistently", () => {
    assert.equal(StatusFormatting.duration(65000), "1m 5s");
    assert.equal(StatusFormatting.diskCapacity(16 * 1024 * 1024, 64 * 1024 * 1024), "16 GiB free of 64 GiB total");
    assert.equal(StatusFormatting.uptime(90061), "1d");
    assert.equal(StatusFormatting.statusLabel("pending_approval"), "pending approval");
});

test("Status transport falls back to polling without dashboard events", () => {
    const intervals = [];
    const timers = {
        setInterval(fn, ms) { intervals.push({ fn, ms }); return intervals.length; },
        clearInterval() {},
        setTimeout() { return 1; },
        clearTimeout() {}
    };
    const controller = StatusTransport.createController({ timers });
    controller.start();
    assert.equal(controller.isLive(), false);
    assert.deepEqual(intervals.slice(-2).map(item => item.ms), [5000, 30000]);
});

test("Dashboard Projection Consumption owns canonical approval and authentication facts", () => {
    const facts = DashboardProjectionConsumption.presentationFacts;
    const server = {
        name: "edge-01",
        status: "pending_approval",
        has_key: false,
        has_password: true,
        pending_updates: [
            { package: "openssl", security: true },
            { package: "kernel", security: true, kept_back: true }
        ],
        upgrade_plan: {
            standard_package_count: 1,
            kept_back_package_count: 1,
            standard_security_count: 1,
            total_security_count: 2,
            full_upgrade_package_count: 2,
            full_upgrade_plan_available: true,
            kept_back_security_plan_available: true
        }
    };

    assert.deepEqual(facts.approvalCounts(server), {
        total: 2,
        standard: 1,
        keptBack: 1,
        full: 2,
        security: 1,
        totalSecurity: 2,
        keptBackSecurity: 1,
        standardSecurity: 1,
        fullPlanAvailable: true,
        keptBackSecurityPlanAvailable: true,
        newPackages: [],
        removedPackages: [],
        keptBackSecurityNewPackages: [],
        keptBackSecurityRemovedPackages: []
    });
    assert.deepEqual(facts.authFacts(server, true), {
        label: "Global SSH key + password",
        effectiveKey: true,
        usesGlobalKey: true
    });
});

test("Status Page Interaction consumes canonical presentation facts for action plans", () => {
    const store = StatusPageInteraction.createStore({
        presentationFacts: DashboardProjectionConsumption.presentationFacts
    });
    store.dispatch({
        type: "serversSnapshotReceived",
        servers: [{
            name: "edge-01",
            status: "pending_approval",
            has_key: false,
            has_password: true,
            pending_updates: [{ package: "openssl", security: true }],
            upgrade_plan: { standard_package_count: 1, standard_security_count: 1 }
        }]
    });
    store.dispatch({ type: "globalKeyAvailabilityChanged", available: true });
    store.dispatch({ type: "selectionChanged", name: "edge-01", selected: true });

    const plan = store.planAction("edge-01", "approve_security");
    const bulkPlan = store.planBulkAction("approve_security");
    assert.equal(plan.enabled, true);
    assert.equal(bulkPlan.eligibleHosts[0].auth, "Global SSH key + password");
    assert.equal(plan.payloadFacts.counts.standardSecurity, 1);
});

test("Status Page Interaction preserves filters, selection, and drawer state", () => {
    const store = StatusPageInteraction.createStore({
        presentationFacts: DashboardProjectionConsumption.presentationFacts
    });
    store.dispatch({ type: "serversSnapshotReceived", servers: [
        { name: "prod-01", status: "updating", tags: ["prod"] },
        { name: "lab-01", status: "idle", tags: ["lab"] }
    ] });
    store.dispatch({ type: "filtersChanged", patch: { search: "prod" } });
    store.dispatch({ type: "selectionChanged", name: "prod-01", selected: true });
    store.dispatch({ type: "drawerOpened", name: "prod-01", tab: "logs" });

    const view = store.getView();
    assert.deepEqual(view.visibleServers.map(server => server.name), ["prod-01"]);
    assert.deepEqual(view.selectedNames, ["prod-01"]);
    assert.deepEqual(view.drawer, { open: true, serverName: "prod-01", tab: "logs", logFollow: true });
});
