const { test, expect, ensureAuthenticatedSession } = require("./support/fixtures");
async function seed(page) {
 for (const name of ["bulk-a","bulk-b","hidden-c"]) {
  expect(await page.evaluate(async name => (await fetch("/api/servers",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({name,host:`${name}.example.test`,user:"root",tags:["old"]})})).status,name)).toBe(201);
 }
 await page.goto("/manage");
 await expect(page.locator("#manage-servers-table tbody tr")).toHaveCount(3);
}
async function preview(page) {
 await page.locator("#bulk-preview-button").click();
 await expect(page.locator("#bulk-preview")).toBeVisible();
}
async function confirm(page) { await page.locator("#bulk-review-button").click();await expect(page.locator("#bulk-confirmation")).toBeVisible();await page.locator("#bulk-confirm").click(); }
test("bulk inventory real backend previews normalized values, confirms and supports keyboard/mobile filtering",async({page})=>{
 await ensureAuthenticatedSession(page);await seed(page);
 await page.setViewportSize({width:390,height:844});
 await page.locator("#search").fill("bulk-");
 await expect(page.locator("#manage-servers-table tbody tr")).toHaveCount(2);
 const choice=page.getByRole("checkbox",{name:"Select bulk-a for bulk changes",exact:true});await choice.focus();await page.keyboard.press("Space");
 await page.getByRole("checkbox",{name:"Select bulk-b for bulk changes",exact:true}).check();
 await expect(page.locator("#bulk-selection-count")).toContainText("2 selected");
 await page.locator("#bulk-add-tags").fill(" production ,production");await page.locator("#bulk-remove-tags").fill("old");await page.locator("#bulk-availability").selectOption("disable");
 await preview(page);await expect(page.locator("#bulk-preview-rows tr")).toHaveCount(2);await expect(page.locator("#bulk-preview-rows")).toContainText("Disabled · tags: production");await expect(page.locator("#bulk-notice")).toContainText("resumes normal scheduling");
 await page.locator("#bulk-review-button").focus();await page.keyboard.press("Enter");await expect(page.locator("#bulk-confirm-heading")).toBeFocused();
 const response=page.waitForResponse(r=>r.url().endsWith("/api/servers/bulk")&&r.request().method()==="POST");await page.locator("#bulk-confirm").click();expect((await response).status()).toBe(200);await expect(page.locator("#bulk-feedback")).toContainText("Updated 2 servers");
 const inventory=await page.evaluate(async()=>await(await fetch("/api/servers")).json());
 expect(inventory.find(s=>s.name==="hidden-c").tags).toEqual(["old"]);expect(inventory.filter(s=>s.name.startsWith("bulk-")).every(s=>s.disabled)).toBe(true);
 await page.getByRole("checkbox",{name:"Select bulk-a for bulk changes",exact:true}).check();await page.locator("#search").fill("hidden-c");await expect(page.locator("#bulk-selection-count")).toContainText("0 selected");
});
test("bulk real backend concurrent edit rejects the entire selection and requires another preview",async({page})=>{
 await ensureAuthenticatedSession(page);await seed(page);
 await page.getByRole("checkbox",{name:"Select bulk-a for bulk changes",exact:true}).check();await page.getByRole("checkbox",{name:"Select bulk-b for bulk changes",exact:true}).check();await page.locator("#bulk-add-tags").fill("prod");await preview(page);
 expect(await page.evaluate(async()=>(await fetch("/api/servers/bulk-a/availability",{method:"PUT",headers:{"Content-Type":"application/json"},body:'{"disabled":true}'})).status)).toBe(200);
 await confirm(page);await expect(page.locator("#bulk-feedback")).toContainText("changed");await expect(page.locator("#bulk-confirmation")).toBeHidden();
 const inventory=await page.evaluate(async()=>await(await fetch("/api/servers")).json());expect(inventory.every(s=>s.tags.join(",")==="old")).toBe(true);
 await preview(page);await confirm(page);await expect(page.locator("#bulk-feedback")).toContainText("Updated 2 servers");
});
test("bulk lost commit response refreshes real values and never blindly repeats a command",async({page})=>{
 await ensureAuthenticatedSession(page);await seed(page);await page.getByRole("checkbox",{name:"Select bulk-a for bulk changes",exact:true}).check();await page.locator("#bulk-add-tags").fill("prod");await preview(page);
 let commits=0;await page.route("**/api/servers/bulk",async route=>{commits++;await route.fetch();await route.abort("failed");});
 await confirm(page);await expect(page.locator("#bulk-feedback")).toContainText("Result uncertain");await expect(page.locator("#bulk-confirmation")).toBeHidden();await expect(page.locator("#manage-servers-table tbody tr").filter({has:page.getByRole("checkbox",{name:"Select bulk-a for bulk changes",exact:true})})).toContainText("prod");expect(commits).toBe(1);
 await page.unroute("**/api/servers/bulk");await preview(page);await expect(page.locator("#bulk-preview-rows")).toContainText("old, prod");
});
