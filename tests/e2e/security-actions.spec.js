const {test,expect,ensureAuthenticatedSession}=require("./support/fixtures");
const {fulfillJson}=require("./support/builders");

function fixture(server="example-host"){
    const base={server,package:"example",installed:"1.0-1",candidate:"1.0-2",cves:["CVE-2026-1000"],cve_count:1,severity:"critical",published_at:"2026-11-01T10:00:00Z",observed_at:"2026-11-02T10:00:00Z",scopes:["security"],known_exploited:false};
    return {generated_at:"2026-11-02T12:00:00Z",servers:["example-host","other-host"],fixes:[base],alerts:[{...base,cves:["CVE-2026-2000"],candidate:"",known_exploited:true,exploitation_freshness:"fresh",required_action:'Read vendor advice <script>alert("unsafe")</script>'}],lifecycle:[{server,component:"ubuntu",product:"ubuntu",kind:"os",version:"25.04",cycle:"25.04",state:"end_of_life",eol_at:"2026-01-17",source:"https://endoflife.date/ubuntu",freshness:"fresh",inventory_freshness:"stale",observed_at:"2026-11-02T10:00:00Z"},{server,component:"python3.9",product:"python",kind:"application",version:"3.9.2",cycle:"3.9",state:"upstream_end_of_life",eol_at:"2025-10-31",source:"https://endoflife.date/python",freshness:"fresh",inventory_freshness:"fresh"}],feeds:{ubuntu:{state:"fresh",observed_at:"2026-11-02T10:00:00Z"},kev:{state:"stale",observed_at:"2026-10-01T10:00:00Z",last_error:"unavailable"}},fix_total:1,alert_total:1,lifecycle_total:2,review_findings:7,review_priority:2,page:1,pages:1,recent_days:30};
}
test("security work queues show actionable fixes, unpatched priority and lifecycle without the full ledger",async({page})=>{
    const errors=[];page.on("pageerror",e=>errors.push(e.message));await ensureAuthenticatedSession(page);
    await page.route("**/api/security-actions?**",route=>fulfillJson(route,fixture()));
    await page.goto("/vulnerabilities");
    await expect(page.locator("#security-fixes")).toContainText("1.0-1 → 1.0-2");
    await expect(page.locator("#security-fixes a")).toHaveAttribute("href","/remediation?server=example-host");
    await expect(page.locator("#security-alerts")).toContainText("CVE-2026-2000");
    await expect(page.locator("#security-alerts")).toContainText("<script>");
    await expect(page.locator("#security-alerts script")).toHaveCount(0);
    await expect(page.locator("#security-lifecycle")).toContainText("2026-01-17");
    await expect(page.locator("#security-lifecycle")).toContainText("Fin de support amont");
    await expect(page.locator("#security-review")).toContainText("7 occurrences");
    expect(await page.locator("#all-cves").evaluate(el=>el.open)).toBe(false);
    for(const width of [1440,390]){
        await page.setViewportSize({width,height:900});
        expect(await page.evaluate(()=>document.body.scrollWidth<=document.documentElement.clientWidth)).toBe(true);
        await page.screenshot({path:`/tmp/slu-security-actions-${width}.png`,fullPage:true});
    }
    await page.locator("#security-alerts a").first().click();
    await expect(page).toHaveURL(/#all-cves$/);
    expect(await page.locator("#all-cves").evaluate(el=>el.open)).toBe(true);
    expect(errors).toEqual([]);
});
test("queue failures preserve only matching evidence and distinguish unknown data from no fixes",async({page})=>{
    await ensureAuthenticatedSession(page);let fail=false;
    await page.route("**/api/security-actions?**",route=>{
        if(fail)return route.fulfill({status:503,contentType:"application/json",body:'{"error":"synthetic unavailable"}'});
        return fulfillJson(route,fixture());
    });
    await page.goto("/vulnerabilities");await expect(page.locator("#security-fixes")).toContainText("example-host");
    fail=true;await page.locator("#security-refresh").click();await expect(page.locator("#security-error")).toContainText("derniers résultats");
    await expect(page.locator("#security-fixes")).toContainText("example-host");
    await page.locator("#security-server").selectOption("other-host");await expect(page.locator("#security-error")).toContainText("HTTP 503");
    await expect(page.locator("#security-fixes")).not.toContainText("example-host");
    fail=false;await page.unroute("**/api/security-actions?**");
    await page.route("**/api/security-actions?**",route=>fulfillJson(route,{...fixture("other-host"),fixes:[],alerts:[],lifecycle:[],fix_total:0,alert_total:0,lifecycle_total:0}));
    await page.locator("#security-refresh").click();await expect(page.locator("#security-fixes")).toContainText("ne signifie pas qu’aucun correctif");
    await expect(page.locator("#security-alerts")).toContainText("données inconnues");
});
