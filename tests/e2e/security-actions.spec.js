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

test("lifecycle distinguishes partial recorded inventory, observation dates and conflicting versions",async({page})=>{
    await ensureAuthenticatedSession(page);
    const data=fixture();
    const recorded={...data.lifecycle[1],inventory_source:"recorded_findings",inventory_partial:true,inventory_freshness:"stale",inventory_observed_at:"2026-10-01T10:00:00Z"};
    data.lifecycle=[recorded,{...recorded,component:"nodejs",product:"nodejs",version:"",cycle:"",state:"unknown",inventory_version_state:"conflicting",eol_at:"",inventory_freshness:"unknown"}];
    await page.route("**/api/security-actions?**",route=>fulfillJson(route,data));
    await page.goto("/vulnerabilities");
    const rows=page.locator("#security-lifecycle tr");
    await expect(rows.first()).toContainText("Fin de support amont");
    await expect(rows.first()).toContainText("Constats CVE enregistrés · inventaire partiel et historique");
    await expect(rows.first()).toContainText("Paquet observé : 2026-10-01T10:00:00Z");
    await expect(rows.first()).toContainText("Inventaire : à actualiser");
    await expect(rows.last()).toContainText("Version inconnue");
    await expect(rows.last()).toContainText("Versions contradictoires à la même date");
    await expect(rows.last()).toContainText("Inventaire : non vérifié");
});

test("action and ledger scopes stay aligned while their pages and filters stay independent",async({page})=>{
    await ensureAuthenticatedSession(page);
    const requests={actions:[],ledger:[]};
    await page.route("**/api/security-actions?**",route=>{
        const q=new URL(route.request().url()).searchParams;requests.actions.push(Object.fromEntries(q));
        return fulfillJson(route,{...fixture(q.get("server")||"example-host"),page:Number(q.get("page")||1),pages:2});
    });
    await page.route("**/api/vulnerabilities?**",route=>{
        const q=new URL(route.request().url()).searchParams;requests.ledger.push(Object.fromEntries(q));
        return fulfillJson(route,{generated_at:"2026-11-02T12:00:00Z",rows:[],summary:{cves:0,hosts:0,critical_high:0,with_fix:0,unknown_severity:0,findings:0},servers:["example-host","other-host"],distros:[],coverage:[],total:0,page:Number(q.get("page")||1),pages:2,page_size:50});
    });
    await page.goto("/vulnerabilities?server=example-host&recent_days=7");
    await expect(page.locator("#security-server")).toHaveValue("example-host");
    await expect(page.locator("#cve-server")).toHaveValue("example-host");
    await page.locator("#security-server").selectOption("other-host");
    await expect(page.locator("#cve-server")).toHaveValue("other-host");
    await expect(page.locator("#cve-plan-link")).toHaveAttribute("href","/remediation?server=other-host");
    await expect.poll(()=>requests.ledger.at(-1)?.server).toBe("other-host");
    await page.locator("#all-cves > summary").click();
    await page.locator("#cve-server").selectOption("example-host");
    await expect(page.locator("#security-server")).toHaveValue("example-host");
    await expect.poll(()=>requests.actions.at(-1)?.server).toBe("example-host");
    await page.locator("#cve-search").fill("example");
    await expect(page.locator("#security-search")).toHaveValue("example");
    await expect.poll(()=>requests.actions.at(-1)?.q).toBe("example");
    expect(new URL(page.url()).searchParams.get("recent_days")).toBe("7");
    await page.locator("#cve-next").click();
    await expect(page.locator("#cve-page")).toContainText("2");
    await page.locator("#security-next").click();
    await expect(page.locator("#security-page")).toHaveText("Page 2 / 2");
    expect(new URL(page.url()).searchParams.get("page")).toBe("2");
    expect(new URL(page.url()).searchParams.get("action_page")).toBe("2");
    await page.reload();
    await expect(page.locator("#security-page")).toHaveText("Page 2 / 2");
    await expect(page.locator("#cve-page")).toContainText("2");
    await page.locator("#security-search").fill("different");
    await expect(page.locator("#cve-search")).toHaveValue("different");
    await expect(page.locator("#security-page")).toHaveText("Page 1 / 2");
    await expect(page.locator("#cve-page")).toContainText("1");
    expect(new URL(page.url()).searchParams.has("page")).toBe(false);
    expect(new URL(page.url()).searchParams.has("action_page")).toBe(false);
    await page.locator("#all-cves > summary").click();
    await page.locator("#cve-clear").click();
    await expect(page.locator("#security-search")).toHaveValue("");
    await expect(page.locator("#security-server")).toHaveValue("");
});

test("refresh applies a search still waiting for debounce",async({page})=>{
    await ensureAuthenticatedSession(page);
    let lastQuery;
    await page.route("**/api/security-actions?**",route=>{
        lastQuery=new URL(route.request().url()).searchParams.get("q");
        return fulfillJson(route,fixture());
    });
    await page.goto("/vulnerabilities");
    await expect(page.locator("#security-fixes")).toContainText("example-host");
    // Input and refresh in the same event turn ensure the debounce is pending.
    await page.evaluate(()=>{
        const input=document.getElementById("security-search");input.value="example";
        input.dispatchEvent(new Event("input",{bubbles:true}));
        document.getElementById("security-refresh").click();
    });
    await expect(page.locator("#security-search")).toHaveValue("example");
    await expect.poll(()=>lastQuery).toBe("example");
    expect(new URL(page.url()).searchParams.get("q")).toBe("example");
    await expect(page.locator("#cve-search")).toHaveValue("example");
    await page.evaluate(()=>{
        const input=document.getElementById("security-search");input.value="other";
        input.dispatchEvent(new Event("input",{bubbles:true}));
        const server=document.getElementById("security-server");server.value="other-host";
        server.dispatchEvent(new Event("change",{bubbles:true}));
    });
    await expect(page.locator("#security-search")).toHaveValue("other");
    await expect(page.locator("#cve-search")).toHaveValue("other");
    await expect(page.locator("#cve-plan-link")).toHaveAttribute("href","/remediation?server=other-host");
    await expect.poll(()=>lastQuery).toBe("other");
});

test("a completing request does not replace a pending search draft",async({page})=>{
    await ensureAuthenticatedSession(page);
    let hold=false,release,lastQuery;
    await page.route("**/api/security-actions?**",async route=>{
        lastQuery=new URL(route.request().url()).searchParams.get("q");
        if(hold){hold=false;await new Promise(resolve=>{release=resolve;});}
        return fulfillJson(route,fixture());
    });
    await page.goto("/vulnerabilities");
    await expect(page.locator("#security-fixes")).toContainText("example-host");
    await page.clock.install();
    hold=true;
    await page.locator("#security-refresh").click();
    await expect.poll(()=>typeof release).toBe("function");
    await page.evaluate(()=>{
        const input=document.getElementById("security-search");input.value="example";
        input.dispatchEvent(new Event("input",{bubbles:true}));
    });
    release();
    await expect(page.locator("#security-filters")).toHaveAttribute("aria-busy","false");
    await expect(page.locator("#security-search")).toHaveValue("example");
    await page.clock.fastForward(300);
    await expect.poll(()=>lastQuery).toBe("example");
    expect(new URL(page.url()).searchParams.get("q")).toBe("example");
});

test("queue navigation starts from the accepted page after results shrink",async({page})=>{
    await ensureAuthenticatedSession(page);
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
    await ensureAuthenticatedSession(page);
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

test("browser back cancels the ledger draft before restoring its independent page",async({page})=>{
    await ensureAuthenticatedSession(page);
    await page.route("**/api/security-actions?**",route=>{
        const q=new URL(route.request().url()).searchParams;
        return fulfillJson(route,fixture(q.get("server")||"example-host"));
    });
    await page.route("**/api/vulnerabilities?**",route=>{
        const q=new URL(route.request().url()).searchParams;
        return fulfillJson(route,{generated_at:"2026-11-02T12:00:00Z",rows:[],summary:{cves:0,hosts:0,critical_high:0,with_fix:0,unknown_severity:0,findings:0},servers:["example-host","other-host"],distros:[],coverage:[],total:0,page:Number(q.get("page")||1),pages:2,page_size:50});
    });
    await page.goto("/vulnerabilities?server=example-host&page=2");
    await expect(page.locator("#cve-page")).toHaveText("Page 2 of 2");
    await page.locator("#security-server").selectOption("other-host");
    await expect(page.locator("#cve-server")).toHaveValue("other-host");
    await page.locator("#all-cves > summary").click();
    await page.clock.install();
    await page.locator("#cve-search").fill("abandoned ledger draft");
    await page.goBack();
    await expect(page.locator("#cve-page")).toHaveText("Page 2 of 2");
    await page.clock.fastForward(300);
    expect(new URL(page.url()).searchParams.get("page")).toBe("2");
    await expect(page.locator("#cve-page")).toHaveText("Page 2 of 2");
    await expect(page.locator("#cve-server")).toHaveValue("example-host");
    await expect(page.locator("#security-server")).toHaveValue("example-host");
    await expect(page.locator("#cve-search")).toHaveValue("");
    await expect(page.locator("#security-search")).toHaveValue("");
});
