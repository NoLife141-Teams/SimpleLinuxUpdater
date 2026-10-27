const {test} = require("node:test");
const assert = require("node:assert/strict");
const {createStore} = require("../../static/js/admin-page-interaction");
test("installation snapshot requests discard stale responses and preserve accepted facts on failure",()=>{
 const store=createStore();
 const first=store.dispatch({type:"snapshotRequested",stream:"installation"})[0].requestID;
 const second=store.dispatch({type:"snapshotRequested",stream:"installation"})[0].requestID;
 store.dispatch({type:"installationSnapshotReceived",requestID:first,data:{state:"ready"}});
 assert.equal(store.getView().installation,null);
 store.dispatch({type:"installationSnapshotReceived",requestID:second,receivedAt:"2026-10-27T00:00:00Z",data:{state:"ready",workers:{scheduler:{enabled:true,errors:2},"https://secret":{}},storage:{db_bytes:0},verified_backup_age_seconds:null}});
 const view=store.getView();assert.equal(view.installation.workers.length,7);assert.equal(view.installation.storage.dbBytes,0);assert.equal(view.installation.storage.walBytes,null);assert.equal(view.installation.backupAgeSeconds,null);assert.equal(view.streams.installation.status,"current");
 const third=store.dispatch({type:"snapshotRequested",stream:"installation"})[0].requestID;
 store.dispatch({type:"snapshotFailed",stream:"installation",requestID:third,error:"Unavailable"});
 assert.equal(store.getView().installation.state,"ready");assert.equal(store.getView().streams.installation.status,"stale");
 const section=store.getView().workspace.sections.find(item=>item.id==="installation");assert.ok(section.summary.includes("Stale")||section.summary.includes("stale"));
});
