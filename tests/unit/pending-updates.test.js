const {test} = require('node:test');
const assert = require('node:assert/strict');
const {createStore, packageKey} = require('../../static/js/pending-updates-interaction.js');
const {render} = require('../../static/js/pending-updates-view.js');
const update = (name, extra = {}) => ({package:name, current_version:'1', candidate_version:'2', cve_state:'ready', ...extra});
const findings = count => Array.from({length:count},(_,i)=>({id:`CVE-2026-${String(i).padStart(4,'0')}`, disposition:'unknown', assessment:{}}));
const host = updates => ({name:'host',pending_updates:updates});

test('inspection filters and pages all packages without changing the approval plan',()=>{
    const store=createStore(), server=host(Array.from({length:17},(_,i)=>update(`package-${i}`,{security:i%2===0})));
    const before=JSON.stringify(server);
    assert.equal(store.view(server).packages.items.length,6);
    store.dispatch({action:'page',value:'next'},server);
    assert.equal(store.view(server).packages.start,7);
    store.dispatch({action:'filter',value:'security'},server);
    assert.equal(store.view(server).packages.total,9);
    store.dispatch({action:'search',value:'package-16'},server);
    assert.equal(store.view(server).packages.total,1);
    assert.equal(JSON.stringify(server),before);
    const empty=host([]);
    assert.equal(store.view(empty).packages.page,0);
    assert.equal(store.view(empty).packages.start,0);
});

test('selected evidence survives enrichment and reordered snapshots but not a different package version',()=>{
    const store=createStore(), packageA=update('same:amd64',{cve_findings:findings(14)}), packageB=update('same:arm64');
    let server=host([packageA,packageB]);
    store.dispatch({action:'package',value:packageKey(packageA)},server);
    store.dispatch({action:'cve-page',value:'next'},server);
    store.dispatch({action:'evidence',value:'CVE-2026-0006'},server);
    server={...server,pending_updates:[packageB,{...packageA,cve_coverage:'official',cve_scanned_at:'new'}]};
    assert.equal(store.view(server).evidence.id,'CVE-2026-0006');
    assert.equal(store.view(server).findings.start,6);
    server={...server,pending_updates:[{...packageA,candidate_version:'3'},packageB]};
    assert.equal(store.view(server).mode,'list');
    assert.equal(store.view(server).evidence,null);
    assert.equal(store.view({...server,name:'another'}).query,'');
});

test('CVE pagination, searching and one evidence selection remain bounded as findings change',()=>{
    const store=createStore(), pkg=update('example',{cve_findings:findings(44)});
    let server=host([pkg]);
    store.dispatch({action:'package',value:packageKey(pkg)},server);
    store.dispatch({action:'evidence',value:'CVE-2026-0000'},server);
    store.dispatch({action:'evidence',value:'CVE-2026-0001'},server);
    assert.equal(store.view(server).evidence.id,'CVE-2026-0001');
    store.dispatch({action:'cve-search',value:'0043'},server);
    assert.equal(store.view(server).findings.total,1);
    store.dispatch({action:'cve-search',value:''},server);
    for(let i=0;i<20;i++)store.dispatch({action:'cve-page',value:'next'},server);
    assert.equal(store.view(server).findings.end,44);
    server=host([{...pkg,cve_findings:findings(2)}]);
    assert.equal(store.view(server).findings.page,0);
    assert.equal(store.view(server).findings.items.length,2);
});

test('unknown references use one neutral explanation and retain qualifications and safe source evidence',()=>{
    const store=createStore(), pkg=update('example',{cve_findings:findings(44)}), server=host([pkg]);
    store.dispatch({action:'package',value:packageKey(pkg)},server);
    let html=render(store.view(server));
    assert.equal((html.match(/Distribution assessment needs review/g)||[]).length,1);
    assert.equal((html.match(/data-pending-action="evidence"/g)||[]).length,5);
    assert.ok(!html.includes('pending-cve-reported'));
    pkg.cve_findings[0]={...pkg.cve_findings[0], advisory_url:'javascript:alert(1)',installed_version:'old-proof',candidate_version:'new-proof',assessment:{vendor_status:'not_affected',vendor_reason:'<script>bad</script>'}};
    store.dispatch({action:'evidence',value:pkg.cve_findings[0].id},server);
    html=render(store.view(server));
    assert.ok(html.includes('Distribution: not_affected'));
    assert.ok(html.includes('&lt;script&gt;bad&lt;/script&gt;'));
    assert.ok(html.includes('old-proof → new-proof'));
    assert.ok(!html.includes('href="javascript:'));
    assert.ok(!html.includes('<script>'));
});

test('fixed, remaining and vendor-qualified findings keep their source comparison semantics',()=>{
    const store=createStore(), pkg=update('example',{cve_findings:[
        {id:'fixed',disposition:'fixed_by_candidate',assessment:{confidence:'vendor_confirmed'}},
        {id:'remaining',disposition:'still_affected',assessment:{vendor_status:'unimportant'}},
        {id:'withdrawn',disposition:'unknown',assessment:{vendor_status:'withdrawn'}},
    ]}),server=host([pkg]);
    store.dispatch({action:'package',value:packageKey(pkg)},server);
    const html=render(store.view(server));
    for(const text of ['Not reported for the proposed version','Still reported for the proposed version','Distribution: unimportant','Distribution: withdrawn','Distribution-confirmed source match'])assert.ok(html.includes(text));
});

test('Status owns inspection intent, requests an immediate drawer render, and exposes defensive data',()=>{
    const {createStore: createStatusStore}=require('../../static/js/status-page-interaction.js');
    const store=createStatusStore(), pkg=update('example',{cve_findings:findings(6)}), server=host([pkg]);
    store.dispatch({type:'serversSnapshotReceived',servers:[server]});
    store.dispatch({type:'drawerOpened',name:'host',tab:'pending'});
    const effects=store.dispatch({type:'pendingUpdatesChanged',intent:{action:'package',value:packageKey(pkg)}});
    assert.ok(effects.some(e=>e.type==='render'&&e.scope==='pendingUpdates'&&e.priority==='immediate'));
    const publicView=store.getView();publicView.pendingUpdates.selected.candidate_version='tampered';
    assert.equal(store.getView().pendingUpdates.selected.candidate_version,'2');
    assert.equal(store.getView().pendingUpdates.mode,'package');
});
