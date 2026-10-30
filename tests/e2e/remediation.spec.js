const {test,expect,ensureAuthenticatedSession,password}=require("./support/fixtures");
test.describe("remediation startup",()=>{
 test.use({scanTransportPhase:"remediation"});
 test("a delayed workflow script initializes the server selected by a fast plan response",async({page})=>{
  await ensureAuthenticatedSession(page);
  expect((await api(page,"/api/servers","POST",{name:"startup-host",host:"startup-host.invalid",user:"root",pass:"synthetic"})).status).toBe(201);
  await page.route("**/static/js/remediation-workflow.js*",async route=>{
   // Let a prematurely mounted page finish its request before the workflow
   // arrives. Correct startup waits for this dependency before requesting data.
   await page.waitForResponse(r=>r.url().includes("/api/vulnerabilities/plan?"),{timeout:1500}).catch(()=>{});
   await route.continue();
  });
  await page.goto("/remediation?server=startup-host");
  await expect(page.locator("#remediation-workflow")).toBeVisible();
  await expect(page.locator("#remediation-prepare")).toBeEnabled();
 });
});
async function api(page,path,method="GET",body){return page.evaluate(async({path,method,body})=>{const r=await fetch(path,{method,headers:{"Content-Type":"application/json"},body:body===undefined?undefined:JSON.stringify(body)});return {status:r.status,data:await r.json().catch(()=>({}))};},{path,method,body});}
async function prepareHost(page){await ensureAuthenticatedSession(page);expect((await api(page,"/api/servers","POST",{name:"example-host",host:"example-host.invalid",user:"root",pass:"synthetic"})).status).toBe(201);const key=await api(page,"/api/hostkeys/scan","POST",{host:"example-host.invalid",port:22});expect((await api(page,"/api/hostkeys/trust","POST",{host:"example-host.invalid",port:22,fingerprint_sha256:key.data.fingerprint_sha256})).status).toBe(200);expect((await api(page,"/api/servers/example-host/vulnerabilities/scan","POST")).status).toBe(202);await expect.poll(async()=>(await api(page,"/api/servers/example-host/vulnerabilities")).data.last_attempt.state).toBe("complete");await page.goto("/vulnerabilities?server=example-host");await expect(page.locator("#cve-count")).toHaveText("2");await page.getByRole("link",{name:"Correction plan",exact:true}).click();await expect(page).toHaveURL(/\/remediation\?server=example-host$/);await expect(page.locator("#plan-cves")).toHaveText("2");}
for(const phase of ["remediation","remediation-fail"]){test.describe(`real backend ${phase}`,()=>{test.use({scanTransportPhase:phase});test("manual immutable scope, verification and retry stay distinct",async({page})=>{
 const errors=[];page.on("pageerror",e=>errors.push(e.message));await prepareHost(page);
 await page.getByRole("button",{name:"Prepare remediation",exact:true}).click();await expect(page.locator("#remediation-operation")).toContainText("Operation: prepared");await expect(page.locator("#remediation-operation")).toContainText("extra-package");await expect(page.locator("#remediation-operation")).toContainText("1 potentially addressed CVEs");
 if(phase==="remediation-fail"){await page.route("**/api/vulnerabilities/plan?**",route=>route.fulfill({status:503,contentType:"application/json",body:JSON.stringify({error:"Synthetic temporary read failure"})}),{times:1});await page.getByRole("button",{name:"Refresh plan",exact:true}).click();await expect(page.locator("#plan-error")).toContainText("previous successful plan");await page.getByRole("button",{name:"Refresh plan",exact:true}).click();await expect(page.locator("#plan-error")).toBeHidden();}
 if(phase==="remediation-fail"){
 expect((await api(page,"/api/servers","POST",{name:"other-host",host:"other-host.invalid",user:"root",pass:"synthetic"})).status).toBe(201);
 await page.getByRole("button",{name:"Refresh plan",exact:true}).click();await expect(page.locator('#plan-server option[value="other-host"]')).toHaveCount(1);
 await page.route("**/api/vulnerabilities/plan?server=other-host&**",route=>route.fulfill({status:503,contentType:"application/json",body:JSON.stringify({error:"Synthetic Server read failure"})}),{times:1});
 await page.locator("#plan-server").selectOption("other-host");await expect(page.locator("#plan-error")).toBeVisible();await expect(page.locator("#remediation-workflow")).toBeHidden();
 await page.locator("#plan-server").selectOption("example-host");await expect(page.locator("#remediation-operation")).toContainText("Operation: prepared");}
 const history=(await api(page,"/api/servers/example-host/remediation")).data;expect(history.plans[0].state).toBe("prepared");expect((await api(page,"/api/servers/example-host/vulnerabilities")).data.findings.find(f=>f.package==="addressable").installed_version).toBe("1");
 await page.locator("#remediation-scope").selectOption("full_upgrade");await expect(page.locator("#remediation-scope-facts")).toContainText("new-dependency");await expect(page.locator("#remediation-scope-facts")).toContainText("new-dependency: not installed → 2");await expect(page.locator("#remediation-scope-facts")).toContainText("old-runtime");
 await page.locator("#remediation-scope").selectOption("all");await page.getByRole("button",{name:"Approve displayed scope",exact:true}).click();
 if(phase==="remediation-fail"){await expect(page.getByRole("button",{name:"Retry CVE verification only"})).toBeVisible({timeout:20000});const failed=(await api(page,"/api/servers/example-host/remediation")).data.plans[0];expect(failed.state).toBe("verification_failed");expect(failed.comparison).toBeUndefined();await page.getByRole("button",{name:"Retry CVE verification only"}).click();}
 await expect(page.locator("#remediation-operation")).toContainText("Operation: verified",{timeout:20000});await expect(page.locator("#remediation-operation")).toContainText("corrected: 1");await expect(page.locator("#remediation-operation")).toContainText("still_present: 1");const verified=(await api(page,"/api/servers/example-host/remediation")).data.plans[0];expect(verified.scan_id).not.toBe(verified.before_scan_id);expect(verified.job_id).toBe(history.plans[0].job_id);
 // Filtering recorded evidence never changes the approved transaction or host counts.
 await page.locator("#plan-search").fill("residual");await expect(page.locator("#plan-status")).toContainText("matching packages");expect((await api(page,"/api/servers/example-host/remediation")).data.plans[0].digest).toBe(verified.digest);
 await page.getByText("Document manual treatment or mitigation",{exact:true}).click();await page.locator('#remediation-review [name="package"]').fill("residual");await page.locator('#remediation-review [name="owner"]').fill("Example operations");await page.locator('#remediation-review [name="reason"]').fill("No installable correction established");await page.locator('#remediation-review [name="decision"]').fill("Review publisher advisory and monitor exposure");await page.locator('#remediation-review [name="reevaluate_at"]').fill("2026-11-30T10:00");await page.getByRole("button",{name:"Record risk review"}).click();await expect(page.locator("#remediation-reviews")).toContainText("Example operations");
 await page.setViewportSize({width:390,height:844});expect(await page.evaluate(()=>document.body.scrollWidth<=document.documentElement.clientWidth)).toBe(true);await page.screenshot({path:`/tmp/slu-531-${phase}-mobile.png`,fullPage:true});await page.setViewportSize({width:1440,height:1000});await page.screenshot({path:`/tmp/slu-531-${phase}-desktop.png`,fullPage:true});
 await page.goto("/vulnerabilities?server=example-host");await page.locator("button[data-cve]").first().focus();await page.keyboard.press("Enter");await expect(page.locator("#cve-detail")).toBeVisible();await page.keyboard.press("Escape");await expect(page.locator("button[data-cve]").first()).toBeFocused();expect(errors).toEqual([]);
 });});}
