const {test,expect} = require("@playwright/test");
const {spawn,execFileSync} = require("node:child_process");
const fs = require("node:fs/promises");
const path = require("node:path");
const os = require("node:os");
const net = require("node:net");

test("production installation health is public, admin cookie diagnostics refresh and remain stale on failure", async ({page, browser}, testInfo)=>{
 let binary=process.env.SLU_INSTALLATION_HEALTH_BINARY;
 let buildDirectory;
 if (!binary) {buildDirectory=await fs.mkdtemp(path.join(os.tmpdir(),"slu-health-binary-494-"));binary=path.join(buildDirectory,"webserver");execFileSync("go",["build","-o",binary,"."],{timeout:60000});}
 const directory=await fs.mkdtemp(path.join(os.tmpdir(),"slu-health-494-"));
 const port=await new Promise((resolve,reject)=>{const socket=net.createServer();socket.on("error",reject);socket.listen(0,"127.0.0.1",()=>{const port=socket.address().port;socket.close(()=>resolve(port));});});
 const url=`http://127.0.0.1:${port}`;
 const env={...Object.fromEntries(Object.entries(process.env).filter(([key])=>!key.startsWith("DEBIAN_UPDATER_"))),GIN_MODE:"release",DEBIAN_UPDATER_LISTEN_ADDR:`127.0.0.1:${port}`,DEBIAN_UPDATER_DB_PATH:path.join(directory,"servers.db"),DEBIAN_UPDATER_HOST_FACTS_AUTO_REFRESH_ENABLED:"false",DEBIAN_UPDATER_VULNERABILITY_SCAN_ENABLED:"false"};
 let log="";
 const child=spawn(binary,[],{cwd:path.resolve("."),env,stdio:["ignore","pipe","pipe"]});
 child.stdout.on("data",data=>{log+=data});child.stderr.on("data",data=>{log+=data});
 const exited=new Promise(resolve=>child.once("exit",(code,signal)=>resolve({code,signal})));
 try {
  await expect.poll(async()=>{try{return (await page.request.get(url+"/readyz")).status()}catch{return 0}},{timeout:10000}).toBe(200);
  const probe=await page.request.get(url+"/healthz");expect(await probe.json()).toEqual({status:"alive"});expect(probe.headers()["cache-control"]).toBe("no-store");
  expect((await page.request.get(url+"/api/installation/health")).status()).toBe(401);
  execFileSync(binary,["healthcheck"],{env,timeout:3000}); // Works while installation lock is held.
  await page.goto(url+"/setup");
  const setup=await page.evaluate(async()=>{const response=await fetch("/api/auth/setup",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({username:"admin",password:"DisposableHealth494!"})});return response.status});expect(setup).toBe(200);
  await page.goto(url+"/admin#admin-section-installation");
  const section=page.locator("#admin-section-installation");await expect(section).toBeVisible();
  await expect(page.locator("#installation-state")).toHaveText("ready");
  await expect(page.locator("#installation-workers tr")).toHaveCount(7);
  await expect(page.locator("#installation-workers tr").filter({hasText:"host_facts"})).toContainText("Disabled");
  await expect(page.locator("#installation-workers tr").filter({hasText:"vulnerabilities"})).toContainText("Disabled");
  await expect(page.locator("#installation-backup")).toContainText("never");
  await expect(page.locator("#installation-storage")).toContainText("bytes");
  const diagnostic=await page.request.get(url+"/api/installation/health");expect(diagnostic.status()).toBe(200);expect(diagnostic.headers()["cache-control"]).toContain("no-store");const facts=await diagnostic.json();expect(facts.workers.scheduler.last_tick).not.toMatch(/^0001-/);expect(facts.notifications.pending).toBe(0);expect(facts.verified_backup_age_seconds).toBeNull();expect(JSON.stringify(facts)).not.toContain(directory);
  const rotated=await page.evaluate(async()=>{const response=await fetch("/api/metrics/token",{method:"POST"});return {status:response.status,data:await response.json()}});expect(rotated.status).toBe(200);
  const metrics=await page.request.get(url+"/metrics",{headers:{Authorization:`Bearer ${rotated.data.token}`}});expect(metrics.status()).toBe(200);const text=await metrics.text();expect(text).toContain("slu_installation_ready 1");expect(text).toContain("simplelinuxupdater_update_runs");expect(text).not.toContain(rotated.data.token);expect(text).not.toContain(directory);
  expect((await page.request.get(url+"/api/installation/health",{headers:{Authorization:`Bearer ${rotated.data.token}`}})).status()).toBe(401);
  const route=url+"/api/installation/health";
  await page.route(route,route=>route.fulfill({status:503,contentType:"application/json",body:'{"error":"unavailable"}'}));
  await section.getByRole("button",{name:"Refresh",exact:true}).click();
  await expect(section.locator("[data-admin-section-status]")).toHaveText("Stale");await expect(page.locator("#installation-state")).toHaveText("ready");
  await page.unroute(route);
  await section.getByRole("button",{name:"Retry",exact:true}).click();
  await expect(section.locator("[data-admin-section-status]")).toHaveText("Current");
  await page.setViewportSize({width:390,height:844});await section.scrollIntoViewIfNeeded();await expect(page.locator("#installation-state")).toBeVisible();
  await page.screenshot({path:testInfo.outputPath("installation-health-mobile.png"),fullPage:false});
  const anonymous=await browser.newContext();try{const response=await anonymous.request.get(url+"/api/installation/health");expect(response.status()).toBe(401)}finally{await anonymous.close()}
 } finally {
  child.kill("SIGTERM");const timer=setTimeout(()=>child.kill("SIGKILL"),12000);const exit=await exited;clearTimeout(timer);
  await testInfo.attach("production-runtime-log",{body:log,contentType:"text/plain"});
  await fs.rm(directory,{recursive:true,force:true});if(buildDirectory)await fs.rm(buildDirectory,{recursive:true,force:true});expect(exit.code,log).toBe(0);
 }
});
