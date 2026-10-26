const { test, expect } = require('@playwright/test');
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const crypto = require('node:crypto');

// Each identity scenario owns a separate process, database, config, lock and
// loopback port. It cannot change the 94 historical tests' credentials/state.
test.describe.serial('named identities on isolated installations', () => {
  let processHandle, directory, baseURL;
  const password = 'SyntheticIdentity123!';
  test.beforeEach(async ({ page }) => {
    directory = fs.mkdtempSync(path.join(os.tmpdir(), 'slu-474-browser-'));
    const port = await new Promise((resolve, reject) => {
      const server = net.createServer(); server.on('error', reject);
      server.listen(0, '127.0.0.1', () => { const port = server.address().port; server.close(() => resolve(port)); });
    });
    baseURL = `http://127.0.0.1:${port}`;
    processHandle = spawn(process.env.SLU_IDENTITY_TEST_BINARY || path.resolve('webserver'), [], {
      cwd: path.resolve('.'), env: { ...process.env, DEBIAN_UPDATER_DB_PATH: path.join(directory, 'servers.db'), DEBIAN_UPDATER_LISTEN_ADDR: `127.0.0.1:${port}`, DEBIAN_UPDATER_KNOWN_HOSTS: path.join(directory, 'known_hosts') }, stdio: 'ignore',
    });
    await expect.poll(async () => { try { return (await fetch(`${baseURL}/api/auth/status`)).status; } catch (_) { return 0; } }).toBe(200);
    await page.goto(`${baseURL}/setup`);
    await page.locator('#username').fill('admin'); await page.locator('#password').fill(password); await page.locator('#password-confirm').fill(password);
    await page.getByRole('button', { name: /create.*account/i }).click();
    await expect(page).toHaveURL(`${baseURL}/`);
  });
  test.afterEach(async () => {
    if (processHandle && processHandle.exitCode === null) { const exited = new Promise(resolve => processHandle.once('exit', resolve)); processHandle.kill('SIGTERM'); await exited; }
    if (directory) fs.rmSync(directory, { recursive: true, force: true });
  });
  async function api(page, route, method = 'GET', body) {
    return page.evaluate(async ({ route, method, body }) => {
      const response = await window.__nativeFetch(route, { method, headers: { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
      return { status: response.status, data: await response.json().catch(() => ({})) };
    }, { route, method, body });
  }
  function totp(secret) {
    const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'; let bits = '';
    for (const char of secret) bits += alphabet.indexOf(char).toString(2).padStart(5, '0');
    const key = Buffer.from(bits.match(/.{8}/g).map(byte => parseInt(byte, 2)));
    const counter = Buffer.alloc(8); counter.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)));
    const hash = crypto.createHmac('sha1', key).update(counter).digest(); const offset = hash[19] & 15;
    return String((hash.readUInt32BE(offset) & 0x7fffffff) % 1000000).padStart(6, '0');
  }
  test('accounts, scoped personal tokens, single-display secrets and responsive views', async ({ page }, info) => {
    await page.goto(`${baseURL}/account`);
    await expect(page.locator('#identity-summary')).toContainText('admin · admin');
    await page.locator('#new-account-name').fill('reader'); await page.locator('#new-account-password').fill(password);
    await page.getByRole('button', { name: 'Create account', exact: true }).click(); await expect(page.locator('#account-list')).toContainText('reader');
    await page.locator('#token-name').fill('monitoring'); await page.getByRole('button', { name: 'Create personal token' }).click();
    await expect(page.locator('#token-secret')).toContainText('slu_'); const secret = await page.locator('#token-secret').textContent();
    const result = await page.evaluate(async secret => {
      const read = await fetch('/api/servers', { headers: { Authorization: `Bearer ${secret}` } });
      const write = await fetch('/api/update/missing', { method: 'POST', headers: { Authorization: `Bearer ${secret}` } });
      return { read: read.status, write: write.status };
    }, secret); expect(result).toEqual({ read: 200, write: 403 });
    await page.getByRole('button', { name: 'I have saved the secret' }).click(); await expect(page.locator('#token-secret')).toBeEmpty();
    await expect(page.locator('#token-list')).toContainText('monitoring');
    // Screenshots intentionally contain neither passwords, token nor MFA secrets.
    await page.evaluate(() => window.scrollTo(0, 0));
    await page.screenshot({ path: info.outputPath('account-desktop.png'), fullPage: true });
    await page.setViewportSize({ width: 390, height: 844 }); await page.screenshot({ path: info.outputPath('account-mobile.png'), fullPage: true });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBeTruthy();
    await page.getByRole('button', { name: 'Revoke', exact: true }).click(); await expect(page.locator('#token-list')).toContainText('revoked');
    expect((await api(page, '/api/servers', 'GET')).status).toBe(200);
    await page.locator('#logout-btn').click(); await expect(page).toHaveURL(`${baseURL}/login`);
    await page.locator('#username').fill('reader'); await page.locator('#password').fill(password); await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(page).toHaveURL(`${baseURL}/`); await expect(page.getByRole('link', { name: 'Admin', exact: true })).toHaveCount(0);
    await expect(page.getByRole('link', { name: 'Manage Servers', exact: true })).toHaveCount(0);
    expect((await api(page, '/api/update/missing', 'POST')).status).toBe(403);
    await page.goto(`${baseURL}/account`); await expect(page.locator('#identity-summary')).toContainText('reader · reader'); await expect(page.locator('#accounts-section')).toBeHidden();
  });
  test('confirmed MFA challenge, recovery replay rejection and sensitive reauthentication', async ({ page }) => {
    await page.goto(`${baseURL}/account`); await page.getByRole('button', { name: 'Set up authenticator' }).click();
    await expect(page.locator('#mfa-secret')).toHaveText(/^[A-Z2-7]+$/);
    const secret = await page.locator('#mfa-secret').textContent();
    await page.locator('#mfa-confirm-code').fill(totp(secret)); await page.getByRole('button', { name: 'Enable MFA', exact: true }).click();
    await expect(page.locator('#recovery-codes')).not.toBeEmpty(); const codes = (await page.locator('#recovery-codes').textContent()).trim().split('\n'); expect(codes).toHaveLength(10);
    await page.getByRole('link', { name: 'Sign in again' }).click(); await page.locator('#username').fill('admin'); await page.locator('#password').fill(password); await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(page.locator('#mfa-login-form')).toBeVisible();
    const incomplete = await page.evaluate(async () => (await fetch('/api/auth/status')).json()); expect(incomplete.authenticated).toBe(false);
    await page.locator('#mfa-login-code').fill(codes[0]); await page.getByRole('button', { name: 'Verify and sign in' }).click(); await expect(page).toHaveURL(`${baseURL}/`);
    await page.goto(`${baseURL}/account`); await expect(page.locator('#mfa-status')).toHaveText('Authenticator enabled.');
    await page.locator('#reauth-password').fill(password); await page.locator('#reauth-code').fill(codes[0]); await page.getByRole('button', { name: 'Confirm identity' }).click(); await expect(page.locator('#account-status')).toContainText('invalid authentication factor');
    await page.locator('#reauth-code').fill(codes[1]); await page.getByRole('button', { name: 'Confirm identity' }).click(); await expect(page.locator('#account-status')).toContainText('Identity confirmed');
    await page.getByRole('button', { name: 'Disable MFA', exact: true }).click(); await expect(page).toHaveURL(`${baseURL}/login`);
    await page.locator('#username').fill('admin'); await page.locator('#password').fill(password); await page.getByRole('button', { name: 'Sign in', exact: true }).click(); await expect(page).toHaveURL(`${baseURL}/`);
  });
});
