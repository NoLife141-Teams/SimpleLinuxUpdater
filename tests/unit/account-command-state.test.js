const test = require('node:test');
const assert = require('node:assert/strict');
const { create } = require('../../static/js/account-command-state');
for (const operation of ['token-form', 'mfa-enroll', 'mfa-confirm', 'account-create-form', 'account-update', 'account-password-form']) {
    test(`Account admits one ${operation} command until it settles`, () => {
        const state = create(), first = state.begin(operation);
        assert.ok(first);
        assert.equal(state.begin(operation), null);
        assert.equal(state.begin('another'), null);
        assert.equal(state.current(first), true);
        state.finish(first);
        const second = state.begin(operation);
        assert.ok(second);
        assert.equal(state.current(first), false);
        state.finish(first);
        assert.equal(state.current(second), true);
    });
}
test('unacknowledged token and enrollment outcomes cannot be replaced', () => {
    const state = create();
    state.protect('token');
    assert.equal(state.begin('token-form'), null);
    state.acknowledge();
    assert.ok(state.begin('token-form'));
    const enrollment = create();
    enrollment.protect('enrollment');
    assert.equal(enrollment.begin('mfa-enroll'), null);
    assert.equal(enrollment.begin('token-form'), null);
    assert.ok(enrollment.begin('mfa-confirm'));
});
test('recovery results freeze all mutations even after acknowledgement', () => {
    const state = create(); state.protect('recovery'); state.acknowledge();
    assert.equal(state.begin('token-form'), null);
    assert.equal(state.begin('mfa-confirm'), null);
});
test('departure and acknowledgement invalidate late completion tickets', () => {
    const state = create(), ticket = state.begin('token-form');
    state.acknowledge(); assert.equal(state.current(ticket), false);
    state.destroy(); assert.equal(state.current(ticket), false);
    assert.equal(state.begin('token-form'), null);
    assert.deepEqual(Object.keys(state.view()).sort(), ['active', 'busy', 'frozen', 'secret']);
});
