const { test } = require("node:test");
const assert = require("node:assert/strict");
const { createStore } = require("../../static/js/manage-page-interaction.js");
function fixture() { const store = createStore(); store.dispatch({ type: "inventorySnapshotReceived", items: Array.from({length: 30}, (_,i) => ({name: `host-${String(i).padStart(2,"0")}`, host: "example.invalid", tags: [], port: 22, user: "root"})) }); return store; }
test("bulk selection is explicit, persists across pagination, and clears on filters", () => {
 const store = fixture();
 store.dispatch({type:"bulkSelectionChanged",name:"host-29",selected:true});
 assert.deepEqual(store.getView().inventoryBulk.selected, []);
 store.dispatch({type:"bulkSelectionChanged",name:"host-00",selected:true});
 store.dispatch({type:"pageChanged",page:2});
 store.dispatch({type:"bulkSelectionChanged",name:"host-29",selected:true});
 assert.deepEqual(store.getView().inventoryBulk.selected, ["host-00","host-29"]);
 store.dispatch({type:"filtersChanged",patch:{search:"host-29"}});
 assert.deepEqual(store.getView().inventoryBulk.selected, []);
});
test("bulk confirmation is distinct, invalidated by changes and admitted once", () => {
 const store=fixture();store.dispatch({type:"bulkSelectionChanged",name:"host-00",selected:true});store.dispatch({type:"bulkDraftChanged",patch:{addTags:"prod"}});
 assert.deepEqual(store.dispatch({type:"bulkCommitRequested"}),[]);
 const request=store.dispatch({type:"bulkPreviewRequested"})[0];
 assert.equal(request.kind,"preview");assert.deepEqual(request.body.names,["host-00"]);
 store.dispatch({type:"bulkPreviewReceived",generation:request.generation,result:{batch_id:"first",can_confirm:true,rows:[]}});
 assert.deepEqual(store.dispatch({type:"bulkCommitRequested"}),[]);
 store.dispatch({type:"bulkConfirmationRequested"});store.dispatch({type:"bulkDraftChanged",patch:{addTags:"other"}});
 assert.deepEqual(store.dispatch({type:"bulkCommitRequested"}),[]);
 const second=store.dispatch({type:"bulkPreviewRequested"})[0];store.dispatch({type:"bulkPreviewReceived",generation:second.generation,result:{batch_id:"second",can_confirm:true,rows:[]}});store.dispatch({type:"bulkConfirmationRequested"});
 assert.equal(store.dispatch({type:"bulkCommitRequested"})[0].body.batch_id,"second");assert.deepEqual(store.dispatch({type:"bulkCommitRequested"}),[]);
});
test("lost bulk response refreshes inventory and audit and requires a new preview",()=>{
 const store=fixture();store.dispatch({type:"bulkSelectionChanged",name:"host-00",selected:true});store.dispatch({type:"bulkDraftChanged",patch:{availability:"disable"}});const request=store.dispatch({type:"bulkPreviewRequested"})[0];store.dispatch({type:"bulkPreviewReceived",generation:request.generation,result:{batch_id:"one",can_confirm:true,rows:[]}});store.dispatch({type:"bulkConfirmationRequested"});store.dispatch({type:"bulkCommitRequested"});
 const effects=store.dispatch({type:"bulkRequestFailed",generation:request.generation,uncertain:true});assert.deepEqual(effects[0].streams,["inventory","audit"]);assert.equal(store.getView().inventoryBulk.preview,null);assert.match(store.getView().inventoryBulk.message,/uncertain/);assert.deepEqual(store.dispatch({type:"bulkCommitRequested"}),[]);
});
test("stale bulk preview responses cannot restore an invalidated confirmation",()=>{
 const store=fixture();store.dispatch({type:"bulkSelectionChanged",name:"host-00",selected:true});store.dispatch({type:"bulkDraftChanged",patch:{addTags:"x"}});const first=store.dispatch({type:"bulkPreviewRequested"})[0];store.dispatch({type:"bulkPreviewReceived",generation:first.generation,result:{batch_id:"old",can_confirm:true}});store.dispatch({type:"bulkDraftChanged",patch:{addTags:"y"}});store.dispatch({type:"bulkPreviewReceived",generation:first.generation,result:{batch_id:"old",can_confirm:true}});assert.equal(store.getView().inventoryBulk.preview,null);
});
