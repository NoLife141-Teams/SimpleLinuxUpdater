const { test, expect, password, ensureAuthenticatedSession } = require('./support/fixtures');
// These scenarios never enable tracing or capture secrets in screenshots.
test.use({ trace: 'off', screenshot: 'off', video: 'off' });
test.beforeEach(async ({ page }) => { await ensureAuthenticatedSession(page); await page.goto('/account'); await expect(page.locator('#identity-summary')).toContainText('admin · admin'); });
async function delayed(page, path, method) {
    let count = 0, release;
    const gate = new Promise(resolve => { release = resolve; });
    await page.route(`**${path}`, async route => {
        if (route.request().method() !== method) return route.continue();
        count++;
        const response = await route.fetch(); // Always the real isolated backend.
        await gate;
        await route.fulfill({ response }).catch(() => {});
    });
    return { release, count: () => count };
}
async function submitTwice(page, form) {
    await page.locator(form).evaluate(el => { el.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); el.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); });
}
test('token double submission preserves one unacknowledged secret on mobile and keyboard', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    const pending = await delayed(page, '/api/auth/tokens', 'POST');
    await page.locator('#token-name').fill('single-token');
    await page.locator('#token-name').press('Enter');
    await submitTwice(page, '#token-form');
    await expect.poll(pending.count).toBe(1);
    await expect(page.locator('#token-form')).toHaveAttribute('aria-busy', 'true');
    await expect(page.getByRole('button', { name: 'Create personal token' })).toBeDisabled();
    pending.release();
    await expect(page.locator('#token-result')).toBeVisible();
    const first = await page.locator('#token-secret').textContent();
    await submitTwice(page, '#token-form');
    expect(pending.count()).toBe(1);
    expect((await page.locator('#token-secret').textContent()) === first).toBe(true);
    await expect(page.locator('#token-dismiss')).toBeFocused();
    await page.keyboard.press('Enter');
    await expect(page.locator('#token-secret')).toBeEmpty();
    const count = await page.evaluate(async () => (await (await window.__nativeFetch('/api/auth/tokens')).json()).tokens.filter(t => t.name === 'single-token').length);
    expect(count).toBe(1);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});
test('account creation and password admit one command under delayed responses', async ({ page }) => {
    const account = await delayed(page, '/api/auth/accounts', 'POST');
    await page.locator('#new-account-name').fill('reader');
    await page.locator('#new-account-password').fill(password);
    await submitTwice(page, '#account-create-form');
    await expect.poll(account.count).toBe(1);
    await expect(page.locator('#account-create-form')).toHaveAttribute('aria-busy', 'true');
    account.release();
    await expect(page.locator('#account-list')).toContainText('reader');
    const change = await delayed(page, '/api/auth/password', 'PUT');
    await page.locator('#account-current-password').fill(password);
    await page.locator('#account-new-password').fill('ChangedSynthetic123');
    await page.locator('#account-confirm-password').fill('ChangedSynthetic123');
    await submitTwice(page, '#account-password-form');
    await expect.poll(change.count).toBe(1);
    await expect(page.locator('#account-password-form')).toHaveAttribute('aria-busy', 'true');
    change.release();
    await expect(page.locator('#account-status')).toContainText('Password changed');
    await expect(page.locator('#account-current-password')).toHaveValue('');
});
test('bad password stays in context, genuine revoked session returns to login', async ({ page, context }) => {
    await page.locator('#reauth-password').fill('WrongSynthetic123');
    await page.locator('#reauth-password').press('Enter');
    await expect(page.locator('#account-status')).toContainText('invalid credentials');
    await expect(page).toHaveURL(/\/account$/);
    await expect(page.locator('#reauth-password')).toHaveValue('');
    const other = await context.newPage();
    await other.goto('/account');
    await other.evaluate(() => window.__nativeFetch('/api/auth/logout', { method: 'POST' }));
    await page.locator('#reauth-password').fill(password);
    await page.locator('#reauth-password').press('Enter');
    await expect(page).toHaveURL(/\/login$/);
    await other.close();
});
test('departure rejects a delayed token body and back navigation cannot resurrect it', async ({ page }) => {
    const pending = await delayed(page, '/api/auth/tokens', 'POST');
    await page.locator('#token-name').fill('departure-token');
    await page.locator('#token-name').press('Enter');
    await expect.poll(pending.count).toBe(1);
    await page.goto('/');
    pending.release();
    await page.goBack();
    await expect(page.locator('#identity-summary')).toContainText('admin · admin');
    await expect(page.locator('#token-secret')).toBeEmpty();
    await expect(page.locator('#token-result')).toBeHidden();
});
test('expired recent confirmation keeps draft and requires an explicit single resumption', async ({ page }) => {
    expect((await page.request.post('/__test/expire-reauthentication')).status()).toBe(204);
    let posts = 0;
    page.on('request', request => { if (request.url().endsWith('/api/auth/tokens') && request.method() === 'POST') posts++; });
    await page.locator('#token-name').fill('resume-once');
    await page.locator('#token-name').press('Enter');
    const dialog = page.locator('.reauthentication-dialog');
    await expect(dialog).toBeVisible();
    await expect(page.locator('#reauthentication-password')).toBeFocused();
    await page.locator('#reauthentication-password').fill('WrongSynthetic123');
    await page.locator('#reauthentication-password').press('Enter');
    await expect(page.locator('#reauthentication-error')).toContainText('invalid credentials');
    await expect(page.locator('#reauthentication-password')).toHaveValue('');
    expect(posts).toBe(1);
    await page.locator('#reauthentication-password').fill(password);
    await page.locator('#reauthentication-password').press('Enter');
    await expect(dialog).toHaveCount(0);
    await expect(page.locator('#action-confirm-modal')).toHaveClass(/active/);
    expect(posts).toBe(1);
    await page.locator('#action-confirm-submit').click();
    await expect(page.locator('#token-result')).toBeVisible();
    expect(posts).toBe(2); // One denied request, exactly one confirmed resubmission.
    await page.locator('#token-dismiss').click();
    await expect(page.locator('#token-list')).toContainText('resume-once');
    expect((await page.request.get('/api/auth/tokens')).ok()).toBe(true);
});
test('cancelling reauthentication clears credentials and preserves a useful draft without replay', async ({ page }) => {
    expect((await page.request.post('/__test/expire-reauthentication')).status()).toBe(204);
    await page.locator('#new-account-name').fill('preserved-reader');
    await page.locator('#new-account-password').fill(password);
    await page.locator('#new-account-password').press('Enter');
    await expect(page.locator('.reauthentication-dialog')).toBeVisible();
    await page.locator('#reauthentication-password').fill(password);
    await page.keyboard.press('Escape');
    await expect(page.locator('.reauthentication-dialog')).toHaveCount(0);
    await expect(page.locator('#new-account-name')).toHaveValue('preserved-reader');
    await expect(page.locator('#new-account-password')).toHaveValue('');
    await expect(page.locator('#account-create-form')).toHaveAttribute('aria-busy', 'false');
    const result = await page.request.get('/api/auth/accounts');
    expect((await result.json()).accounts.some(account => account.username === 'preserved-reader')).toBe(false);
});
function totp(secret) {
    const crypto = require('node:crypto');
    const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
    const bits = [...secret].map(char => alphabet.indexOf(char).toString(2).padStart(5, '0')).join('');
    const key = Buffer.from(bits.match(/.{8}/g).map(byte => parseInt(byte, 2)));
    const counter = Buffer.alloc(8); counter.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)));
    const hash = crypto.createHmac('sha1', key).update(counter).digest();
    return String((hash.readUInt32BE(hash[19] & 15) & 0x7fffffff) % 1000000).padStart(6, '0');
}
test('MFA admission, rejected enrollment factor and recovery survive deliberate session revocation', async ({ page }) => {
    const enroll = await delayed(page, '/api/auth/mfa/enroll', 'POST');
    await page.locator('#mfa-enroll').evaluate(el => { el.click(); el.dispatchEvent(new MouseEvent('click', { bubbles: true })); });
    await expect.poll(enroll.count).toBe(1);
    await expect(page.locator('#mfa-enroll')).toBeDisabled();
    enroll.release();
    await expect(page.locator('#mfa-enrollment')).toBeVisible();
    const secret = await page.locator('#mfa-secret').textContent();
    await expect(page.locator('#mfa-enroll')).toBeDisabled();
    const code = totp(secret);
    await page.locator('#mfa-confirm-code').fill(code === '000000' ? '111111' : '000000');
    await page.locator('#mfa-confirm-code').press('Enter');
    await expect(page.locator('#account-status')).toContainText('invalid or expired enrollment code');
    await expect(page).toHaveURL(/\/account$/);
    const confirm = await delayed(page, '/api/auth/mfa/confirm', 'POST');
    await page.locator('#mfa-confirm-code').fill(totp(secret));
    await submitTwice(page, '#mfa-confirm-form');
    await expect.poll(confirm.count).toBe(1);
    confirm.release();
    await expect(page.locator('#recovery-result')).toBeVisible();
    expect(await page.locator('#recovery-codes').evaluate(el => el.textContent.trim().split('\n').length === 10)).toBe(true);
    await expect(page.locator('#mfa-secret')).toBeEmpty();
    await expect(page.locator('#recovery-dismiss')).toBeFocused();
    await expect(page.getByRole('button', { name: 'Create personal token' })).toBeDisabled();
    await submitTwice(page, '#token-form');
    expect((await page.request.get('/api/auth/identity')).status()).toBe(401);
    await expect(page).toHaveURL(/\/account$/);
    // A real unauthenticated response elsewhere must not trigger a refresh here.
    expect(await page.locator('#recovery-codes').evaluate(el => el.textContent.trim().split('\n').length === 10)).toBe(true);
    await page.locator('#recovery-dismiss').press('Enter');
    await expect(page).toHaveURL(/\/login$/);
});
test('account changes are confirmed, uniquely admitted, and self changes return to login', async ({ page }) => {
    expect((await page.request.post('/api/auth/accounts', { headers: { Origin: new URL(page.url()).origin }, data: { username: 'second-admin', password, role: 'admin' } })).status()).toBe(201);
    await page.reload();
    const current = page.locator('#account-list li').filter({ has: page.getByRole('combobox', { name: 'Role for admin', exact: true }) });
    await current.getByRole('combobox').selectOption('reader');
    await current.getByRole('button', { name: 'Save role' }).click();
    await expect(page.locator('#action-confirm-message')).toContainText('sessions');
    await page.keyboard.press('Escape');
    await expect(page).toHaveURL(/\/account$/);
    const update = await delayed(page, '/api/auth/accounts/1', 'PUT');
    await current.getByRole('button', { name: 'Save role' }).click();
    await page.locator('#action-confirm-submit').click();
    await expect.poll(update.count).toBe(1);
    await expect(current.getByRole('button', { name: 'Save role' })).toBeDisabled();
    await current.getByRole('button', { name: 'Save role' }).evaluate(el => el.dispatchEvent(new MouseEvent('click', { bubbles: true })));
    expect(update.count()).toBe(1);
    update.release();
    await expect(page).toHaveURL(/\/login$/);
});
test('Admin restore offers contextual reauthentication and one explicit resumption on the real backend', async ({ page }) => {
    const passphrase = 'DisposableBackupPassphrase123';
    const exported = await page.request.post('/api/backup/export', { headers: { Origin: new URL(page.url()).origin }, data: { passphrase, include_known_hosts: true } });
    expect(exported.status()).toBe(200);
    const archive = await exported.body();
    await page.goto('/admin');
    await page.locator('[data-admin-section-link="backup"]').click();
    await page.locator('#backup-restore-file').setInputFiles({ name: 'disposable.slubkp', mimeType: 'application/octet-stream', buffer: archive });
    await page.locator('#backup-restore-passphrase').fill(passphrase);
    await page.locator('#backup-verify-btn').click();
    await expect(page.locator('#backup-review-readiness')).toHaveText('Ready for confirmation');
    expect((await page.request.post('/__test/expire-reauthentication')).status()).toBe(204);
    let attempts = 0;
    page.on('request', request => { if (request.url().endsWith('/api/backup/restore')) attempts++; });
    await page.locator('#backup-restore-btn').click();
    await page.locator('#typed-confirm-input').fill('RESTORE');
    await page.locator('#typed-confirm-submit').click();
    await expect(page.locator('.reauthentication-dialog')).toBeVisible();
    expect(attempts).toBe(1);
    await page.locator('#reauthentication-password').fill(password);
    await page.locator('#reauthentication-password').press('Enter');
    await expect(page.locator('#action-confirm-title')).toHaveText('Resume sensitive action');
    expect(attempts).toBe(1);
    await page.locator('#action-confirm-submit').click();
    await expect(page).toHaveURL(/\/login$/);
    expect(attempts).toBe(2);
});
test('late JSON cannot redisplay a token after lifecycle destruction and bfcache resumes cleanly', async ({ page }) => {
    await page.evaluate(() => {
        const original = window.__nativeFetch;
        let release;
        const gate = new Promise(resolve => { release = resolve; });
        window.releaseAccountBody = release;
        window.__nativeFetch = async (...args) => {
            const response = await original(...args);
            if (args[0] === '/api/auth/tokens' && args[1]?.method === 'POST') {
                const json = response.json.bind(response);
                response.json = async () => { window.accountBodyReading = true; await gate; return json(); };
            }
            return response;
        };
    });
    await page.locator('#token-name').fill('late-body');
    await page.locator('#token-name').press('Enter');
    await expect.poll(() => page.evaluate(() => Boolean(window.accountBodyReading))).toBe(true);
    await page.evaluate(() => {
        window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true }));
        window.releaseAccountBody();
    });
    await expect(page.locator('#token-secret')).toBeEmpty();
    await expect(page.locator('#token-result')).toBeHidden();
    const reloaded = page.waitForEvent('domcontentloaded');
    await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true })));
    await reloaded;
    await expect(page.locator('#identity-summary')).toContainText('admin · admin');
    await expect(page.locator('#token-result')).toBeHidden();
});
test('a second recent-confirmation expiry stops after the one explicitly approved attempt', async ({ page }) => {
    await page.request.post('/__test/expire-reauthentication');
    let attempts = 0;
    page.on('request', request => { if (request.url().endsWith('/api/auth/tokens') && request.method() === 'POST') attempts++; });
    await page.locator('#token-name').fill('expire-again');
    await page.locator('#token-name').press('Enter');
    await page.locator('#reauthentication-password').fill(password);
    await page.locator('#reauthentication-password').press('Enter');
    await expect(page.locator('#action-confirm-title')).toHaveText('Resume sensitive action');
    await page.request.post('/__test/expire-reauthentication');
    await page.locator('#action-confirm-submit').click();
    await expect(page.locator('#account-status')).toContainText('reauthentication required');
    await expect(page.locator('#token-form')).toHaveAttribute('aria-busy', 'false');
    expect(attempts).toBe(2);
    await expect(page.locator('.reauthentication-dialog')).toHaveCount(0);
    const tokens = await (await page.request.get('/api/auth/tokens')).json();
    expect(tokens.tokens.some(token => token.name === 'expire-again')).toBe(false);
});
test('cancelling contextual Admin restore clears passphrases and retains only the selected archive', async ({ page }) => {
    const passphrase = 'DisposableBackupPassphrase123';
    const exported = await page.request.post('/api/backup/export', { headers: { Origin: new URL(page.url()).origin }, data: { passphrase, include_known_hosts: true } });
    expect(exported.status()).toBe(200);
    const archive = await exported.body();
    await page.goto('/admin');
    await page.locator('[data-admin-section-link="backup"]').click();
    await page.locator('#backup-restore-file').setInputFiles({ name: 'disposable.slubkp', mimeType: 'application/octet-stream', buffer: archive });
    await page.locator('#backup-restore-passphrase').fill(passphrase);
    await page.locator('#backup-verify-btn').click();
    await expect(page.locator('#backup-review-readiness')).toHaveText('Ready for confirmation');
    await page.request.post('/__test/expire-reauthentication');
    let attempts = 0;
    page.on('request', request => { if (request.url().endsWith('/api/backup/restore')) attempts++; });
    await page.locator('#backup-restore-btn').click();
    await page.locator('#typed-confirm-input').fill('RESTORE');
    await page.locator('#typed-confirm-submit').click();
    await expect(page.locator('.reauthentication-dialog')).toBeVisible();
    expect(await page.locator('#backup-restore-file').evaluate(el => el.files.length)).toBe(1);
    expect(await page.locator('#backup-restore-passphrase').evaluate(el => Boolean(el.value))).toBe(true);
    await page.locator('#reauthentication-password').fill(password);
    await page.keyboard.press('Escape');
    await expect(page.locator('.reauthentication-dialog')).toHaveCount(0);
    await expect(page.locator('#backup-restore-passphrase')).toHaveValue('');
    expect(await page.locator('#backup-restore-file').evaluate(el => el.files.length)).toBe(1);
    expect(attempts).toBe(1);
    await expect(page.locator('#backup-review-readiness')).toHaveText('Ready for confirmation');
    await expect(page.locator('#backup-restore-btn')).toBeDisabled();
});
test('Admin remount and cached navigation cancel restore identity dialogs and erase backup credentials', async ({ page }) => {
    const passphrase = 'DisposableBackupPassphrase123';
    const exported = await page.request.post('/api/backup/export', { headers: { Origin: new URL(page.url()).origin }, data: { passphrase, include_known_hosts: true } });
    expect(exported.status()).toBe(200);
    const archive = await exported.body();
    await page.goto('/admin');
    await page.locator('[data-admin-section-link="backup"]').click();
    let attempts = 0;
    page.on('request', request => { if (request.url().endsWith('/api/backup/restore')) attempts++; });
    for (const transition of ['remount', 'cached-navigation']) {
        await page.locator('#backup-restore-file').setInputFiles({ name: 'disposable.slubkp', mimeType: 'application/octet-stream', buffer: archive });
        await page.locator('#backup-restore-passphrase').fill(passphrase);
        await page.locator('#backup-verify-btn').click();
        await expect(page.locator('#backup-review-readiness')).toHaveText('Ready for confirmation');
        await page.request.post('/__test/expire-reauthentication');
        await page.locator('#backup-restore-btn').click();
        await page.locator('#typed-confirm-input').fill('RESTORE');
        await page.locator('#typed-confirm-submit').click();
        await expect(page.locator('.reauthentication-dialog')).toBeVisible();
        await page.locator('#reauthentication-password').fill(password);
        await page.evaluate(transition => {
            if (transition === 'remount') window.AdminPage.mount();
            else {
                window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true }));
                window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true }));
            }
        }, transition);
        await expect(page.locator('.reauthentication-dialog')).toHaveCount(0);
        await expect(page.locator('#backup-restore-passphrase')).toHaveValue('');
        await expect(page.locator('#backup-restore-btn')).toBeDisabled();
    }
    expect(attempts).toBe(2); // Both were denied by real recent-auth admission.
    await page.locator('#backup-restore-passphrase').fill(passphrase);
    await expect(page.locator('#backup-verify-btn')).toBeEnabled();
});
test('obsolete initial session reads cannot dismiss unacknowledged MFA recovery codes', async ({ page }) => {
    let release, completed;
    const gate = new Promise(resolve => { release = resolve; });
    const finished = new Promise(resolve => { completed = resolve; });
    await page.route('**/api/auth/sessions', async route => {
        await gate;
        try {
            const response = await route.fetch();
            expect(response.status()).toBe(401);
            await route.fulfill({ response }).catch(() => {});
        } finally { completed(); }
    });
    await page.reload();
    await page.locator('#mfa-enroll').click();
    await expect(page.locator('#mfa-enrollment')).toBeVisible();
    const secret = await page.locator('#mfa-secret').textContent();
    await page.locator('#mfa-confirm-code').fill(totp(secret));
    await page.locator('#mfa-confirm-code').press('Enter');
    await expect(page.locator('#recovery-result')).toBeVisible();
    release(); await finished;
    expect((await page.request.get('/api/auth/identity')).status()).toBe(401);
    await expect(page.locator('#recovery-result')).toBeVisible();
    await expect(page).toHaveURL(/\/account$/);
    await page.locator('#recovery-dismiss').click();
    await expect(page).toHaveURL(/\/login$/);
});
test('Account waits for identity initialization before admitting a session-revoking MFA command', async ({ page }) => {
    let release, reads = 0, mutations = 0;
    const gate = new Promise(resolve => { release = resolve; });
    await page.route('**/api/auth/identity', async route => {
        reads++;
        await gate;
        const response = await route.fetch();
        expect(response.status()).toBe(200);
        await route.fulfill({ response });
    });
    page.on('request', request => { if (request.url().endsWith('/api/auth/mfa/enroll')) mutations++; });
    await page.reload();
    await expect.poll(() => reads).toBe(1);
    await expect(page.locator('#mfa-enroll')).toBeDisabled();
    await page.locator('#mfa-enroll').evaluate(el => el.dispatchEvent(new MouseEvent('click', { bubbles: true })));
    expect(mutations).toBe(0);
    release();
    await expect(page.locator('#identity-summary')).toContainText('admin · admin');
    await page.locator('#mfa-enroll').click();
    await expect(page.locator('#mfa-enrollment')).toBeVisible();
    const secret = await page.locator('#mfa-secret').textContent();
    await page.locator('#mfa-confirm-code').fill(totp(secret));
    await page.locator('#mfa-confirm-code').press('Enter');
    await expect(page.locator('#recovery-result')).toBeVisible();
    expect((await page.request.get('/api/auth/identity')).status()).toBe(401);
    await expect(page).toHaveURL(/\/account$/);
    expect(reads).toBe(1);
    await page.locator('#recovery-dismiss').click();
    await expect(page).toHaveURL(/\/login$/);
});
for (const outcome of ['confirmed', 'rejected', 'enrollment-cancelled', 'token-acknowledged']) {
    test(`interrupted initial lists resume after ${outcome} without reading over protected secrets`, async ({ page }) => {
        let release, reads = 0;
        const gate = new Promise(resolve => { release = resolve; });
        await page.route(/\/api\/auth\/(tokens|sessions|accounts)$/, async route => {
            if (route.request().method() !== 'GET') return route.continue();
            reads++;
            const response = await route.fetch();
            await gate;
            await route.fulfill({ response }).catch(() => {});
        });
        await page.reload();
        await expect(page.locator('#identity-summary')).toContainText('admin · admin');
        await expect.poll(() => reads).toBe(3);
        if (outcome === 'enrollment-cancelled') {
            await page.locator('#mfa-enroll').click();
            await expect(page.locator('#mfa-enrollment')).toBeVisible();
            expect(reads).toBe(3);
            await page.locator('#mfa-cancel').click();
        } else if (outcome === 'token-acknowledged') {
            await page.locator('#token-name').fill('interrupted-token');
            await page.locator('#token-name').press('Enter');
            await expect(page.locator('#token-result')).toBeVisible();
            expect(reads).toBe(3);
            await page.locator('#token-dismiss').click();
        } else {
            await page.locator('#reauth-password').fill(outcome === 'confirmed' ? password : 'WrongSynthetic123');
            await page.locator('#reauth-password').press('Enter');
            await expect(page.locator('#account-status')).toContainText(outcome === 'confirmed' ? 'Identity confirmed' : 'invalid credentials');
        }
        release();
        await expect(page.locator('#account-list li')).toHaveCount(1);
        await expect(page.locator('#session-list li')).toHaveCount(1);
        await expect(page.locator('#account-list').getByRole('button', { name: 'Save role' })).toBeEnabled();
        await expect.poll(() => reads).toBe(6);
        if (outcome === 'token-acknowledged') await expect(page.locator('#token-list')).toContainText('interrupted-token');
    });
}
test('cached Admin navigation cancels the explicit restore resumption and unlocks the page', async ({ page }) => {
    const passphrase = 'DisposableBackupPassphrase123';
    const exported = await page.request.post('/api/backup/export', { headers: { Origin: new URL(page.url()).origin }, data: { passphrase, include_known_hosts: true } });
    expect(exported.status()).toBe(200);
    const archive = await exported.body();
    await page.goto('/admin');
    await page.locator('[data-admin-section-link="backup"]').click();
    await page.locator('#backup-restore-file').setInputFiles({ name: 'disposable.slubkp', mimeType: 'application/octet-stream', buffer: archive });
    await page.locator('#backup-restore-passphrase').fill(passphrase);
    await page.locator('#backup-verify-btn').click();
    await expect(page.locator('#backup-review-readiness')).toHaveText('Ready for confirmation');
    await page.request.post('/__test/expire-reauthentication');
    let attempts = 0;
    page.on('request', request => { if (request.url().endsWith('/api/backup/restore')) attempts++; });
    await page.locator('#backup-restore-btn').click();
    await page.locator('#typed-confirm-input').fill('RESTORE');
    await page.locator('#typed-confirm-submit').click();
    await page.locator('#reauthentication-password').fill(password);
    await page.locator('#reauthentication-password').press('Enter');
    await expect(page.locator('#action-confirm-title')).toHaveText('Resume sensitive action');
    await page.evaluate(() => {
        window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true }));
        window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true }));
    });
    await expect(page.locator('#action-confirm-modal')).not.toHaveClass(/active/);
    expect(await page.locator('.dashboard-shell').evaluate(el => el.inert)).toBe(false);
    await expect(page.locator('#backup-restore-passphrase')).toHaveValue('');
    await expect(page.locator('#backup-restore-btn')).toBeDisabled();
    expect(attempts).toBe(1);
    await page.locator('#backup-restore-passphrase').fill(passphrase);
    await expect(page.locator('#backup-verify-btn')).toBeEnabled();
});
