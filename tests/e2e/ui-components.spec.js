const { test, expect, ensureAuthenticatedSession } = require('./support/fixtures');
const { stubAdminApi, stubManageApi, stubDashboardApi, makeServer, fulfillJson } = require('./support/builders');
const fs = require('node:fs');
const path = require('node:path');
async function capture(page, testInfo, name) {
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    const image = await page.screenshot({ fullPage: false });
    await testInfo.attach(name, { body: image, contentType: 'image/png' });
    if (process.env.SLU_CAPTURE_UI_515) {
        const directory = path.resolve('.github/assets/ui-components-515');
        fs.mkdirSync(directory, { recursive: true });
        fs.writeFileSync(path.join(directory, `${name}.png`), image);
    }
}
async function openConfirmation(page) {
    await page.evaluate(() => {
        window.__componentConfirmed = undefined;
        void window.confirmTypedAction('Confirm this fixture action.', 'CONFIRM', { title: 'Shared confirmation', consequences: 'Fixture only: no business action will run.' })
            .then(value => { window.__componentConfirmed = value; });
    });
}
for (const prefix of ['manage', 'admin']) {
    test(`${prefix} Package Deferral fields align by row and stack naturally on mobile`, async ({ page }, testInfo) => {
        await ensureAuthenticatedSession(page);
        if (prefix === 'manage') {
            await stubManageApi(page, { servers: [makeServer('shared-components-host')] });
            // All host-key and profile facts are synthetic; no remote connection runs.
            await page.route('**/api/servers/shared-components-host/package-deferrals', route => fulfillJson(route, { revision: 'ui-fixture', rules: [] }));
            await page.route('**/api/servers/shared-components-host/postchecks', route => fulfillJson(route, { profile: null }));
            await page.goto('/manage');
            await page.locator('[data-action="edit-server"]').click();
        } else {
            await stubAdminApi(page, {});
            await page.goto('/admin#admin-section-scheduled-policies');
        }
        const editor = page.locator(prefix === 'manage' ? '#server-package-deferrals' : '#policy-package-deferrals');
        for (const width of [1284, 768, 700, 390, 320]) {
            await page.setViewportSize({ width, height: 884 });
            await editor.scrollIntoViewIfNeeded();
            const fields = await editor.locator('.deferral-fields label').evaluateAll(labels => labels.map(label => {
                const input = label.querySelector('input').getBoundingClientRect();
                const range = document.createRange();
                range.selectNode(label.firstChild);
                const text = label.querySelector('span')?.getBoundingClientRect() || range.getBoundingClientRect();
                return { labelTop: text.top, labelBottom: text.bottom, inputTop: input.top, inputBottom: input.bottom, x: input.x };
            }));
            if (width >= 700) {
                expect(Math.max(...fields.map(f => f.labelTop)) - Math.min(...fields.map(f => f.labelTop))).toBeLessThanOrEqual(1);
                expect(Math.max(...fields.map(f => f.inputTop)) - Math.min(...fields.map(f => f.inputTop))).toBeLessThanOrEqual(1);
            } else {
                for (let i = 0; i < fields.length; i++) {
                    expect(fields[i].inputTop - fields[i].labelBottom).toBeGreaterThanOrEqual(5);
                    if (i) expect(fields[i].labelTop - fields[i-1].inputBottom).toBeGreaterThanOrEqual(7);
                }
            }
            await capture(page, testInfo, `${prefix}-deferrals-${width}`);
        }
        if (prefix === 'manage') {
            await page.setViewportSize({ width: 1284, height: 884 });
            const dialog = await page.locator('#edit-modal .modal').boundingBox();
            expect(dialog.width).toBeGreaterThan(900);
        }
    });
}
for (const width of [1284, 390]) {
    test(`shared typed confirmations keep identical geometry and keyboard behavior at ${width}px`, async ({ page }, testInfo) => {
        await page.setViewportSize({ width, height: 884 });
        await ensureAuthenticatedSession(page);
        await stubDashboardApi(page, () => []);
        await stubManageApi(page, { servers: [] });
        const widths = [], controls = [];
        for (const route of ['/', '/manage', '/account']) {
            await page.goto(route);
            const previous = page.locator('#logout-btn');
            await previous.focus();
            await openConfirmation(page);
            const dialog = page.locator('#typed-confirm-modal');
            const input = page.locator('#typed-confirm-input');
            await expect(dialog).toBeVisible();
            await expect(input).toBeFocused();
            await expect(page.locator('#typed-confirm-submit')).toBeDisabled();
            const geometry = await dialog.evaluate(node => {
                const modal = node.querySelector('.modal').getBoundingClientRect();
                const input = node.querySelector('input').getBoundingClientRect();
                const actions = node.querySelector('.modal-actions').getBoundingClientRect();
                const submit = getComputedStyle(node.querySelector('#typed-confirm-submit'));
                const cancel = getComputedStyle(node.querySelector('#typed-confirm-cancel'));
                const field = getComputedStyle(node.querySelector('input'));
                return { width: modal.width, gap: actions.top - input.bottom, left: modal.left, right: modal.right,
                    controlStyle: { inputHeight: input.height, inputPadding: field.padding, submitWeight: submit.fontWeight, cancelWeight: cancel.fontWeight } };
            });
            widths.push(geometry.width);
            controls.push(JSON.stringify(geometry.controlStyle));
            expect(geometry.gap).toBeGreaterThanOrEqual(12);
            expect(geometry.gap).toBeLessThanOrEqual(16);
            expect(geometry.left).toBeGreaterThanOrEqual(0);
            expect(geometry.right).toBeLessThanOrEqual(width);
            await input.fill('WRONG');
            await expect(page.locator('#typed-confirm-submit')).toBeDisabled();
            await input.fill('CONFIRM');
            await page.keyboard.press('Shift+Tab');
            await expect(page.locator('#typed-confirm-submit')).toBeFocused();
            await page.keyboard.press('Tab');
            await expect(input).toBeFocused();
            await capture(page, testInfo, `typed-${route === '/' ? 'status' : route.slice(1)}-${width}`);
            await page.keyboard.press('Escape');
            await expect(dialog).not.toBeVisible();
            await expect(previous).toBeFocused();
            expect(await page.evaluate(() => window.__componentConfirmed)).toBe(false);
            await openConfirmation(page);
            await expect(input).toBeFocused();
            await input.fill('CONFIRM');
            await page.locator('#typed-confirm-submit').click();
            await expect.poll(() => page.evaluate(() => window.__componentConfirmed)).toBe(true);
            await expect(dialog).not.toBeVisible();
            await expect(previous).toBeFocused();
        }
        expect(Math.max(...widths) - Math.min(...widths)).toBeLessThanOrEqual(1);
        expect(new Set(controls).size).toBe(1);
    });
}
test('inventory file component has one state, visible keyboard focus, selection and reset', async ({ page }, testInfo) => {
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, { servers: [] });
    await page.goto('/manage#manage-section-import');
    const input = page.locator('#inventory-import-file');
    const trigger = page.locator('label[for="inventory-import-file"]');
    await expect(input).toHaveAccessibleName('Choose inventory file');
    await expect(page.locator('#import-file-name')).toHaveText('No file selected');
    await page.getByRole('link', { name: 'Download JSON example' }).focus();
    await page.keyboard.press('Tab');
    await expect(input).toBeFocused();
    await expect(trigger).toHaveCSS('outline-style', 'solid');
    await input.setInputFiles({ name: 'first.csv', mimeType: 'text/csv', buffer: Buffer.from('name,host,port,user,tags\nui-host,192.0.2.90,22,root,[]\n') });
    await expect(page.locator('#import-file-name')).toHaveText('first.csv');
    await expect(page.locator('#inventory-import').getByText('first.csv', { exact: true })).toHaveCount(1);
    await expect(input).toHaveAccessibleName('Choose inventory file');
    await expect(page.locator('#import-preview-button')).toBeEnabled();
    await input.setInputFiles({ name: 'replacement.json', mimeType: 'application/json', buffer: Buffer.from('[{"name":"ui-host","host":"192.0.2.90","user":"root"}]') });
    await expect(page.locator('#import-file-name')).toHaveText('replacement.json');
    await expect(page.locator('#inventory-import').getByText('replacement.json', { exact: true })).toHaveCount(1);
    await expect(input).toHaveAccessibleName('Choose inventory file');
    await page.locator('#import-preview-button').click();
    await expect(page.locator('#import-preview')).toBeVisible();
    await page.locator('#import-reset').click();
    await expect(page.locator('#import-file-name')).toHaveText('No file selected');
    await expect(input).toHaveValue('');
    await expect(trigger).toHaveText('Choose inventory file');
    await expect(input).toHaveAccessibleName('Choose inventory file');
    await expect(page.locator('#inventory-import').getByText('replacement.json', { exact: true })).toHaveCount(0);
    await expect(page.locator('#import-preview-button')).toBeDisabled();
    await page.setViewportSize({ width: 390, height: 844 });
    await trigger.scrollIntoViewIfNeeded();
    await capture(page, testInfo, 'inventory-file-390');
});
