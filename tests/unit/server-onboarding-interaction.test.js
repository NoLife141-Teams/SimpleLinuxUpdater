const test = require('node:test');
const assert = require('node:assert/strict');
const { createStore } = require('../../static/js/server-onboarding-interaction.js');
const { createStore: createManageStore } = require('../../static/js/manage-page-interaction.js');

function scanned(store) {
    const request = store.begin('scan');
    assert.equal(store.begin('scan'), null);
    store.finish(request, { fingerprint_sha256: 'SHA256:test' });
}

test('onboarding requires explicit identity confirmation and successful checks before saving', () => {
    const store = createStore();
    assert.equal(store.begin('create'), null);
    scanned(store);
    assert.equal(store.getView().trusted, false);
    assert.equal(store.begin('check'), null);
    store.trust(true);
    const failed = store.begin('check');
    assert.equal(store.back(), false);
    store.finish(failed, { ready: false, checks: [] });
    assert.equal(store.next(), false);
    assert.equal(store.begin('create'), null);
    const success = store.begin('check');
    assert.equal(store.getView().report, null);
    store.finish(success, { ready: true, checks: [{ status: 'warning' }] });
    assert.equal(store.next(), true);
    const save = store.begin('create');
    assert.equal(store.begin('create'), null);
    store.finish(save, { name: 'lab' });
    assert.equal(store.getView().savedName, 'lab');
});

test('editing the connection clears trust and previous checks; stale results are discarded', () => {
    const store = createStore();
    const stale = store.begin('scan');
    store.finish(stale, null, true);
    store.reset();
    scanned(store);
    assert.equal(store.finish(stale, { fingerprint_sha256: 'SHA256:stale' }), false);
    store.trust(true);
    const check = store.begin('check');
    store.finish(check, { ready: true });
    store.back();
    assert.deepEqual(store.getView(), { step: 1, busy: false, scan: null, trusted: false, report: null, savedName: '' });
});

test('draft validation does not reserve a creation command before the final confirmation', () => {
    const store = createManageStore();
    const invalid = store.dispatch({ type: 'creationValidationRequested', payload: {} });
    assert.equal(invalid[0].type, 'commandRejected');
    const valid = store.dispatch({ type: 'creationValidationRequested', payload: { name: 'lab', host: '192.0.2.24', user: 'root', port: 22, hasPassword: true } });
    assert.equal(valid[0].type, 'creationValidated');
    assert.deepEqual(store.getView().commands.inFlight, []);
});
