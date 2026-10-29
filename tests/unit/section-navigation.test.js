const test = require("node:test");
const assert = require("node:assert/strict");
const navigation = require("../../static/js/section-navigation.js");
const manage = require("../../static/js/manage-page-interaction.js");
const admin = require("../../static/js/admin-page-interaction.js");
const rect = (id, top, bottom = top + 200) => ({ id, top, bottom });
const geometry = (sections, overrides = {}) => ({ sections, anchorY: 160, scrollY: 100, viewportHeight: 884, documentHeight: 3000, ...overrides });

test("same-row cards share an area regardless of height; interaction chooses the only active section", () => {
    let state = navigation.reconcile(navigation.create("global-key"), geometry([rect("global-key", 100, 300), rect("add-server", 103, 700), rect("import", 720)]));
    assert.deepEqual(state.areaSections, ["global-key", "add-server"]);
    state = navigation.select(state, "add-server");
    state = navigation.reconcile(state, geometry([rect("global-key", -150, -10), rect("add-server", -147, 400), rect("import", 420)]));
    assert.equal(state.activeSection, "add-server");
    state = navigation.reconcile(state, geometry([rect("global-key", -430), rect("add-server", -427), rect("import", 140)]));
    assert.equal(state.activeSection, "import");
});

test("visible explicit destination stays selected across a row split or an anchor just above its heading", () => {
    let state = navigation.select(navigation.create("a"), "b");
    state = navigation.reconcile(state, geometry([rect("a", -200), rect("b", 175), rect("c", 700)]));
    assert.equal(state.activeSection, "b");
    assert.deepEqual(state.areaSections, ["b"]);
    state = navigation.reconcile({ ...state, explicitSection: "" }, geometry([rect("a", -200), rect("b", 175), rect("c", 700)]));
    assert.equal(state.activeSection, "a");
});

test("ordered rows advance and return monotonically through gaps with a boundary dead band", () => {
    let state = navigation.create("a");
    const sequence = [];
    for (const offset of [0, 100, 200, 300, 400, 500, 600, 500, 400, 300, 200, 100, 0]) {
        state = navigation.reconcile(state, geometry([rect("a", 200-offset, 250-offset), rect("b", 500-offset, 550-offset), rect("c", 800-offset)]));
        sequence.push(state.activeSection);
    }
    assert.deepEqual(sequence, ["a", "a", "a", "a", "b", "b", "b", "b", "b", "a", "a", "a", "a"]);
    state = navigation.reconcile(navigation.create("a"), geometry([rect("a", -100), rect("b", 170)]));
    for (const top of [157, 163, 155, 161]) {
        state = navigation.reconcile(state, geometry([rect("a", -100), rect("b", top)]));
        assert.equal(state.activeSection, "a");
    }
});

test("document edge retains an explicit visible destination until scroll intent and tall pages keep initial selection", () => {
    const end = geometry([rect("a", -400), rect("b", 200), rect("c", 500)], { scrollY: 2116 });
    let state = navigation.reconcile(navigation.select(navigation.create("a"), "b"), end);
    assert.equal(state.activeSection, "b");
    state = navigation.reconcile({ ...state, explicitSection: "" }, end);
    assert.equal(state.activeSection, "c");
    state = navigation.reconcile(navigation.create("b"), geometry([rect("a", 200), rect("b", 500)], { viewportHeight: 2000, documentHeight: 1500, scrollY: 0 }));
    assert.equal(state.activeSection, "b");
});

for (const [name, api, ids] of [["Manage", manage, ["global-key", "add-server", "import"]], ["Admin", admin, ["app-time", "notifications", "application-checks"]]]) {
    test(`${name} interaction owns area and focus priority across layout changes without focus effects`, () => {
        const store = api.createStore();
        const receive = (sections) => store.dispatch({ type: "sectionGeometryReceived", geometry: geometry(sections) });
        receive([rect(ids[0], 100), rect(ids[1], 102), rect(ids[2], 600)]);
        store.dispatch({ type: "sectionInteracted", sectionID: ids[1] });
        let effects = receive([rect(ids[0], 90), rect(ids[1], 92), rect(ids[2], 590)]);
        assert.equal(store.getView().workspace.activeSection, ids[1]);
        assert.deepEqual(store.getView().workspace.areaSections, ids.slice(0,2));
        assert.ok(!effects.some(effect => effect.type === "focusSection"));
        receive([rect(ids[0], -300), rect(ids[1], 100), rect(ids[2], 600)]);
        assert.deepEqual(store.getView().workspace.areaSections, [ids[1]]);
        effects = receive([rect(ids[0], -900), rect(ids[1], -400), rect(ids[2], 100)]);
        assert.equal(store.getView().workspace.activeSection, ids[2]);
        assert.ok(!effects.some(effect => effect.type === "focusSection"));
        store.dispatch({ type: "sectionNavigationRequested", sectionID: ids[1] });
        effects = store.dispatch({ type: "sectionCollapseToggled", sectionID: ids[1] });
        assert.ok(effects.some(effect => effect.type === "clearSectionLocation" && effect.sectionID === ids[1]));
    });
}

test("Admin starts nearby open-section reads before selection, once, without expanding closed sections", () => {
    const store = admin.createStore();
    store.dispatch({ type: "sectionPreferencesRestored", collapsedSections: ["backup"] });
    const receive = top => store.dispatch({ type: "sectionGeometryReceived", geometry: geometry([
        rect("app-time", 100, 2000), rect("notifications", top, top + 500),
        rect("backup", 1500, 2000), rect("metrics", 4000, 4500),
    ]) });
    receive(1800);
    const effects = receive(1700);
    assert.equal(store.getView().workspace.activeSection, "app-time");
    assert.deepEqual(effects.filter(x => x.type === "loadSectionData").map(x => x.sectionID), ["notifications"]);
    assert.ok(!effects.some(x => ["focusSection", "persistSectionPreferences"].includes(x.type)));
    assert.deepEqual(store.getView().workspace.collapsedSections, ["backup"]);
    assert.ok(!receive(1650).some(x => x.type === "loadSectionData"));
});

test("Admin also warms sections above the viewport when arriving by a bottom deep link", () => {
    const store = admin.createStore();
    store.dispatch({ type: "sectionPreferencesRestored", collapsedSections: ["notifications"] });
    store.dispatch({ type: "sectionNavigationRequested", sectionID: "metrics" });
    const effects = store.dispatch({ type: "sectionGeometryReceived", geometry: geometry([
        rect("notifications", -1200, -500), rect("scheduled-runs", -2600, -1800),
        rect("backup", -1900, -1100), rect("installation", -600, -200), rect("metrics", 160, 1500),
    ]) });
    assert.equal(store.getView().workspace.activeSection, "metrics");
    assert.deepEqual(effects.filter(x => x.type === "loadSectionData").map(x => x.sectionID), ["installation"]);
    assert.deepEqual(store.getView().workspace.collapsedSections, ["notifications"]);
    assert.ok(!effects.some(x => ["focusSection", "persistSectionPreferences"].includes(x.type)));
});
