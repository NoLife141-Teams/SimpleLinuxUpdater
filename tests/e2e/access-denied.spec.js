const { test, expect, password, ensureAuthenticatedSession } = require('./support/fixtures');
const fs = require('node:fs');
const path = require('node:path');

// Only synthetic accounts. No capture of login fields or one-time secrets.
test.use({ trace: 'off', screenshot: 'off', video: 'off' });
for (const role of ['reader', 'operator']) {
    for (const width of [320, 390, 1284]) {
        test(`${role} denied pages keep the shell and safe keyboard exits at ${width}px`, async ({ page }) => {
            await page.setViewportSize({ width, height: 884 });
            await ensureAuthenticatedSession(page);
            const created = await page.evaluate(async ({ role, password }) => {
                return (await window.__nativeFetch('/api/auth/accounts', {
                    method: 'POST', headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ username: role, password, role }),
                })).status;
            }, { role, password });
            expect(created).toBe(201);
            await page.locator('#logout-btn').click();
            await expect(page).toHaveURL(/\/login$/);
            await page.locator('#username').fill(role);
            await page.locator('#password').fill(password);
            await page.getByRole('button', { name: 'Sign in', exact: true }).click();
            await expect(page).toHaveURL(/\/$/);
            await page.waitForLoadState('networkidle');
            const reads = [], errors = [];
            let observe = false;
            page.on('request', request => {
                if (observe && new URL(request.url()).pathname.startsWith('/api/')) reads.push(new URL(request.url()).pathname);
            });
            page.on('pageerror', error => errors.push(error.message));
            page.on('console', message => {
                if (/Content Security Policy|violates.*directive/i.test(message.text())) errors.push(message.text());
            });
            for (const route of ['/admin', '/manage']) {
                reads.length = 0;
                observe = true;
                const response = await page.goto(route);
                expect(response.status()).toBe(403);
                expect(response.headers()['content-type']).toContain('text/html');
                expect(response.headers()['cache-control']).toContain('no-store');
                expect(response.headers()['content-security-policy']).toContain("script-src 'self'");
                await expect(page).toHaveTitle('SimpleLinuxUpdater - Access denied');
                await expect(page.getByRole('heading', { name: 'Access denied', level: 1 })).toBeVisible();
                await expect(page.getByText('Your account does not have permission to open this page.')).toBeVisible();
                await expect(page.locator('body')).toHaveAttribute('data-account-role', role);
                await expect(page.getByRole('navigation', { name: 'Main navigation' })).toBeVisible();
                await expect(page.locator('a[href="/admin"], a[href="/manage"]')).toHaveCount(0);
                await expect(page.locator('main')).toHaveCount(1);
                await page.waitForLoadState('networkidle');
                expect(reads).toEqual([]);
                expect(errors).toEqual([]);
                expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
                if (process.env.SLU_CAPTURE_UI_518) {
                    const directory = path.resolve('.github/assets/access-denied-518');
                    fs.mkdirSync(directory, { recursive: true });
                    await page.screenshot({ path: path.join(directory, `${role}-${route.slice(1)}-${width}.png`), fullPage: true });
                }
                observe = false;
                await page.locator('#logout-btn').focus();
                await page.keyboard.press('Tab');
                await expect(page.getByRole('link', { name: 'Return to Status', exact: true })).toBeFocused();
                if (route === '/manage') {
                    await page.keyboard.press('Tab');
                    await expect(page.getByRole('link', { name: 'Go to Account', exact: true })).toBeFocused();
                }
                await page.keyboard.press('Enter');
                await expect(page).toHaveURL(route === '/admin' ? /\/$/ : /\/account$/);
                if (route === '/manage') await expect(page.locator('#identity-summary')).toContainText(`${role} · ${role}`);
                await page.waitForLoadState('networkidle');
            }
            await page.goto('/admin');
            await page.locator('#logout-btn').click();
            await expect(page).toHaveURL(/\/login$/);
            expect(await page.evaluate(async () => (await (await fetch('/api/auth/status')).json()).authenticated)).toBe(false);
        });
    }
}
