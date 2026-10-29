const { test, expect, password, ensureAuthenticatedSession } = require('./support/fixtures');
const { fulfillJson } = require('./support/builders');
const fs = require('node:fs');
const path = require('node:path');
// Only synthetic, non-secret facts are captured explicitly below. Sensitive
// commands never enable automatic screenshots, traces or videos.
test.use({ trace: 'off', screenshot: 'off', video: 'off', locale: 'en-US', timezoneId: 'Europe/Paris' });
const sampleToken = { id: 'ui-token', name: 'UI metadata fixture', scopes: ['operational:read'], expires_at: '2026-11-01T05:30:00Z', revoked: false, last_used_at: '', last_used_origin: '' };
const sampleSession = { id: 1, current: true, client_label: 'Synthetic UI browser', client_ip: '192.0.2.x', expires_at: '2026-11-01T06:30:00Z' };
async function facts(page, options = {}) {
    await ensureAuthenticatedSession(page);
    await page.route('**/api/app-settings/timezone', route => fulfillJson(route, { timezone: 'America/Toronto', resolved_timezone: 'America/Toronto', editable_timezone: 'America/Toronto' }));
    for (const [name, value] of Object.entries({ tokens: options.tokens || [sampleToken], sessions: options.sessions || [sampleSession], accounts: options.accounts || [
        { id: 1, username: 'admin', role: 'admin', disabled: false, mfa_enabled: false },
        { id: 2, username: 'disabled-fixture', role: 'reader', disabled: true, mfa_enabled: false },
    ] })) {
        await page.route(`**/api/auth/${name}`, route => route.request().method() === 'GET' ? fulfillJson(route, { [name]: value }) : route.continue());
    }
}
async function ready(page) {
    await page.goto('/account');
    await expect(page.locator('#identity-summary')).toContainText('admin · admin');
}
async function capture(page, testInfo, name) {
    const image = await page.screenshot({ fullPage: false });
    await testInfo.attach(name, { body: image, contentType: 'image/png' });
    if (process.env.SLU_CAPTURE_UI_516) {
        const directory = path.resolve('.github/assets/account-ui-516');
        fs.mkdirSync(directory, { recursive: true });
        fs.writeFileSync(path.join(directory, `${name}.png`), image);
    }
}
for (const width of [1284, 390]) {
    test(`password mismatch remains local and does not send a mutation at ${width}px`, async ({ page }, testInfo) => {
        await page.setViewportSize({ width, height: 884 });
        await facts(page); await ready(page);
        let mutations = 0;
        page.on('request', request => { if (request.method() === 'PUT' && new URL(request.url()).pathname === '/api/auth/password') mutations++; });
        await page.locator('#account-current-password').fill(password);
        await page.locator('#account-new-password').fill('DifferentSynthetic123');
        await page.locator('#account-confirm-password').fill('AnotherSynthetic123');
        await page.locator('#account-password-form button').click();
        const feedback = page.locator('#password-feedback');
        await expect(feedback).toContainText('do not match');
        await expect(feedback).toHaveAttribute('role', 'alert');
        await expect(feedback).toBeFocused();
        expect(mutations).toBe(0);
        await expect(page.locator('#account-status')).not.toBeFocused();
        await expect(page.locator('#account-current-password')).toHaveValue('');
        await expect(page.locator('#account-new-password')).toHaveValue('');
        await expect(page.locator('#account-confirm-password')).toHaveValue('');
        expect(await page.evaluate(() => scrollY)).toBeGreaterThan(200);
        await capture(page, testInfo, `password-error-${width}`);
        await feedback.getByRole('link').click();
        await expect(page.locator('#account-confirm-password')).toBeFocused();
        const field = await page.locator('#account-confirm-password').boundingBox();
        expect(field.y).toBeGreaterThan(0);
        expect(field.y + field.height).toBeLessThan(884);
    });
}
test('Account lists distinguish pending, error and recovery independently while preserving form drafts', async ({ page }, testInfo) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await facts(page);
    let release, failure = true;
    const pending = new Promise(resolve => { release = resolve; });
    await page.route('**/api/auth/tokens', async route => {
        if (route.request().method() !== 'GET') return route.continue();
        await pending;
        await fulfillJson(route, failure ? { error: 'Temporary token read failure' } : { tokens: [sampleToken] }, failure ? 503 : 200);
    });
    try {
        await ready(page);
        await expect(page.locator('#tokens-state')).toHaveAttribute('data-state', 'loading');
        await expect(page.locator('#token-list')).toHaveAttribute('aria-busy', 'true');
        await expect(page.locator('#sessions-state')).toHaveAttribute('data-state', 'current');
        await expect(page.locator('#accounts-state')).toHaveAttribute('data-state', 'current');
        await page.locator('#token-name').fill('Useful unsaved draft');
        await expect(page.locator('#token-form button')).toBeEnabled();
        await page.locator('#tokens-state').scrollIntoViewIfNeeded();
        await capture(page, testInfo, 'tokens-loading-390');
        release();
        await expect(page.locator('#tokens-state')).toHaveAttribute('data-state', 'failed');
        await expect(page.locator('#tokens-status')).toContainText('Could not load personal tokens');
        await expect(page.locator('#token-list')).toHaveAttribute('aria-busy', 'false');
        await capture(page, testInfo, 'tokens-error-390');
        failure = false;
        await page.locator('[data-account-list-retry="tokens"]').click();
        await expect(page.locator('#tokens-state')).toHaveAttribute('data-state', 'current');
        await expect(page.locator('#token-list')).toContainText(sampleToken.name);
        await expect(page.locator('#token-name')).toHaveValue('Useful unsaved draft');
        await expect(page.locator('#sessions-state')).toHaveAttribute('data-state', 'current');
    } finally { release(); }
});
test('empty Account lists are explicit valid states', async ({ page }, testInfo) => {
    await facts(page, { tokens: [], sessions: [], accounts: [] }); await ready(page);
    for (const name of ['tokens', 'sessions', 'accounts']) {
        await expect(page.locator(`#${name}-state`)).toHaveAttribute('data-state', 'empty');
        await expect(page.locator(`#${name}-status`)).toContainText('No ');
        await expect(page.locator(`[data-account-list-retry="${name}"]`)).toBeHidden();
    }
    await page.locator('#tokens-state').scrollIntoViewIfNeeded();
    await capture(page, testInfo, 'empty-lists-1284');
});
test('a failed refresh retains accepted rows after an admitted revoke', async ({ page }) => {
    await facts(page); await ready(page);
    await expect(page.locator('#token-list')).toContainText(sampleToken.name);
    await page.route('**/api/auth/tokens/ui-token', route => fulfillJson(route, { message: 'revoked' }));
    await page.route('**/api/auth/tokens', route => fulfillJson(route, { error: 'Refresh unavailable' }, 503));
    await page.locator('#token-list').getByRole('button', { name: 'Revoke', exact: true }).click();
    await page.locator('#action-confirm-submit').click();
    await expect(page.locator('#tokens-state')).toHaveAttribute('data-state', 'failed');
    await expect(page.locator('#tokens-status')).toContainText('Previous results remain visible');
    await expect(page.locator('#token-list')).toContainText(sampleToken.name);
    await expect(page.locator('#token-feedback')).toContainText('Token revoked');
});
test('actions follow the primary/danger conventions and identity confirmation stays explicit', async ({ page }, testInfo) => {
    await facts(page); await ready(page);
    for (const form of ['reauth-form', 'token-form', 'account-password-form', 'account-create-form']) {
        await expect(page.locator(`#${form} button[type=submit]`)).toHaveClass(/primary-action/);
    }
    await expect(page.locator('#mfa-disable')).toHaveClass(/btn-danger/);
    await expect(page.locator('#token-list').getByRole('button', { name: 'Revoke', exact: true })).toHaveClass(/btn-danger/);
    await expect(page.locator('#session-list').getByRole('button', { name: 'Revoke session' })).toHaveClass(/btn-danger/);
    await expect(page.locator('#account-list').getByRole('button', { name: 'Disable', exact: true })).toHaveClass(/btn-danger/);
    await expect(page.locator('#account-list').getByRole('button', { name: 'Enable', exact: true })).not.toHaveClass(/btn-danger/);
    expect((await page.request.post('/__test/expire-reauthentication')).status()).toBe(204);
    await page.locator('#token-name').fill('Dialog UI fixture');
    await page.locator('#token-form button[type=submit]').click();
    await expect(page.locator('.reauthentication-dialog')).toBeVisible();
    await expect(page.locator('.reauthentication-dialog button[type=submit]')).toHaveClass(/primary-action/);
    await expect(page.locator('#reauthentication-password')).toHaveValue('');
    await capture(page, testInfo, 'reauthentication-1284');
    await page.locator('.reauthentication-dialog [data-cancel]').click();
    await expect(page.locator('#token-name')).toHaveValue('Dialog UI fixture');
});
test('Account dates preserve UTC instants and show application-zone DST independently of browser entry', async ({ page }, testInfo) => {
    const instants = ['2026-11-01T05:30:00Z', '2026-11-01T06:30:00Z', '2026-03-08T06:59:00Z', '2026-03-08T07:01:00Z'];
    await facts(page, { tokens: [...instants.map((expires_at, i) => ({ ...sampleToken, id: `dst-${i}`, name: `DST fixture ${i}`, expires_at, revoked: true })), { ...sampleToken, id: 'missing', name: 'Missing date fixture', expires_at: '' }, { ...sampleToken, id: 'invalid', name: 'Invalid date fixture', expires_at: 'invalid' }] });
    await ready(page);
    await expect(page.locator('#account-timezone')).toContainText('America/Toronto');
    for (let i = 0; i < instants.length; i++) {
        const time = page.locator('#token-list li').filter({ hasText: `DST fixture ${i}` }).locator('time');
        await expect(time).toHaveAttribute('datetime', new Date(instants[i]).toISOString());
        await expect(time).toHaveAttribute('title', new RegExp(instants[i]));
    }
    const times = page.locator('#token-list time');
    await expect(times.nth(0)).toContainText('01:30');
    await expect(times.nth(0)).toContainText('EDT');
    await expect(times.nth(1)).toContainText('01:30');
    await expect(times.nth(1)).toContainText('EST');
    await expect(times.nth(2)).toContainText('01:59');
    await expect(times.nth(3)).toContainText('03:01');
    await expect(page.locator('#token-list li').filter({ hasText: 'Missing date fixture' })).toContainText('Unavailable');
    await expect(page.locator('#token-list li').filter({ hasText: 'Invalid date fixture' })).toContainText('Unavailable');
    await expect(page.locator('label[for="token-expiry"]')).toContainText('your browser timezone');
    await page.locator('#token-list').scrollIntoViewIfNeeded();
    await capture(page, testInfo, 'dates-1284');
});
test('Retry and delayed timezone reads cannot read over a protected one-time token', async ({ page }) => {
    await facts(page);
    let tokenReads = 0, failure = true, releaseTimezone;
    const timezone = new Promise(resolve => { releaseTimezone = resolve; });
    await page.route('**/api/app-settings/timezone', async route => { await timezone; await fulfillJson(route, { timezone: 'America/Toronto', resolved_timezone: 'America/Toronto' }); });
    await page.route('**/api/auth/tokens', route => {
        if (route.request().method() !== 'GET') return route.continue();
        tokenReads++;
        return fulfillJson(route, failure ? { error: 'Read unavailable' } : { tokens: [] }, failure ? 503 : 200);
    });
    try {
        await ready(page);
        await expect(page.locator('#tokens-state')).toHaveAttribute('data-state', 'failed');
        await page.locator('#token-name').fill('Protected UI fixture');
        await page.locator('#token-form button[type=submit]').click();
        await expect(page.locator('#token-result')).toBeVisible();
        expect(await page.locator('#token-secret').evaluate(el => el.textContent.length > 0)).toBe(true);
        await expect(page.locator('[data-account-list-retry="tokens"]')).toBeDisabled();
        await page.locator('[data-account-list-retry="tokens"]').dispatchEvent('click');
        releaseTimezone();
        await expect(page.locator('#account-timezone')).toContainText('America/Toronto');
        expect(tokenReads).toBe(1);
        expect(await page.locator('#token-secret').evaluate(el => el.textContent.length > 0)).toBe(true);
        failure = false;
        await page.locator('#token-dismiss').click();
        await expect(page.locator('#tokens-state')).toHaveAttribute('data-state', 'empty');
        expect(tokenReads).toBe(2);
        expect(await page.locator('#token-secret').evaluate(el => el.textContent.length)).toBe(0);
    } finally { releaseTimezone(); }
});
