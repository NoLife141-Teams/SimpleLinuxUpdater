const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

function authContext(role, response = { status: 200 }) {
    const window = { fetch: async () => response, location: { href: '', replace() {} }, addEventListener() {} };
    const context = vm.createContext({ window, document: { body: { dataset: { accountRole: role } } } });
    vm.runInContext(fs.readFileSync('static/auth.js', 'utf8'), context);
    return window;
}
test('role presentation uses the same reader/operator/admin scope vocabulary', () => {
    const reader = authContext('reader'); const operator = authContext('operator'); const admin = authContext('admin');
    for (const user of [reader, operator, admin]) assert.equal(user.canUseScope('operational:read'), true);
    assert.equal(reader.canUseScope('maintenance:write'), false); assert.equal(reader.canUseScope('logs:read'), false);
    assert.equal(operator.canUseScope('approvals:write'), true); assert.equal(operator.canUseScope('inventory:write'), false);
    assert.equal(admin.canUseScope('settings:write'), true);
});
test('sensitive reauthentication response remains available to the page without signing out', async () => {
    const response = { status: 401, clone() { return { json: async () => ({ reauthentication_required: true }) }; } };
    const window = authContext('operator', response);
    assert.equal(await window.fetch('/api/auth/tokens'), response); assert.equal(window.location.href, '');
});
test('an expired session still redirects to login', async () => {
    const response = { status: 401, clone() { return { json: async () => ({ error: 'authentication required' }) }; } };
    const window = authContext('reader', response); void window.fetch('/api/servers');
    await new Promise(resolve => setImmediate(resolve)); assert.equal(window.location.href, '/login');
});
