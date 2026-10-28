const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const { create } = require('../../static/js/browser-effects');
function fixture(fetch) {
    const redirects = [], lifecycle = create(); lifecycle.init();
    const window = { BrowserEffects: { create }, __nativeFetch: fetch, location: { assign: path => redirects.push(path) } };
    vm.runInNewContext(fs.readFileSync('static/js/reauthentication.js', 'utf8'), { window, DOMException });
    return { api: window.Reauthentication.create(lifecycle), lifecycle, redirects };
}
const response = (status, data) => new Response(JSON.stringify(data), { status });
for (const path of ['/api/auth/reauthenticate', '/api/auth/mfa/confirm', '/api/auth/password']) {
    test(`rejected credentials on ${path} keep a valid cookie identity`, async () => {
        const paths = [];
        const { api, redirects, lifecycle } = fixture(async url => { paths.push(url); return url === '/api/auth/status' ? response(200, { authenticated: true }) : response(401, { error: 'invalid factor' }); });
        await assert.rejects(api.request(path, { method: 'POST' }), /invalid factor/);
        assert.deepEqual(paths, [path, '/api/auth/status']);
        assert.deepEqual(redirects, []); lifecycle.destroy();
    });
}
test('a revoked cookie redirects without leaving an unresolved request promise', async () => {
    const { api, redirects, lifecycle } = fixture(async () => response(401, { error: 'authentication required' }));
    await assert.rejects(api.request('/api/auth/tokens'), { name: 'AbortError' });
    assert.deepEqual(redirects, ['/login']); lifecycle.destroy();
});
test('recent confirmation expiry remains distinct from rejected credentials', async () => {
    const { api, redirects, lifecycle } = fixture(async () => response(401, { error: 'recent confirmation required', reauthentication_required: true }));
    await assert.rejects(api.request('/api/auth/tokens'), error => error.reauthenticationRequired === true);
    assert.deepEqual(redirects, []); lifecycle.destroy();
});
test('a body arriving after page departure is rejected before secret acceptance', async () => {
    let release, reading;
    const started = new Promise(resolve => { reading = resolve; });
    const body = new Promise(resolve => { release = resolve; });
    const { api, lifecycle } = fixture(async () => ({ status: 200, ok: true, json: () => { reading(); return body; } }));
    const pending = api.request('/api/auth/tokens', { method: 'POST' });
    await started; lifecycle.destroy(); release({ secret: 'synthetic-local-only' });
    await assert.rejects(pending, { name: 'AbortError' });
});
test('a previous helper lifetime cannot open a dialog after parent reinitialization', async () => {
    const { api, lifecycle } = fixture(async () => response(200, {}));
    lifecycle.destroy(); lifecycle.init();
    assert.equal(await api.confirmIdentity(), false);
    lifecycle.destroy();
});
test('an unavailable identity check is not treated as a confirmed session expiry', async () => {
    const { api, redirects, lifecycle } = fixture(async url => url === '/api/auth/status' ? response(503, {}) : response(401, { error: 'invalid factor' }));
    await assert.rejects(api.request('/api/auth/reauthenticate'), /invalid factor/);
    assert.deepEqual(redirects, []); lifecycle.destroy();
});
test('an aborted snapshot cannot redirect when its late 401 body ignores cancellation', async () => {
    let release, reading;
    const started = new Promise(resolve => { reading = resolve; });
    const body = new Promise(resolve => { release = resolve; });
    const { api, lifecycle, redirects } = fixture(async () => ({ status: 401, ok: false, json: () => { reading(); return body; } }));
    const controller = new AbortController();
    const pending = api.request('/api/auth/sessions', { signal: controller.signal });
    await started;
    controller.abort(); release({ error: 'authentication required' });
    await assert.rejects(pending, { name: 'AbortError' });
    assert.deepEqual(redirects, []); lifecycle.destroy();
});

test('sensitive encrypted download preserves binary bytes and still decodes reauthentication errors as JSON', async () => {
    const expected = new Uint8Array([0, 255, 7, 128]);
    let sentOptions;
    const { api, lifecycle } = fixture(async (path, options) => { sentOptions = options; return new Response(expected); });
    const blob = await api.sensitive('/api/backup/archives/id/download', { method: 'POST', responseType: 'blob' }, 'Download');
    assert.deepEqual(new Uint8Array(await blob.arrayBuffer()), expected);
    assert.equal(sentOptions.responseType, undefined);
    assert.equal(sentOptions.cache, 'no-store');
    lifecycle.destroy();
    const rejected = fixture(async () => response(401, { error: 'confirmation needed', reauthentication_required: true }));
    await assert.rejects(rejected.api.request('/api/backup/archives/id/download', { method: 'POST', responseType: 'blob' }), error => error.reauthenticationRequired === true);
    rejected.lifecycle.destroy();
});
