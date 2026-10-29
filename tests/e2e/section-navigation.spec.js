const { test, expect, ensureAuthenticatedSession } = require("./support/fixtures");
const { stubAdminApi, stubManageApi, makeServer } = require("./support/builders");

async function settle(page) {
    let previous = "", stable = 0;
    await expect.poll(async () => {
        const current = await page.evaluate(() => Array.from(document.querySelectorAll('[data-admin-section], [data-manage-section]')).map(node => {
            const rect = node.getBoundingClientRect();
            return `${Math.round(rect.top + scrollY)}:${Math.round(rect.height)}`;
        }).join('|'));
        stable = current === previous ? stable + 1 : 0;
        previous = current;
        return stable;
    }, { intervals: [20, 30, 40] }).toBeGreaterThanOrEqual(2);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
}
async function active(page, prefix) {
    return page.locator(`[data-${prefix}-section-link][aria-current=location]`).getAttribute(`data-${prefix}-section-link`);
}
async function rows(page, prefix) {
    return page.locator(`[data-${prefix}-section]`).evaluateAll((nodes, prefix) => {
        const result = [];
        for (const node of nodes) {
            const top = Math.round(node.getBoundingClientRect().top + scrollY);
            const row = result.find(row => Math.abs(row.top - top) <= 8);
            const id = node.getAttribute(`data-${prefix}-section`);
            if (row) row.ids.push(id); else result.push({ top, ids: [id] });
        }
        return result.sort((a,b) => a.top-b.top);
    }, prefix);
}

for (const prefix of ["manage", "admin"]) {
    for (const viewport of [{ width: 1284, height: 884 }, { width: 390, height: 844 }]) {
        test(`${prefix} section areas track stable rows in both directions at ${viewport.width}px without moving focus`, async ({ page }) => {
            await page.setViewportSize(viewport);
            await ensureAuthenticatedSession(page);
            if (prefix === "manage") await stubManageApi(page, { servers: Array.from({length:12}, (_,i) => makeServer(`area-host-${i}`)) });
            else await stubAdminApi(page, {});
            const errors = [];
            page.on('pageerror', error => errors.push(String(error)));
            await page.goto(`/${prefix}`);
            await expect(page.locator(prefix === 'manage' ? '#manage-servers-table' : '#auth-session-status')).toBeVisible();
            // Finish lazy reads before measuring the layout; then preserve the disclosure state.
            const ids = await page.locator(`[data-${prefix}-section-link]`).evaluateAll((nodes, prefix) => nodes.map(node => node.getAttribute(`data-${prefix}-section-link`)), prefix);
            for (const id of ids) {
                await page.locator(`[data-${prefix}-section-link="${id}"]`).click();
                await settle(page);
            }
            if (prefix === 'admin') {
                await expect(page.locator('[data-admin-section-lifecycle][data-status="loading"]')).toHaveCount(0);
            }
            await page.evaluate(() => document.activeElement.blur());
            await page.evaluate(() => scrollTo(0,0));
            await settle(page);
            const layout = await rows(page, prefix);
            if (viewport.width === 1284 && prefix === 'manage') expect(layout[0].ids).toEqual(['global-key','add-server']);
            if (viewport.width === 390) expect(layout.every(row => row.ids.length === 1)).toBe(true);
            const maxScroll = await page.evaluate(() => document.documentElement.scrollHeight-innerHeight);
            const samples = [];
            for (let y=0; y<maxScroll; y+=110) samples.push(y);
            samples.push(maxScroll);
            const down = [];
            for (const y of [...samples, ...samples.slice().reverse()]) {
                await page.evaluate(y => { window.dispatchEvent(new WheelEvent('wheel')); scrollTo(0,y); }, y);
                await settle(page);
                const selected = await active(page,prefix);
                const index = layout.findIndex(row => row.ids.includes(selected));
                down.push(index);
                const expected = layout[index].ids.slice().sort();
                const marked = await page.locator(`[data-${prefix}-section-link].is-in-area`).evaluateAll((nodes,prefix) => nodes.map(node => node.getAttribute(`data-${prefix}-section-link`)).sort(),prefix);
                expect(marked).toEqual(expected);
                await expect(page.locator(`[data-${prefix}-section-link][aria-current=location]`)).toHaveCount(1);
                expect(await page.evaluate(() => document.activeElement.tagName)).toBe('BODY');
            }
            const forward = down.slice(0,samples.length);
            const backward = down.slice(samples.length);
            expect(forward.every((value,i) => !i || value >= forward[i-1])).toBe(true);
            expect(backward.every((value,i) => !i || value <= backward[i-1])).toBe(true);
            expect([...new Set(forward)]).toEqual(layout.map((_,i)=>i));
            expect(errors).toEqual([]);
        });
    }
    test(`${prefix} section interaction priority survives resize, disclosures, deep links and history`, async ({ page }) => {
        await page.setViewportSize({width:1284,height:884});
        await ensureAuthenticatedSession(page);
        if (prefix === 'manage') await stubManageApi(page, {servers:[]}); else await stubAdminApi(page,{});
        const first = prefix === 'manage' ? 'global-key' : 'app-time';
        const second = prefix === 'manage' ? 'add-server' : 'notifications';
        const errors = [];
        page.on('pageerror', error => errors.push(String(error)));
        await page.goto(`/${prefix}#${prefix}-section-${second}`);
        await expect(page).toHaveURL(new RegExp(`#${prefix}-section-${second}$`));
        await expect(page.locator(`#${prefix}-section-${second}-heading`)).toBeFocused();
        await expect.poll(() => active(page,prefix)).toBe(second);
        await page.locator(`#${prefix}-section-${second}-heading`).evaluate(node => node.blur());
        await page.locator(`#${prefix}-section-${first}-heading`).click();
        await expect.poll(() => active(page,prefix)).toBe(first);
        await page.locator(`#${prefix}-section-${second}-heading`).focus();
        await expect.poll(() => active(page,prefix)).toBe(second);
        await page.setViewportSize({width:1100,height:884});
        await settle(page);
        await expect(page.locator(`#${prefix}-section-${second}-heading`)).toBeFocused();
        const headingBox = await page.locator(`#${prefix}-section-${second}-heading`).boundingBox();
        if (headingBox.y < 884 && headingBox.y + headingBox.height > 164)
            await expect.poll(() => active(page,prefix)).toBe(second);
        // A reflow may move the focused card completely outside the viewport.
        // Keep its keyboard focus and allow geometry to follow the visible row.
        await page.locator(`[data-${prefix}-section-link="${second}"]`).click();
        await expect.poll(() => active(page,prefix)).toBe(second);
        await page.evaluate(() => history.replaceState({navigationTest: "preserved"}, "", location.href));
        await page.locator(`[data-${prefix}-section-toggle="${second}"]`).click();
        expect(await page.evaluate(() => history.state)).toEqual({navigationTest: "preserved"});
        await expect(page.locator(`[data-${prefix}-section-content="${second}"]`)).toBeHidden();
        await page.locator(`[data-${prefix}-section-link="${first}"]`).click();
        await page.locator(`[data-${prefix}-section-link="${second}"]`).click();
        await expect(page.locator(`[data-${prefix}-section-content="${second}"]`)).toBeVisible();
        await page.goBack();
        await expect(page.locator(`#${prefix}-section-${first}-heading`)).toBeFocused();
        await page.goForward();
        await expect(page.locator(`#${prefix}-section-${second}-heading`)).toBeFocused();
        await expect.poll(() => active(page,prefix)).toBe(second);
        for (let n = 0; n < 2; n++) {
            await page.evaluate(prefix => window[prefix === 'manage' ? 'ManagePage' : 'AdminPage'].mount(), prefix);
            await expect(page.locator(`#${prefix}-section-${second}-heading`)).toBeFocused();
            await expect.poll(() => active(page,prefix)).toBe(second);
        }
        const length = await page.evaluate(() => history.length);
        await page.evaluate(() => {
            window.dispatchEvent(new PageTransitionEvent('pagehide', {persisted:true}));
            window.dispatchEvent(new PageTransitionEvent('pageshow', {persisted:true}));
        });
        await page.locator(`[data-${prefix}-section-link="${first}"]`).click();
        expect(await page.evaluate(() => history.length)).toBe(length + 1);
        await expect(page.locator(`[data-${prefix}-section-link][aria-current=location]`)).toHaveCount(1);
        expect(errors).toEqual([]);
        await page.screenshot({path: test.info().outputPath(`${prefix}-desktop.png`)});
    });
}
