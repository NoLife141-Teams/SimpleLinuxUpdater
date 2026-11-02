const {test,expect,ensureAuthenticatedSession}=require("./support/fixtures");
const {fulfillJson}=require("./support/builders");

function fixture(server="example-host"){
    const base={server,package:"example",installed:"1.0-1",candidate:"1.0-2",cves:["CVE-2026-1000"],cve_count:1,severity:"critical",published_at:"2026-11-01T10:00:00Z",observed_at:"2026-11-02T10:00:00Z",scopes:["security"],known_exploited:false};
    return {generated_at:"2026-11-02T12:00:00Z",servers:["example-host","other-host"],fixes:[base],alerts:[{...base,cves:["CVE-2026-2000"],candidate:"",known_exploited:true,exploitation_freshness:"fresh",required_action:'Read vendor advice <script>alert("unsafe")</script>'}],lifecycle:[{server,component:"ubuntu",product:"ubuntu",kind:"os",version:"25.04",cycle:"25.04",state:"end_of_life",eol_at:"2026-01-17",source:"https://endoflife.date/ubuntu",freshness:"fresh",inventory_freshness:"stale",observed_at:"2026-11-02T10:00:00Z"},{server,component:"python3.9",product:"python",kind:"application",version:"3.9.2",cycle:"3.9",state:"upstream_end_of_life",eol_at:"2025-10-31",source:"https://endoflife.date/python",freshness:"fresh",inventory_freshness:"fresh"}],feeds:{ubuntu:{state:"fresh",observed_at:"2026-11-02T10:00:00Z"},kev:{state:"stale",observed_at:"2026-10-01T10:00:00Z",last_error:"unavailable"}},fix_total:1,alert_total:1,lifecycle_total:2,review_findings:7,review_priority:2,page:1,pages:1,recent_days:30};
}
const coverage=(server="example-host")=>({coverage:[{server,state:"complete",freshness:"stale",unknown:0,unsupported:0,error_count:0}],rows:[],servers:["example-host","other-host"],distros:[],summary:{cves:0,hosts:0,critical_high:0,with_fix:0,unknown_severity:0,findings:0},total:0,page:1,pages:1,page_size:50});
async function setup(page,actions=route=>fulfillJson(route,fixture())){
    await ensureAuthenticatedSession(page);
    await page.route("**/api/security-actions?**",actions);
    await page.route("**/api/vulnerabilities?**",route=>fulfillJson(route,coverage(new URL(route.request().url()).searchParams.get("server")||"example-host")));
}
test("one decision list at a time, full-width evidence and responsive layout",async({page})=>{
    const errors=[];page.on("pageerror",e=>errors.push(e.message));await setup(page);
    await page.goto("/vulnerabilities");
    await expect(page.locator("html")).toHaveAttribute("lang","en");
    await expect(page.getByRole("tab",{name:/Available fixes/})).toHaveAttribute("aria-selected","true");
    await expect(page.locator(".security-item")).toHaveCount(1);
    await expect(page.locator("#security-rows")).not.toContainText("CVE-2026-2000");
    await expect(page.locator("#security-rows a")).toHaveAttribute("href","/remediation?server=example-host");
    await page.getByRole("button",{name:/^Fix details for/}).click();
    await expect(page.locator(".security-evidence")).toBeVisible();
    await expect(page.locator(".security-evidence")).toContainText("1.0-2");
    await page.getByRole("tab",{name:/Priority alerts/}).click();
    await expect(page.locator("#security-rows")).toContainText("CVE-2026-2000");
    await page.getByRole("button",{name:/^Details and mitigations for/}).click();
    await expect(page.locator(".security-evidence")).toContainText('<script>alert("unsafe")</script>');
    await expect(page.locator(".security-evidence script")).toHaveCount(0);
    await expect(page.locator(".security-evidence a").first()).toHaveAttribute("href",/\/vulnerabilities\/all\?server=example-host&q=CVE/);
    await page.getByRole("tab",{name:/End of support/}).click();
    await expect(page.locator(".security-item")).toHaveCount(2);
    await expect(page.locator("#security-rows")).toContainText("Upstream support ended");
    for(const width of [1440,390]){
        await page.setViewportSize({width,height:900});
        expect(await page.evaluate(()=>document.body.scrollWidth<=document.documentElement.clientWidth)).toBe(true);
        await page.screenshot({path:`/tmp/slu-cve-english-${width}.png`,fullPage:true});
    }
    await page.locator(".security-detail-toggle").first().click();
    await expect(page.locator(".security-evidence").first()).toContainText("2026-01-17");
    expect(errors).toEqual([]);
});
test("scope, keyboard tabs and history preserve search without loading the full ledger",async({page})=>{
    const requests=[];await setup(page,route=>{const q=new URL(route.request().url()).searchParams;requests.push(Object.fromEntries(q));return fulfillJson(route,fixture(q.get("server")||"example-host"));});
    const ledgerRequests=[];page.on("request",r=>{if(r.url().includes("/api/vulnerabilities?"))ledgerRequests.push(new URL(r.url()).searchParams.get("page_size"));});
    await page.goto("/vulnerabilities?server=example-host&recent_days=7");
    await page.locator("#security-search").fill("example");await expect.poll(()=>requests.at(-1)?.q).toBe("example");
    await page.getByRole("tab",{name:/Available fixes/}).focus();await page.keyboard.press("ArrowRight");
    await expect(page.getByRole("tab",{name:/Priority alerts/})).toHaveAttribute("aria-selected","true");
    await expect.poll(()=>requests.at(-1)?.view).toBe("alerts");
    await expect(page.locator("#security-search")).toHaveValue("example");
    await expect(page.locator("#security-recent")).toHaveValue("7");
    await page.locator("#security-server").selectOption("other-host");
    await expect(page.locator("#cve-plan-link")).toHaveAttribute("href","/remediation?server=other-host");
    await expect(page.locator("#security-ledger-link")).toHaveAttribute("href","/vulnerabilities/all?server=other-host&q=example");
    await page.goBack();await expect(page.locator("#security-server")).toHaveValue("example-host");
    await page.goBack();await expect(page.getByRole("tab",{name:/Available fixes/})).toHaveAttribute("aria-selected","true");
    await page.goForward();await expect(page.getByRole("tab",{name:/Priority alerts/})).toHaveAttribute("aria-selected","true");
    expect(ledgerRequests.length).toBeGreaterThan(0);expect(ledgerRequests.every(v=>v==="1")).toBe(true);
    await page.locator("#security-ledger-link").click();
    await expect(page).toHaveURL(/\/vulnerabilities\/all\?server=example-host&q=example$/);
    await expect(page.locator("html")).toHaveAttribute("lang","en");
    await expect(page.getByRole("heading",{name:"All CVEs",exact:true})).toBeVisible();
    await expect(page.locator("#cve-search")).toHaveValue("example");
    await expect(page.locator("#cve-actions-link")).toHaveAttribute("href","/vulnerabilities?server=example-host&q=example");
    await expect(page.locator("#security-filters")).toHaveCount(0);
});
test("coverage warnings are aggregated by server and explicit empty states retain uncertainty",async({page})=>{
    await setup(page,route=>fulfillJson(route,{...fixture(),fixes:[],alerts:[],fix_total:0,alert_total:0,review_findings:8688}));
    await page.unroute("**/api/vulnerabilities?**");
    await page.route("**/api/vulnerabilities?**",route=>fulfillJson(route,{...coverage(),coverage:[...coverage().coverage,{server:"CryptoNode",state:"complete",freshness:"fresh",reason:"source_coverage_unavailable"}]}));
    await page.goto("/vulnerabilities");
    await expect(page.locator("#security-review")).toHaveText("2 servers to review · 2 priority reviews");
    await expect(page.locator("#security-rows")).toContainText("does not mean no fix");
    await page.getByRole("button",{name:"Review evidence"}).click();
    await expect(page.locator("#security-health-hosts li")).toHaveCount(2);
    await expect(page.locator("#security-health-hosts")).toContainText("CVE data unavailable for this distribution");
    await expect(page.locator("#security-health-hosts")).not.toContainText("8688");
    await page.getByRole("tab",{name:/Priority alerts/}).click();
    await expect(page.locator("#security-rows")).toContainText("Unknown or stale data");
});
test("failed refresh retains only evidence for the same scope and coverage fails independently",async({page})=>{
    let fail=false;await setup(page,route=>fail?route.fulfill({status:503,body:"unavailable"}):fulfillJson(route,fixture()));
    await page.goto("/vulnerabilities");await expect(page.locator(".security-item")).toContainText("example-host");
    fail=true;await page.locator("#security-refresh").click();await expect(page.locator("#security-error")).toContainText("Previous results");
    await expect(page.locator(".security-item")).toContainText("example-host");
    await page.locator("#security-server").selectOption("other-host");await expect(page.locator("#security-error")).toContainText("HTTP 503");
    await expect(page.locator("#security-rows")).not.toContainText("example-host");
    fail=false;await page.unroute("**/api/vulnerabilities?**");await page.route("**/api/vulnerabilities?**",r=>r.fulfill({status:503,body:"unavailable"}));
    await page.locator("#security-refresh").click();await expect(page.locator(".security-item")).toContainText("example-host");
    await expect(page.locator("#security-review")).toContainText("Scan coverage unavailable");
    await expect(page.locator("#security-error")).toBeHidden();
});
test("lifecycle details preserve partial provenance, dates and conflicting versions",async({page})=>{
    const data=fixture(),recorded={...data.lifecycle[1],inventory_source:"recorded_findings",inventory_partial:true,inventory_freshness:"stale",inventory_observed_at:"2026-10-01T10:00:00Z"};
    data.lifecycle=[recorded,{...recorded,component:"nodejs",version:"",cycle:"",state:"unknown",inventory_version_state:"conflicting",eol_at:"",inventory_freshness:"unknown"}];
    await setup(page,r=>fulfillJson(r,data));await page.goto("/vulnerabilities?view=lifecycle");
    await expect(page.locator(".security-item").first()).toContainText("Partial inventory");
    await page.locator(".security-detail-toggle").first().click();
    await expect(page.locator(".security-evidence").first()).toContainText("partial historical inventory");
    await expect(page.locator(".security-evidence").first()).toContainText("2026-10-01T10:00:00Z");
    await page.locator(".security-detail-toggle").last().click();
    await expect(page.locator(".security-evidence").last()).toContainText("Conflicting versions");
    await expect(page.locator(".security-evidence").last()).toContainText("Unverified");
});
test("refresh applies a search still waiting for debounce",async({page})=>{
    await setup(page);
    let lastQuery;
    await page.route("**/api/security-actions?**",route=>{
        lastQuery=new URL(route.request().url()).searchParams.get("q");
        return fulfillJson(route,fixture());
    });
    await page.goto("/vulnerabilities");
    await expect(page.locator("#security-rows")).toContainText("example-host");
    // Input and refresh in the same event turn ensure the debounce is pending.
    await page.evaluate(()=>{
        const input=document.getElementById("security-search");input.value="example";
        input.dispatchEvent(new Event("input",{bubbles:true}));
        document.getElementById("security-refresh").click();
    });
    await expect(page.locator("#security-search")).toHaveValue("example");
    await expect.poll(()=>lastQuery).toBe("example");
    expect(new URL(page.url()).searchParams.get("q")).toBe("example");
    await page.evaluate(()=>{
        const input=document.getElementById("security-search");input.value="other";
        input.dispatchEvent(new Event("input",{bubbles:true}));
        const server=document.getElementById("security-server");server.value="other-host";
        server.dispatchEvent(new Event("change",{bubbles:true}));
    });
    await expect(page.locator("#security-search")).toHaveValue("other");
    await expect(page.locator("#cve-plan-link")).toHaveAttribute("href","/remediation?server=other-host");
    await expect.poll(()=>lastQuery).toBe("other");
});

test("a completing request does not replace a pending search draft",async({page})=>{
    await setup(page);
    let hold=false,release,lastQuery;
    await page.route("**/api/security-actions?**",async route=>{
        lastQuery=new URL(route.request().url()).searchParams.get("q");
        if(hold){hold=false;await new Promise(resolve=>{release=resolve;});}
        return fulfillJson(route,fixture());
    });
    await page.goto("/vulnerabilities");
    await expect(page.locator("#security-rows")).toContainText("example-host");
    await page.clock.install();
    hold=true;
    await page.locator("#security-refresh").click();
    await expect.poll(()=>typeof release).toBe("function");
    await page.evaluate(()=>{
        const input=document.getElementById("security-search");input.value="example";
        input.dispatchEvent(new Event("input",{bubbles:true}));
    });
    release();
    await expect(page.locator("#security-panel")).toHaveAttribute("aria-busy","false");
    await expect(page.locator("#security-search")).toHaveValue("example");
    await page.clock.fastForward(300);
    await expect.poll(()=>lastQuery).toBe("example");
    expect(new URL(page.url()).searchParams.get("q")).toBe("example");
});

test("queue navigation starts from the accepted page after results shrink",async({page})=>{
    await setup(page);
    await page.route("**/api/security-actions?**",route=>{
        const requested=Number(new URL(route.request().url()).searchParams.get("page")||1);
        return fulfillJson(route,{...fixture(),page:Math.min(requested,3),pages:3,lifecycle_total:60});
    });
    await page.goto("/vulnerabilities?action_page=5");
    await expect(page.locator("#security-page")).toHaveText("Page 3 / 3");
    await page.locator("#security-prev").click();
    await expect(page.locator("#security-page")).toHaveText("Page 2 / 3");
    expect(new URL(page.url()).searchParams.get("action_page")).toBe("2");
});

test("browser back cancels a search draft before restoring the previous scope",async({page})=>{
    await setup(page);
    const requests=[];
    await page.route("**/api/security-actions?**",route=>{
        const q=new URL(route.request().url()).searchParams;requests.push(Object.fromEntries(q));
        return fulfillJson(route,fixture(q.get("server")||"example-host"));
    });
    await page.goto("/vulnerabilities?server=example-host");
    await expect(page.locator("#security-server")).toHaveValue("example-host");
    await page.locator("#security-server").selectOption("other-host");
    await expect.poll(()=>requests.at(-1)?.server).toBe("other-host");
    await page.clock.install();
    await page.locator("#security-search").fill("abandoned draft");
    await page.goBack();
    await expect(page.locator("#security-server")).toHaveValue("example-host");
    await page.clock.fastForward(300);
    await expect(page.locator("#security-search")).toHaveValue("");
    expect(new URL(page.url()).searchParams.get("q")).toBeNull();
    await expect.poll(()=>requests.at(-1)?.server).toBe("example-host");
    expect(requests.at(-1)?.q).toBe("");
});


test("legacy investigation links redirect to the ledger and retain their filters",async({page})=>{
    await setup(page);
    await page.goto("/vulnerabilities?server=example-host&severity=critical#all-cves");
    await expect(page).toHaveURL(/\/vulnerabilities\/all\?server=example-host&severity=critical#all-cves$/);
    await expect(page.locator("#cve-severity")).toHaveValue("critical");
    await expect(page.locator("#cve-server")).toHaveValue("example-host");
    await expect(page.locator("#all-cves")).toBeVisible();
});

test("fix rows retain the known-exploitation priority when a patch is available",async({page})=>{
    const data=fixture();data.fixes[0]={...data.fixes[0],severity:"medium",known_exploited:true};
    await setup(page,route=>fulfillJson(route,data));await page.goto("/vulnerabilities");
    await expect(page.locator(".security-item")).toContainText("Known exploitation");
    await expect(page.locator(".security-item")).toContainText("Medium");
    await page.getByRole("button",{name:/^Fix details for/}).click();
    await expect(page.locator(".security-evidence")).toContainText("CISA KEV");
    await expect(page.locator("#security-tab-alerts")).toContainText("1");
});

test("a delayed coverage request does not block fixes or overwrite a newer server scope",async({page})=>{
    let release;
    await setup(page,route=>fulfillJson(route,fixture(new URL(route.request().url()).searchParams.get("server")||"example-host")));
    await page.unroute("**/api/vulnerabilities?**");
    await page.route("**/api/vulnerabilities?**",async route=>{
        const server=new URL(route.request().url()).searchParams.get("server");
        if(server==="example-host")await new Promise(resolve=>{release=resolve;});
        const data=coverage(server);if(server==="other-host")data.coverage[0].freshness="fresh";
        return fulfillJson(route,data);
    });
    try{
        await page.goto("/vulnerabilities?server=example-host");
        await expect.poll(()=>typeof release).toBe("function");
        await expect(page.locator(".security-item")).toContainText("example-host");
        await expect(page.locator("#security-panel")).toHaveAttribute("aria-busy","false");
        await expect(page.locator("#security-review")).toContainText("Scan coverage needs review");
        await page.locator("#security-server").selectOption("other-host");
        await expect(page.locator(".security-item")).toContainText("other-host");
        await expect(page.locator("#security-review")).toContainText("Scan coverage verified");
        release();
        await expect(page.locator("#security-health-hosts")).not.toContainText("example-host");
        await expect(page.locator("#security-review")).toContainText("Scan coverage verified");
    }finally{release?.();}
});

for(const coverageFails of [false,true])test(`late coverage ${coverageFails?"failure":"success"} preserves keyboard focus in expanded action evidence`,async({page})=>{
    let release;
    await setup(page);
    await page.unroute("**/api/vulnerabilities?**");
    await page.route("**/api/vulnerabilities?**",async route=>{
        await new Promise(resolve=>{release=resolve;});
        return coverageFails?route.fulfill({status:503,body:"unavailable"}):fulfillJson(route,coverage());
    });
    try{
        await page.goto("/vulnerabilities");
        const toggle=page.getByRole("button",{name:/^Fix details for/});
        await toggle.click();
        await expect(toggle).toBeFocused();
        await expect(page.locator(".security-evidence")).toBeVisible();
        release();
        await expect(page.locator("#security-review")).toContainText(coverageFails?"Scan coverage unavailable":"1 server to review");
        await expect(toggle).toBeFocused();
        await page.keyboard.press("Space");
        await expect(page.locator(".security-evidence")).toBeHidden();
    }finally{release?.();}
});

test("minimal support rows retain each server identity, dated support and partial provenance",async({page})=>{
    const data=fixture();
    data.lifecycle[1]={...data.lifecycle[1],inventory_source:"recorded_findings",inventory_partial:true,inventory_freshness:"stale"};
    data.lifecycle.push({...data.lifecycle[0],server:"other-host",component:"debian",version:"12",state:"extended_support_unverified",eol_at:"2026-06-10",extended_until:"2028-06-30",freshness:"stale"});
    data.lifecycle_total=3;
    await setup(page,route=>fulfillJson(route,data));
    await page.goto("/vulnerabilities?view=lifecycle");
    await expect(page.getByRole("columnheader",{name:"Support",exact:true})).toBeVisible();
    const rows=page.locator(".security-item");
    await expect(rows).toHaveCount(3);
    await expect(rows.nth(1)).toHaveClass(/security-repeat-server/);
    await expect(rows.nth(2)).not.toHaveClass(/security-repeat-server/);
    await expect(page.getByRole("cell",{name:"example-host",exact:true})).toHaveCount(2);
    const repeated=rows.nth(1).locator(".security-server-name");
    expect((await repeated.boundingBox()).width).toBeLessThanOrEqual(1);
    await expect(rows.first().locator(".security-component")).toContainText("Ubuntu");
    await expect(rows.first().locator(".security-component")).toContainText("25.04");
    await expect(rows.first().locator(".security-situation")).toContainText("Jan 17, 2026");
    await expect(rows.nth(1)).toContainText("Partial inventory, stale");
    await expect(rows.nth(2)).toContainText("Verify extended support");
    await expect(rows.nth(2)).toContainText("Standard support: Jun 10, 2026");
    await expect(rows.nth(2)).toContainText("Dates need review");
    await expect(page.locator("#security-navigation")).toBeHidden();
    await expect(page.locator("#security-status")).toHaveText("3 / 3 components");
    const details=page.getByRole("button",{name:"Support details for Ubuntu on example-host",exact:true});
    await details.focus();await page.keyboard.press("Enter");
    await expect(page.locator(".security-evidence").first()).toBeVisible();
    await expect(details).toBeFocused();
    await expect(page.locator(".security-evidence td").first()).toHaveAttribute("colspan","4");
    await page.keyboard.press("Space");
    await expect(page.locator(".security-evidence").first()).toBeHidden();
    for(const width of [390,320]){
        await page.setViewportSize({width,height:900});
        expect((await repeated.boundingBox()).width).toBeGreaterThan(1);
        expect(await page.evaluate(()=>document.body.scrollWidth<=document.documentElement.clientWidth)).toBe(true);
    }
});

test("English CVE labels and calendar dates remain consistent with a French browser west of UTC",async({browser,baseURL})=>{
    const context=await browser.newContext({timezoneId:"America/Toronto",locale:"fr-CA",baseURL});
    const page=await context.newPage();
    try{
        await setup(page);await page.goto("/vulnerabilities?view=lifecycle");
        await expect(page.getByRole("tab",{name:/End of support/})).toHaveAttribute("aria-selected","true");
        await expect(page.locator(".security-item").first().locator(".security-situation")).toContainText("Jan 17, 2026");
    }finally{await context.close();}
});
