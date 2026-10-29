const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function mountedNavigation() {
    const location = new URL('https://example.test/manage?failure_cause=ssh_error#manage-section-audit');
    const frames = [];
    const document = { listeners: {}, documentElement: { scrollHeight: 3000 }, body: {} };
    const window = { listeners: {}, scrollY: 0, innerHeight: 900 };
    let activeSection = 'audit';
    const received = [];
    const section = (id, top) => ({
        getAttribute: () => id,
        getBoundingClientRect: () => ({ top, bottom: top + 500 }),
        closest() { return this; },
    });
    const sections = [section('global-key', 0), section('directory', 500), section('audit', 1000)];
    const header = { getBoundingClientRect: () => ({ height: 72 }) };
    const nav = {
        parentElement: { style: { setProperty() {} } },
        getBoundingClientRect: () => ({ bottom: 160, height: 80 }),
    };
    document.querySelectorAll = () => sections;
    document.querySelector = () => header;
    document.getElementById = () => nav;
    const lifecycle = {
        listen(target, type, callback) { target.listeners[type] = callback; },
        requestAnimationFrame(callback) { frames.push(callback); return frames.length; },
    };
    const interaction = {
        getView: () => ({ workspace: { activeSection } }),
        dispatch(event) {
            received.push(event);
            if (event.type === 'sectionInteracted') activeSection = event.sectionID;
            return [];
        },
    };
    const context = {
        document, window, location,
        history: { state: null, replaceState() {} },
        getComputedStyle: () => ({ position: 'sticky' }),
    };
    const source = fs.readFileSync(path.resolve(__dirname, '../../static/js/section-navigation-adapter.js'), 'utf8');
    vm.runInNewContext(source, context);
    const navigation = context.SectionNavigationAdapter.create({ lifecycle, page: 'manage', interaction, renderEffects() {} });
    navigation.init();
    const flush = () => { while (frames.length) frames.shift()(); };
    flush();
    received.length = 0;
    return { navigation, location, document, sections, received, flush };
}

test('query-only audit edits do not turn an old fragment into pending navigation', () => {
    const state = mountedNavigation();
    // Audit Clear filters removes the query via history.replaceState, without hashchange.
    state.location.search = '';
    state.document.listeners.pointerdown({ target: state.sections[0] });
    state.flush();
    assert.equal(state.received.filter(event => event.type === 'sectionGeometryReceived').length, 1);
});

test('a new native fragment remains protected until its owner acknowledges it', () => {
    const state = mountedNavigation();
    state.location.hash = '#manage-section-directory';
    state.navigation.schedule();
    state.flush();
    assert.equal(state.received.length, 0);
    state.navigation.acknowledgeLocation();
    state.navigation.schedule();
    state.flush();
    assert.equal(state.received.filter(event => event.type === 'sectionGeometryReceived').length, 1);
});
