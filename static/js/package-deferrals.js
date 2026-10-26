(function(root) {
    "use strict";
    const escape = value => String(value ?? "").replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
    function ruleHTML(rule) {
        return `<li><strong>${escape(rule.selector)}</strong> · ${escape(rule.origin)}<p>${escape(rule.reason)}</p><p>By ${escape(rule.author)} · created ${escape(rule.created_at)}<br>Expires ${escape(rule.expires_at_display || rule.expires_at)}</p><button type="button" class="btn-ghost" data-end-deferral="${escape(rule.id)}">${rule.policy_id ? "End policy deferral early (all matching Servers)" : "End deferral early"}</button></li>`;
    }
    function mount(area) {
        if (!area) return null;
        const drafts = new Map(); let owner=null, generation=0, controller=null, busy=false;
        const query = field => area.querySelector(`[data-deferral-field="${field}"]`);
        const feedback=area.querySelector("[data-deferral-feedback]");
        const list=area.querySelector("[data-deferral-list]");
        function key(){return owner?.server_name ? `server:${owner.server_name}` : `policy:${owner?.policy_id || ""}`;}
        function retain(){if(owner)drafts.set(key(),{selector:query("selector").value,reason:query("reason").value,expires_at:query("expires_at").value});}
        function disable(value){busy=value;for(const el of area.querySelectorAll("input,button"))el.disabled=value || !owner;}
        async function load(expected=generation) {
            controller?.abort();controller=new AbortController();list.textContent="Loading saved package deferrals…";
            try {
                const url=owner.server_name?`/api/servers/${encodeURIComponent(owner.server_name)}/package-deferrals`:"/api/package-deferrals";
                const response=await fetch(url,{cache:"no-store",signal:controller.signal});const data=await response.json();
                if(expected!==generation)return;if(!response.ok)throw new Error(data.error || "Could not load deferrals.");
                // The server resolves inheritance and expiration. The adapter only
                // selects the policy being edited from its persisted response.
                const rules=owner.server_name?data.rules:data.rules.filter(r=>r.policy_id===owner.policy_id);
                list.innerHTML=rules.length?`<ul class="deferral-list">${rules.map(ruleHTML).join("")}</ul>`:"No saved package deferrals.";
            }catch(error){if(error.name!=="AbortError" && expected===generation)list.textContent=error.message;}
        }
        async function select(next){retain();generation++;controller?.abort();owner=next;const draft=drafts.get(key()) || {};for(const field of ["selector","reason","expires_at"])query(field).value=draft[field] || "";feedback.textContent=owner?"Rules are saved separately from inventory and policy drafts.":"Save the policy before adding package deferrals.";disable(false);if(owner)await load();else list.textContent="";}
        area.addEventListener("input",retain);
        area.addEventListener("click",async event=>{
            const button=event.target.closest("button");if(!button || busy || !owner)return;
            const current=generation;
            let url="/api/package-deferrals",options={};
            if(button.hasAttribute("data-add-deferral")) {
                const values={};for(const field of ["selector","reason","expires_at"]){values[field]=query(field).value;if(!values[field].trim()){feedback.textContent="Package, reason and expiration with timezone are required.";query(field).focus();return;}}
                options={method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({...owner,...values})};
            }else if(button.dataset.endDeferral) {
                url+=`/${encodeURIComponent(button.dataset.endDeferral)}`;options={method:"DELETE"};
            }else return;
            disable(true);feedback.textContent="Saving…";
            try {
                const response=await fetch(url,options);const data=await response.json();if(!response.ok)throw new Error(data.error || "Could not save package deferral.");
                if(current!==generation)return;
                if(options.method==="POST"){drafts.delete(key());for(const field of ["selector","reason","expires_at"])query(field).value="";}
                await load(current);feedback.textContent="Saved. Pending approval plans are recalculated; review them again.";
                area.querySelector("[data-add-deferral]").focus();
            }catch(error){if(current===generation)feedback.textContent=error.message;}
            finally{if(current===generation)disable(false);}
        });
        root.addEventListener("pagehide",()=>controller?.abort(),{once:true});
        root.addEventListener("beforeunload",event=>{retain();if([...drafts.values()].some(d=>d.selector || d.reason || d.expires_at)){event.preventDefault();event.returnValue="";}});
        disable(false);return Object.freeze({select,clear:()=>select(null)});
    }
    function approvalHTML(plan) {
        const rules=plan?.package_deferrals?.rules || [];if(!rules.length)return "";
        const labels={all:"Standard",security:"Standard security",security_kept_back:"Kept-back security",full_upgrade:"Full upgrade"};
        return `<section class="package-deferral-summary"><h4>Package deferrals</h4><p>Deferred security fixes remain exposed to CVEs. Approval is limited to the transactions below. Rule changes or expiry require a fresh approval.</p><ul>${rules.map(r=>`<li><strong>${escape(r.selector)}</strong> · ${escape(r.origin)} · ${escape(r.reason)} · by ${escape(r.author)} · expires ${escape(r.expires_at_display || r.expires_at)}</li>`).join("")}</ul><details><summary>Rule audit details</summary><a href="/api/package-deferral-snapshots/${encodeURIComponent(plan.package_deferrals.revision)}" target="_blank" rel="noopener">Recorded rules · ${escape(plan.package_deferrals.revision.slice(0,12))}</a></details>${Object.entries(plan.deferral_scopes || {}).map(([scope,p])=>`<p><strong>${labels[scope] || escape(scope)}:</strong> ${p.available?(p.transaction?.length?p.transaction.map(change=>{const split=change.lastIndexOf("=");const name=change.slice(0,split),version=change.slice(split+1);return version==="-"?`Remove ${escape(name)}`:`Install ${escape(name)} version ${escape(version)}`;}).join("; "):"No eligible packages"):escape(p.block_reason)}</p>`).join("")}</section>`;
    }
    root.PackageDeferrals=Object.freeze({mount,approvalHTML});
}(globalThis));
