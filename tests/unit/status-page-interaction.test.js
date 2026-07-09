const test = require("node:test");
const assert = require("node:assert/strict");

const { createStore } = require("../../static/js/status-page-interaction.js");

test("snapshot intake clones adapter-owned data", () => {
    const store = createStore();
    const servers = [{ name: "alpha", status: "idle", tags: ["prod"] }];
    const dashboard = { servers: [{ name: "alpha", timeline: { state: "idle" } }] };

    store.dispatch({ type: "serversSnapshotReceived", servers });
    store.dispatch({ type: "dashboardSnapshotReceived", snapshot: dashboard });
    servers[0].status = "error";
    servers[0].tags.push("mutated");
    dashboard.servers[0].timeline.state = "error";

    assert.deepEqual(store.getServer("alpha"), { name: "alpha", status: "idle", tags: ["prod"] });
    assert.deepEqual(store.getDashboardServer("alpha"), { name: "alpha", timeline: { state: "idle" } });
});

test("canonical and legacy dashboard action data produce the same action view", () => {
    const canonicalStore = createStore();
    canonicalStore.dispatch({ type: "serversSnapshotReceived", servers: [{ name: "alpha", status: "pending_approval" }] });
    canonicalStore.dispatch({
        type: "dashboardSnapshotReceived",
        snapshot: {
            servers: [{
                name: "alpha",
                actions: {
                    approve_all: {
                        enabled: true,
                        reason: "",
                        readiness: "",
                        blocking_status: ""
                    }
                }
            }]
        }
    });

    const legacyStore = createStore();
    legacyStore.dispatch({ type: "serversSnapshotReceived", servers: [{ name: "alpha", status: "pending_approval" }] });
    legacyStore.dispatch({
        type: "dashboardSnapshotReceived",
        snapshot: { servers: [{ name: "alpha", approval_triage: { can_approve_all: true } }] }
    });

    assert.deepEqual(legacyStore.getAction("alpha", "approve_all"), canonicalStore.getAction("alpha", "approve_all"));
});

test("malformed canonical actions fall back to legacy eligibility", () => {
    const store = createStore();
    store.dispatch({ type: "serversSnapshotReceived", servers: [{ name: "alpha", status: "idle" }] });
    store.dispatch({
        type: "dashboardSnapshotReceived",
        snapshot: {
            servers: [{
                name: "alpha",
                actions: { update: { enabled: "yes" } },
                approval_triage: { can_run_checks: true }
            }]
        }
    });

    assert.equal(store.getAction("alpha", "update").enabled, true);
});
