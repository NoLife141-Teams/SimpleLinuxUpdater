const { test, expect, ensureAuthenticatedSession } = require('./support/fixtures');
const { stubAdminApi, stubManageApi, stubDashboardApi, fulfillJson } = require('./support/builders');
const fs = require('node:fs');
const path = require('node:path');

// Foreground comes from CSS, never from antialiased glyph pixels. Temporarily
// transparent text preserves geometry while exposing the actual composed paint
// below every pixel of every text line (including gradients and translucency).
async function measureContrast(locator) {
    await locator.scrollIntoViewIfNeeded();
    const page = locator.page();
    const facts = await locator.evaluate(node => {
        const range = document.createRange();
        range.selectNodeContents(node);
        const style = getComputedStyle(node);
        const color = style.color;
        const originalStyle = node.getAttribute('style');
        const bounds = node.getBoundingClientRect();
        const rects = Array.from(range.getClientRects(), r => {
            // Ellipsized lines can extend behind a neighboring link/border.
            const x = Math.max(r.left, bounds.left), y = Math.max(r.top, bounds.top);
            return { x, y, width: Math.max(0, Math.min(r.right, bounds.right) - x), height: Math.max(0, Math.min(r.bottom, bounds.bottom) - y) };
        });
        node.style.setProperty('color', 'transparent', 'important');
        node.style.setProperty('text-shadow', 'none', 'important');
        return { color, originalStyle, rects, text: node.textContent.trim() };
    });
    let background;
    try {
        background = await page.screenshot({ scale: 'css', animations: 'disabled' });
    } finally {
        await locator.evaluate((node, original) => {
            if (original === null) node.removeAttribute('style');
            else node.setAttribute('style', original);
        }, facts.originalStyle);
    }
    return page.evaluate(async ({ facts, png }) => {
        const luminance = rgb => rgb.map(c => {
            const v = c / 255;
            return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
        }).reduce((sum, c, i) => sum + c * [0.2126, 0.7152, 0.0722][i], 0);
        const ratio = (a, b) => (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
        if (ratio(luminance([255, 255, 255]), luminance([0, 0, 0])) !== 21) throw new Error('Luminance calibration failed');
        const fg = facts.color.match(/[\d.]+/g).map(Number);
        if (fg.length > 3 && fg[3] !== 1) throw new Error('Measurement requires opaque foreground');
        const bitmap = await createImageBitmap(new Blob([Uint8Array.from(atob(png), c => c.charCodeAt(0))], { type: 'image/png' }));
        const canvas = new OffscreenCanvas(bitmap.width, bitmap.height);
        const context = canvas.getContext('2d');
        context.drawImage(bitmap, 0, 0);
        const { data } = context.getImageData(0, 0, bitmap.width, bitmap.height);
        let minimum = Infinity, worstBackground, samples = 0;
        for (const r of facts.rects) {
            // Ignore fractional edges; all sampled pixels must lie inside the line.
            for (let y = Math.max(0, Math.ceil(r.y)); y < Math.min(bitmap.height, Math.floor(r.y + r.height)); y++) {
                for (let x = Math.max(0, Math.ceil(r.x)); x < Math.min(bitmap.width, Math.floor(r.x + r.width)); x++) {
                    const i = (y * bitmap.width + x) * 4;
                    const bg = [data[i], data[i + 1], data[i + 2]];
                    const contrast = ratio(luminance(fg.slice(0, 3)), luminance(bg));
                    samples++;
                    if (contrast < minimum) { minimum = contrast; worstBackground = bg; }
                }
            }
        }
        bitmap.close();
        if (!samples) throw new Error('No visible text pixels measured');
        return { text: facts.text, foreground: fg.slice(0, 3), background: worstBackground, minimum, samples };
    }, { facts, png: background.toString('base64') });
}
async function check(locator, evidence, label) {
    const result = await measureContrast(locator);
    evidence.push({ label, ...result });
    expect(result.minimum, `${label}: ${JSON.stringify(result)}`).toBeGreaterThanOrEqual(4.5);
}
async function save(page, info, name, evidence) {
    const json = Buffer.from(JSON.stringify(evidence, null, 2) + '\n');
    await info.attach(name, { body: json, contentType: 'application/json' });
    if (process.env.SLU_CAPTURE_UI_517) {
        const directory = path.resolve('.github/assets/metadata-contrast-517');
        fs.mkdirSync(directory, { recursive: true });
        fs.writeFileSync(path.join(directory, `${name}.json`), json);
        await page.screenshot({ path: path.join(directory, `${name}.png`), animations: 'disabled' });
    }
}
for (const width of [390, 768, 1284]) {
    test(`informative navigation and metrics contrast on composed surfaces at ${width}px`, async ({ page }, info) => {
        await page.setViewportSize({ width, height: 884 });
        await ensureAuthenticatedSession(page);
        await stubAdminApi(page, {});
        await stubManageApi(page, { servers: [] });
        await stubDashboardApi(page, () => []);
        await page.route('**/api/observability/summary*', route => fulfillJson(route, {
            totals: { updates_total: 1, updates_success: 0, updates_failure: 1, success_rate_pct: 0 },
            duration: { avg_ms: 0, samples_with_duration: 0, samples_without_duration: 1 },
        }));
        for (const prefix of ['manage', 'admin']) {
            const first = prefix === 'manage' ? 'global-key' : 'app-time';
            const second = prefix === 'manage' ? 'import' : 'metrics';
            const evidence = [];
            await page.goto(`/${prefix}#${prefix}-section-${first}`);
            const link = page.locator(`#${prefix}-section-nav a[href="#${prefix}-section-${first}"]`);
            const detail = link.locator('small');
            await expect(detail).toHaveText(prefix === 'manage' ? 'Not configured' : 'America/Toronto');
            await expect(link).toHaveAttribute('aria-current', 'location');
            await check(detail, evidence, 'selected detail');
            await page.locator(`#${prefix}-section-nav a[href="#${prefix}-section-${second}"]`).click();
            await expect(link).not.toHaveAttribute('aria-current', 'location');
            await page.evaluate(() => document.activeElement?.blur());
            await page.mouse.move(0, 0);
            await check(detail, evidence, 'neutral detail');
            await check(link.locator('span').first(), evidence, 'neutral title');
            await expect(link).not.toHaveAttribute('aria-current', 'location');
            await link.hover();
            await check(detail, evidence, 'hover detail');
            // Actual keyboard focus, not a simulated CSS class.
            await page.keyboard.press('Tab');
            await link.focus();
            await expect(link).toBeFocused();
            expect(await link.evaluate(node => node.matches(':focus-visible'))).toBe(true);
            await check(detail, evidence, 'focus detail');
            await save(page, info, `${prefix}-${width}`, evidence);
        }
        await page.goto('/observability');
        await expect(page.locator('#kpi-success-context')).toHaveText('0 successful runs · 1 failed run');
        const evidence = [];
        for (const id of ['kpi-success-context', 'kpi-total-context', 'kpi-duration-samples']) await check(page.locator(`#${id}`), evidence, id);
        await save(page, info, `observability-${width}`, evidence);
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    });
    test(`confirmation context stays readable across pages at ${width}px`, async ({ page }, info) => {
        await page.setViewportSize({ width, height: 884 });
        await ensureAuthenticatedSession(page);
        await stubDashboardApi(page, () => []);
        await stubManageApi(page, { servers: [] });
        for (const route of ['/', '/manage', '/account']) {
            await page.goto(route);
            await page.evaluate(() => {
                void window.confirmAction('Review this fixture only.', {
                    title: 'Review fixture', resources: 'Synthetic resource', consequences: 'No business action will run.', danger: true,
                });
            });
            const dialog = page.locator('#action-confirm-modal');
            await expect(dialog).toBeVisible();
            const evidence = [];
            for (const node of await dialog.locator('.confirmation-summary dt, .confirmation-summary dd').all()) await check(node, evidence, 'confirmation context');
            await save(page, info, `confirmation-${route === '/' ? 'status' : route.slice(1)}-${width}`, evidence);
            await page.keyboard.press('Escape');
            await expect(dialog).not.toBeVisible();
        }
    });
}
