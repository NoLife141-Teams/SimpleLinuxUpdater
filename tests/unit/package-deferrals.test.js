const test=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');

test('package deferral presentation retains exact decisions and escapes operator text',()=>{
    const context=vm.createContext({});
    vm.runInContext(fs.readFileSync(path.join(__dirname,'../../static/js/package-deferrals.js'),'utf8'),context);
    const revision='a'.repeat(64);
    const html=context.PackageDeferrals.approvalHTML({package_deferrals:{revision,rules:[{selector:'openssl:amd64',origin:'Policy: Production (revision 3)',reason:'<script>unsafe</script>',author:'operator',expires_at:'2030-01-01T00:00:00Z'}]},deferral_scopes:{full_upgrade:{available:true,transaction:['curl:amd64=2:1.0~rc1','obsolete:amd64=-']},security:{available:false,block_reason:'Dependency conflict: openssl:amd64'}}});
    assert.match(html,/Install curl:amd64 version 2:1.0~rc1/);
    assert.match(html,/Remove obsolete:amd64/);
    assert.match(html,/&lt;script&gt;/);
    assert.doesNotMatch(html,/<script>/);
    assert.doesNotMatch(html,/<p>Revision /);
    assert.match(html,new RegExp('/api/package-deferral-snapshots/'+revision));
    assert.match(html,/Policy: Production \(revision 3\)/);
    assert.match(html,/Deferred security fixes remain exposed/);
});
