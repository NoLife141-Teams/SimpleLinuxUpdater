const fs = require("node:fs/promises");
const { test, expect, ensureAuthenticatedSession, password } = require("./support/fixtures");
const { startInstallation } = require("./support/installation");

async function downloadJSON(page, selector) {
    const pending = page.waitForEvent("download");
    await page.locator(selector).click();
    const download = await pending;
    expect(await download.failure()).toBeNull();
    return {name:download.suggestedFilename(),value:JSON.parse(await fs.readFile(await download.path(),"utf8"))};
}
async function seed(page, count) {
    const statuses = await page.evaluate(async count => {
        const statuses=[];
        for(let i=0;i<count;i++) statuses.push((await fetch("/api/servers",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({name:`export-${String(i).padStart(2,"0")}`,host:`export-${i}.example.test`,port:2222,user:"updater",tags:i===count-1?["hidden"]:["prod"]})})).status);
        return statuses;
    },count);
    expect(statuses.every(status=>status===201)).toBe(true);
    await page.goto("/manage");
    await expect(page.locator("#manage-servers-table tbody tr")).toHaveCount(Math.min(count,20));
}

test("inventory exports explicit selection, entire filtered view and all, then roundtrips into a second disposable installation",async({page,browser})=>{
    await ensureAuthenticatedSession(page);await seed(page,23);
    await page.setViewportSize({width:390,height:844});
    await page.locator("#tag-filter").fill("prod");
    await page.getByRole("checkbox",{name:"Select export-00 for bulk changes",exact:true}).focus();await page.keyboard.press("Space");
    await page.locator("#inventory-export-scope").selectOption("selection");
    await expect(page.locator("#inventory-export-count")).toContainText("1 hosts");
    let result=await downloadJSON(page,"#inventory-export-download");expect(result.value.map(row=>row.name)).toEqual(["export-00"]);
    await page.locator("#inventory-export-scope").selectOption("filtered");await expect(page.locator("#inventory-export-count")).toContainText("22 hosts");
    result=await downloadJSON(page,"#inventory-export-download");expect(result.value).toHaveLength(22);expect(result.value.some(row=>row.name==="export-22")).toBe(false);
    await page.locator("#inventory-export-scope").selectOption("all");await expect(page.locator("#inventory-export-count")).toContainText("23 hosts");
    result=await downloadJSON(page,"#inventory-export-download");expect(result.name).toBe("inventory-v1.json");expect(result.value).toHaveLength(23);
    for(const row of result.value)expect(Object.keys(row).sort()).toEqual(["host","name","port","tags","user"]);
    const installation=await startInstallation();const context=await browser.newContext({baseURL:installation.url});
    try {
        const destination=await context.newPage();await destination.goto(installation.url);await ensureAuthenticatedSession(destination);
        const imported=await destination.evaluate(async entries=>{
            const preview=await(await fetch("/api/servers/import/preview?format=json",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(entries)})).json();
            const result=await fetch("/api/servers/import",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({batch_id:preview.batch_id,rows:preview.rows.map(({errors,...row})=>row)})});
            const inventory=await(await fetch("/api/servers")).json();
            return {status:result.status,inventory};
        },result.value);
        expect(imported.status).toBe(201);expect(imported.inventory).toHaveLength(23);expect(imported.inventory.every(row=>row.disabled&&!row.has_key&&!row.has_password&&row.host_key_status!=="trusted")).toBe(true);
        await destination.goto("/manage");await destination.locator("#inventory-export-scope").selectOption("all");await expect(destination.locator("#inventory-export-count")).toContainText("23 hosts");
        expect((await downloadJSON(destination,"#inventory-export-download")).value).toEqual(result.value);
    } finally {await context.close();await installation.stop();}
    await page.locator("#tag-filter").fill("no-match");await page.locator("#inventory-export-scope").selectOption("filtered");await expect(page.locator("#inventory-export-count")).toContainText("0 hosts");expect((await downloadJSON(page,"#inventory-export-download")).value).toEqual([]);
});

test("audit downloads the full canonical filtered result beyond its displayed page, including CSV and empty results",async({page})=>{
    await ensureAuthenticatedSession(page);await seed(page,27);
    await page.locator('[data-manage-section-toggle="audit"]').click();
    await page.locator("#audit-action-filter").fill("server.create");await page.locator("#audit-actor-filter").fill("account:1");await page.locator("#audit-actor-filter").press("Tab");
    await page.locator("#audit-status-filter").selectOption("success");await expect(page.locator("#audit-page-info")).toContainText("27 events");
    await page.locator("#audit-next-page").click();await expect(page.locator("#audit-page-info")).toContainText("Page 2");
    const result=await downloadJSON(page,"#audit-export-json");expect(result.name).toBe("audit-v1.json");expect(result.value.items).toHaveLength(27);expect(result.value.manifest.count).toBe(27);expect(result.value.manifest.filters.actor).toBe("account:1");expect(result.value.manifest.filters.action).toBe("server.create");expect(result.value.items.every(row=>row.actor==="account:1"&&row.action==="server.create"&&row.status==="success")).toBe(true);
    const ids=result.value.items.map(row=>row.id);expect(ids).toEqual([...new Set(ids)].sort((a,b)=>b-a));
    const pending=page.waitForEvent("download");await page.locator("#audit-export-csv").click();const download=await pending;expect(download.suggestedFilename()).toBe("audit-v1.csv");const csv=await fs.readFile(await download.path(),"utf8");expect(csv).toContain("record_type,manifest_json");expect(csv).toContain("\r\nmanifest,");
    await page.locator("#audit-target-filter").fill("no-match");await expect(page.locator("#audit-page-info")).toContainText("0 events");const empty=(await downloadJSON(page,"#audit-export-json")).value;expect(empty.items).toEqual([]);expect(empty.manifest).toMatchObject({count:0,exported_from:"",exported_to:""});
});

test("Manage cancels downloads and real backend rejects reader and operator exports",async({page,browser,baseURL})=>{
    await ensureAuthenticatedSession(page);await seed(page,1);
    let release;const gate=new Promise(resolve=>{release=resolve});let reached;const arrived=new Promise(resolve=>{reached=resolve});
    await page.route("**/api/servers/export",async route=>{const response=await route.fetch();reached();await gate;try{await route.fulfill({response})}catch{}});
    await page.locator("#inventory-export-scope").selectOption("all");await page.locator("#inventory-export-download").click();await arrived;
    await page.locator("#inventory-export-cancel").click();await expect(page.locator("#inventory-export-feedback")).toContainText("cancelled");release();await page.unroute("**/api/servers/export");
    for(const role of ["reader","operator"]){
        expect((await page.request.post("/api/auth/accounts",{headers:{Origin:baseURL},data:{username:role,password,role}})).status()).toBe(201);
        const context=await browser.newContext({baseURL});
        try{const login=await context.request.post("/api/auth/login",{headers:{Origin:baseURL},data:{username:role,password}});expect(login.status()).toBe(200);for(const path of ["/api/servers/export","/api/audit-events/export"]){expect((await context.request.post(path,{headers:{Origin:baseURL},data:{}})).status()).toBe(403)}}finally{await context.close()}
    }
});
