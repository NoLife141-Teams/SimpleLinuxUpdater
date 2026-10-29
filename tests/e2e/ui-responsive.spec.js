const { test, expect, ensureAuthenticatedSession } = require('./support/fixtures');
const { stubAdminApi, stubManageApi, stubDashboardApi, makeServer, fulfillJson } = require('./support/builders');
const fs = require('node:fs');
const path = require('node:path');

async function fitsDocument(page) {
    await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(0);
}
async function capture(page, testInfo, name) {
    const image = await page.screenshot({ fullPage: false });
    await testInfo.attach(name, { body: image, contentType: 'image/png' });
    if (process.env.SLU_CAPTURE_UI_514) {
        const directory = path.resolve('.github/assets/ui-responsive-514');
        fs.mkdirSync(directory, { recursive: true });
        fs.writeFileSync(path.join(directory, `${name}.png`), image);
    }
}
async function navClearsHeader(page, prefix) {
    await expect.poll(() => page.evaluate(prefix => {
        const header = document.querySelector('.app-header');
        const pinned = ['sticky', 'fixed'].includes(getComputedStyle(header).position);
        const nav = document.getElementById(`${prefix}-section-nav`).getBoundingClientRect();
        return nav.top - (pinned ? header.getBoundingClientRect().bottom : 0);
    }, prefix)).toBeGreaterThanOrEqual(7);
}
for (const prefix of ['manage', 'admin']) {
    test(`${prefix} section navigation clears the real header after tablet resize and deep links`, async ({ page }, testInfo) => {
        await ensureAuthenticatedSession(page);
        if (prefix === 'manage') await stubManageApi(page, { servers: [makeServer('responsive-host')] });
        else await stubAdminApi(page, {});
        const section = prefix === 'manage' ? 'import' : 'notifications';
        await page.setViewportSize({ width: 768, height: 884 });
        await page.goto(`/${prefix}#${prefix}-section-${section}`);
        const heading = page.locator(`#${prefix}-section-${section}-heading`);
        await expect(heading).toBeFocused();
        for (const width of [768, 900, 1284, 700, 768]) {
            await page.setViewportSize({ width, height: 884 });
            await navClearsHeader(page, prefix);
            await page.locator(`[data-${prefix}-section-link="${section}"]`).click();
            await expect(heading).toBeFocused();
            await expect.poll(() => page.evaluate(({ prefix, section }) => {
                const nav = document.getElementById(`${prefix}-section-nav`).getBoundingClientRect();
                const heading = document.getElementById(`${prefix}-section-${section}-heading`).getBoundingClientRect();
                return heading.top - nav.bottom;
            }, { prefix, section })).toBeGreaterThanOrEqual(0);
            await page.mouse.move(8, 400);
            await page.mouse.wheel(0, 180);
            await navClearsHeader(page, prefix);
            await page.mouse.wheel(0, -100);
            await navClearsHeader(page, prefix);
            await fitsDocument(page);
            await capture(page, testInfo, `${prefix}-${width}`);
        }
    });
}
for (const width of [320, 390]) {
    test(`Admin fits ${width}px before, during and after installation health recovery`, async ({ page }, testInfo) => {
        await page.setViewportSize({ width, height: 844 });
        await ensureAuthenticatedSession(page);
        await stubAdminApi(page, {});
        let release, failure = true, requests = 0;
        const pending = new Promise(resolve => { release = resolve; });
        await page.route('**/api/installation/health', async route => {
            requests++;
            await pending;
            if (failure) await fulfillJson(route, { error: 'Health unavailable' }, 503);
            else await route.continue();
        });
        try {
            await page.goto('/admin#admin-section-installation');
            await expect.poll(() => requests).toBeGreaterThan(0);
            const section = page.locator('#admin-section-installation');
            await expect(section.locator('[data-admin-section-status]')).toHaveText('Loading');
            await fitsDocument(page);
            await capture(page, testInfo, `admin-loading-${width}`);
            release();
            await expect(section.locator('[data-admin-section-status]')).toHaveText('Failed');
            await fitsDocument(page);
            await capture(page, testInfo, `admin-unavailable-${width}`);
            failure = false;
            await section.getByRole('button', { name: 'Retry', exact: true }).click();
            await expect(section.locator('[data-admin-section-status]')).toHaveText('Current');
            await fitsDocument(page);
            const button = await section.locator('[data-admin-section-toggle="installation"]').boundingBox();
            expect(button.x).toBeGreaterThanOrEqual(0);
            expect(button.x + button.width).toBeLessThanOrEqual(width);
        } finally { release(); }
    });
    test(`Observability fits ${width}px with pending, unavailable and current metadata`, async ({ page }, testInfo) => {
        await page.setViewportSize({ width, height: 844 });
        await ensureAuthenticatedSession(page);
        await stubDashboardApi(page, () => [makeServer('host-' + 'long-name-'.repeat(12))]);
        let release, failure = true;
        const pending = new Promise(resolve => { release = resolve; });
        await page.route('**/api/observability/summary*', async route => {
            await pending;
            await fulfillJson(route, failure ? { error: 'Summary unavailable' } : { totals: { updates_total: 1, success_rate_pct: 0 } }, failure ? 503 : 200);
        });
        try {
            await page.goto('/observability');
            await expect(page.locator('#summary-lifecycle')).toContainText('Loading');
            await fitsDocument(page);
            release();
            await expect(page.locator('#summary-lifecycle')).toContainText('Unavailable');
            await fitsDocument(page);
            failure = false;
            await page.locator('#summary-lifecycle').getByRole('button', { name: 'Retry' }).click();
            await expect(page.locator('#summary-lifecycle')).toContainText('Current');
            await fitsDocument(page);
            await capture(page, testInfo, `observability-${width}`);
        } finally { release(); }
    });
}
test('Status filter human words remain whole with visible counters and long technical tags', async ({ page }, testInfo) => {
    const longTag = 'technical-tag-' + 'x'.repeat(100);
    await stubDashboardApi(page, () => [makeServer('filters-host', 'pending_approval', [], { tags: [longTag] })]);
    await ensureAuthenticatedSession(page);
    for (const width of [1284, 900, 768, 390]) {
        await page.setViewportSize({ width, height: 884 });
        for (const label of ['Pending', 'Facts refresh']) {
            const filter = page.locator('#fleet-status-filters button').filter({ has: page.locator('span', { hasText: label }) });
            await expect(filter).toBeVisible();
            const words = await filter.locator('span').evaluate(node => {
                const text = node.firstChild, value = text.textContent;
                return Array.from(value.matchAll(/\S+/g), match => {
                    const range = document.createRange();
                    range.setStart(text, match.index); range.setEnd(text, match.index + match[0].length);
                    const rects = [...range.getClientRects()];
                    const bounds = node.getBoundingClientRect();
                    return { lines: rects.length, fits: rects.every(r => r.left >= bounds.left - 1 && r.right <= bounds.right + 1) };
                });
            });
            expect(words.every(word => word.lines === 1 && word.fits)).toBe(true);
            await expect(filter.locator('strong')).toBeVisible();
            await filter.focus();
            await expect(filter).toBeFocused();
        }
        await fitsDocument(page);
        if (width === 1284) await capture(page, testInfo, 'status-filters-1284');
    }
});
