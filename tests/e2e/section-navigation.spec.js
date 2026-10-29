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

for (const viewport of [{ width: 1284, height: 884 }, { width: 390, height: 844 }]) {
    test(`Manage legacy fragments survive delayed inventory responses at ${viewport.width}px`, async ({ page }) => {
        await page.setViewportSize(viewport);
        await ensureAuthenticatedSession(page);
        await stubManageApi(page, { servers: [makeServer("legacy-host")] });
        for (const [fragment, sectionID] of [["#server-directory", "directory"], ["#audit-trail", "audit"]]) {
            let release;
            const responseReady = new Promise(resolve => { release = resolve; });
            const handler = async route => {
                await responseReady;
                await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([makeServer("legacy-host")]) });
            };
            await page.route("**/api/servers", handler);
            const requested = `/manage?server=legacy-host&audit_target=legacy-host${fragment}`;
            try {
                await page.goto(requested);
                await expect(page.locator(`#manage-section-${sectionID}-heading`)).toBeFocused();
                await expect.poll(() => active(page, "manage")).toBe(sectionID);
                await page.evaluate(() => history.replaceState({ aliasTest: "preserved" }, "", location.href));
                release();
                await expect(page.locator("#manage-servers-table")).toContainText("legacy-host");
                await settle(page);
                await expect(page).toHaveURL(new URL(requested, page.url()).href);
                await expect(page.locator("#search")).toHaveValue("legacy-host");
                await expect(page.locator("#audit-target-filter")).toHaveValue("legacy-host");
                await expect(page.locator(`#manage-section-${sectionID}-heading`)).toBeFocused();
                await expect.poll(() => active(page, "manage")).toBe(sectionID);
                expect(await page.evaluate(() => history.state)).toEqual({ aliasTest: "preserved" });
            } finally {
                release();
                await page.unroute("**/api/servers", handler);
            }
        }
    });
}

for (const viewport of [{ width: 1284, height: 884 }, { width: 390, height: 844 }]) {
    test(`Admin scrolling preserves page position and disclosures after health loads at ${viewport.width}px`, async ({ page }) => {
        await page.setViewportSize(viewport);
        await ensureAuthenticatedSession(page);
        await stubAdminApi(page, {});
        const health = {
            state: "ready", checked_at: "2026-10-29T01:00:00Z",
            workers: { scheduler: { enabled: true, running: true, errors: 0 } },
            storage: { db_bytes: 348160, wal_bytes: 4120032, available_bytes: 41431736320 },
        };
        await page.route("**/api/installation/health", route => route.fulfill({
            status: 200, contentType: "application/json", body: JSON.stringify(health),
        }));
        await page.goto("/admin#admin-section-installation");
        await expect(page.locator("#installation-state")).toHaveText("ready");
        const ids = await page.locator("[data-admin-section-link]").evaluateAll(nodes => nodes.map(node => node.dataset.adminSectionLink));
        // Complete lazy reads before comparing wheel movement with actual page movement.
        for (const id of ids) {
            await page.locator(`[data-admin-section-link="${id}"]`).click();
            await expect(page.locator('[data-admin-section-lifecycle][data-status="loading"]')).toHaveCount(0);
            await settle(page);
        }
        await page.locator('[data-admin-section-link="installation"]').click();
        await settle(page);
        const disclosureState = await page.locator("[data-admin-section-toggle]").evaluateAll(nodes => nodes.map(node => node.getAttribute("aria-expanded")));
        await page.evaluate(() => {
            window.healthWorkerAnchor = document.querySelector("#installation-workers tr");
            window.healthScrollAudit = { mutations: 0, disclosures: [] };
            new MutationObserver(records => {
                for (const record of records) {
                    if (record.target.closest?.("#installation-workers")) window.healthScrollAudit.mutations++;
                    if (record.attributeName === "hidden" && record.target.matches("[data-admin-section-content]") && record.oldValue !== record.target.getAttribute("hidden"))
                        window.healthScrollAudit.disclosures.push(record.target.id);
                }
            }).observe(document.querySelector("main"), { subtree: true, childList: true, attributes: true, attributeFilter: ["hidden"], attributeOldValue: true });
        });
        await page.mouse.move(8, 400);
        const drift = [];
        for (const delta of [...Array(10).fill(140), ...Array(24).fill(-140)]) {
            const before = await page.evaluate(() => ({ y: scrollY, max: document.documentElement.scrollHeight - innerHeight }));
            await page.mouse.wheel(0, delta);
            await page.waitForTimeout(120);
            const after = await page.evaluate(() => scrollY);
            drift.push(after - Math.max(0, Math.min(before.max, before.y + delta)));
        }
        expect(drift.every(value => Math.abs(value) <= 2), JSON.stringify(drift)).toBe(true);
        expect(await page.locator("[data-admin-section-toggle]").evaluateAll(nodes => nodes.map(node => node.getAttribute("aria-expanded")))).toEqual(disclosureState);
        expect(await page.evaluate(() => window.healthScrollAudit.disclosures)).toEqual([]);
        // Navigation alone must not discard a possible browser scroll anchor.
        expect(await page.evaluate(() => window.healthWorkerAnchor.isConnected)).toBe(true);
        expect(await page.evaluate(() => window.healthScrollAudit.mutations)).toBe(0);
        health.state = "maintenance";
        health.workers.scheduler.enabled = false;
        await page.locator('#admin-section-installation [data-admin-section-retry="installation"]').last().click();
        await expect(page.locator("#installation-state")).toHaveText("maintenance");
        await expect(page.locator("#installation-workers tr").first().locator("td").nth(1)).toHaveText("Disabled");
    });
}

test("Admin loads approaching open sections before their headings enter the viewport", async ({ page }) => {
    await page.setViewportSize({ width: 1284, height: 884 });
    await ensureAuthenticatedSession(page);
    const state = {};
    await stubAdminApi(page, state);
    await page.goto("/admin");
    await settle(page);
    expect(state.scheduledRunsLoadCount || 0).toBe(0);
    const heading = page.locator("#admin-section-scheduled-runs-heading");
    await heading.evaluate(node => {
        dispatchEvent(new WheelEvent("wheel", { deltaY: 1 }));
        scrollTo(0, scrollY + node.getBoundingClientRect().top - innerHeight * 1.5);
    });
    await expect.poll(() => state.scheduledRunsLoadCount || 0).toBe(1);
    await expect(heading).not.toBeInViewport();
    await expect(page.locator('[data-admin-section-lifecycle="scheduled-runs"]')).toHaveAttribute("data-status", "current");
    await expect(page.locator('[data-admin-section-toggle="scheduled-runs"]')).toHaveAttribute("aria-expanded", "true");
    await expect(heading).not.toBeFocused();
});

test("A delayed application profile list does not expand an editor while the reader scrolls elsewhere", async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, {});
    let release, requested = false;
    const ready = new Promise(resolve => { release = resolve; });
    await page.route("**/api/postcheck-profiles", async route => {
        requested = true;
        await ready;
        await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ revision: 1, profiles: [{
            id: "web", name: "Website recovery", revision: 1, target_servers: [], target_tags: ["web"],
            checks: [{ id: "http", name: "HTTP readiness", kind: "http_status", url: "https://example.invalid/health", expected_status: 200, timeout_seconds: 30, interval_seconds: 2, mode: "blocking" }],
        }] }) });
    });
    try {
        await page.goto("/admin#admin-section-account-security");
        await expect.poll(() => requested).toBe(true);
        await expect(page.locator("#admin-section-account-security-heading")).toBeFocused();
        const editor = page.locator("#postcheck-editor");
        await expect(editor).toBeHidden();
        const height = await page.locator("#admin-section-application-checks").evaluate(node => node.getBoundingClientRect().height);
        await page.mouse.move(8, 400);
        await page.mouse.wheel(0, 140);
        release();
        await expect(page.locator('#postcheck-profile-select option[value="web"]')).toHaveCount(1);
        await settle(page);
        await expect(editor).toBeHidden();
        await expect(page.locator("#postcheck-test")).toBeDisabled();
        expect(await page.locator("#admin-section-application-checks").evaluate(node => node.getBoundingClientRect().height)).toBeCloseTo(height, 0);
        await page.locator('[data-admin-section-link="application-checks"]').click();
        await page.locator("#postcheck-profile-select").selectOption("web");
        await expect(editor).toBeVisible();
        await expect(page.locator("#postcheck-name")).toHaveValue("Website recovery");
    } finally {
        release();
    }
});
