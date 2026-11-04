const { test, expect, ensureAuthenticatedSession } = require('./support/fixtures');
const { stubDashboardApi, stubManageApi, stubAdminApi, makeServer, makePendingUpdates, fulfillJson } = require('./support/builders');

test('mobile shell, disclosures and server actions stay usable without horizontal scrolling', async ({ page }) => {
    await stubDashboardApi(page, () => [makeServer('web-01', 'pending_approval', makePendingUpdates(3))]);
    await ensureAuthenticatedSession(page);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto('/');
    const menu = page.locator('.app-nav-toggle');
    await expect(page.locator('#app-navigation')).toBeHidden();
    await menu.click();
    await expect(menu).toHaveAttribute('aria-expanded', 'true');
    await expect(page.getByRole('link', { name: 'Observability', exact: true })).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(menu).toBeFocused();
    await expect(menu).toHaveAttribute('aria-expanded', 'false');
    await expect(page.locator('#search')).toBeHidden();
    await page.locator('.fleet-filter-toggle').click();
    await page.locator('#search').fill('web');
    await page.locator('.fleet-filter-toggle').click();
    await expect(page.locator('#search')).toHaveValue('web');
    const row = page.locator('#servers-table tr[data-name="web-01"]');
    for (const width of [320, 390, 700]) {
        await page.setViewportSize({ width, height: 844 });
        const geometry = await row.evaluate(node => {
            const row = node.getBoundingClientRect();
            return { width: document.documentElement.scrollWidth, viewport: innerWidth, row: { left: row.left, right: row.right }, actions: [...node.querySelectorAll('.timeline-actions button')].map(button => {
                const r = button.getBoundingClientRect();
                return { left: r.left, right: r.right, height: r.height };
            }) };
        });
        expect(geometry.width).toBeLessThanOrEqual(width);
        for (const action of geometry.actions) {
            expect(action.left).toBeGreaterThanOrEqual(0);
            expect(action.right).toBeLessThanOrEqual(width);
            expect(action.height).toBeGreaterThanOrEqual(44);
        }
    }
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto('/');
    const approveButton = row.getByRole('button', { name: 'Approve (3)', exact: true });
    await expect(approveButton).toBeVisible();
    const approve = await approveButton.boundingBox();
    expect(approve.y + approve.height).toBeLessThanOrEqual(844);
    await row.getByRole('button', { name: 'Packages', exact: true }).click();
    await expect(page.locator('#status-drawer')).toHaveAttribute('aria-hidden', 'false');
    await expect(page.locator('#status-drawer-status .status-pill')).toHaveText(/pending approval/i);
});

test('directory prioritizes servers while export and bulk reviews stay explicit', async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, { servers: [makeServer('inventory-01')] });
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto('/manage');
    const row = page.locator('#manage-servers-table tbody tr').first();
    await expect(row).toContainText('inventory-01');
    const bounds = await row.boundingBox();
    expect(bounds.y + bounds.height).toBeLessThanOrEqual(1000);
    await expect(page.locator('#inventory-export-scope')).toBeHidden();
    await page.locator('.inventory-tool-disclosure > summary').filter({ hasText: 'Export inventory' }).click();
    await expect(page.locator('#inventory-export-scope')).toBeVisible();
    await expect(page.locator('#inventory-export-download')).toBeDisabled();
    await row.locator('input[type=checkbox]').check();
    await expect(page.locator('#inventory-bulk')).toHaveAttribute('open', '');
    await expect(page.locator('#bulk-selection-summary')).toHaveText('1 selected');
    await expect(page.locator('#bulk-preview-button')).toBeEnabled();
    await expect(page.locator('#bulk-confirm')).toBeHidden();
});

test('mobile settings picker opens the requested section and restores heading focus', async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, {});
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto('/admin');
    await expect(page.locator('[data-admin-section-content=notifications]')).toBeHidden();
    await page.locator('.admin-section-picker > summary').click();
    await page.locator('[data-admin-section-link=notifications]').click();
    await expect(page.locator('#admin-section-notifications-heading')).toBeFocused();
    await expect(page.locator('[data-admin-section-content=notifications]')).toBeVisible();
    await expect(page.locator('[data-section-current]')).toHaveText('Notifications');
    await expect(page.locator('.admin-section-picker')).not.toHaveAttribute('open', '');
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(390);
    await page.setViewportSize({ width: 1440, height: 1000 });
    await expect(page.locator('[data-admin-section-link=backup]')).toBeVisible();
    const links = await page.locator('[data-admin-section-link]').evaluateAll(nodes => nodes.map(node => {
        const rect = node.getBoundingClientRect();
        return { top: Math.round(rect.top), left: rect.left, right: rect.right };
    }));
    expect(new Set(links.map(link => link.top)).size).toBe(1);
    expect(links.at(-1).left).toBeGreaterThan(links[0].left);
    expect(links.every(link => link.left >= 0 && link.right <= 1440)).toBe(true);
    await page.locator('[data-admin-section-link=backup]').click();
    await expect(page.locator('#admin-section-backup-heading')).toBeFocused();
});

test('success rate retains number scale and semantic severity independently', async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await page.route('**/api/observability/summary*', route => fulfillJson(route, {
        totals: { updates_total: 10, updates_success: 9, updates_failure: 1, success_rate_pct: 90 },
        duration: { avg_ms: 1000, samples_with_duration: 10, samples_without_duration: 0 },
    }));
    await page.goto('/observability');
    await expect(page.locator('#kpi-success-rate')).toHaveText('90.00%');
    await expect(page.locator('#kpi-success-severity')).toHaveText('Degraded');
    for (const width of [390, 1440]) {
        await page.setViewportSize({ width, height: 844 });
        const fonts = await page.locator('#kpi-success-rate, #kpi-total, #kpi-duration').evaluateAll(nodes => nodes.map(node => getComputedStyle(node).fontSize));
        expect(new Set(fonts).size).toBe(1);
    }
});
