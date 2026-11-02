(function(root){
    "use strict";
    let current;
    const views={fixes:{title:"Available fixes",help:"Confirmed fix, with installation simulated before approval.",noun:"fixes",total:"fix_total",empty:"Stale or missing evidence does not mean no fix has been published."},alerts:{title:"Priority alerts without a fix",help:"Recent critical advisories or known exploitation, without an announced fix.",noun:"alerts",total:"alert_total",empty:"Unknown or stale data still needs review."},lifecycle:{title:"OS and application support",help:"Support for observed versions. Check Debian/Ubuntu fixes separately.",noun:"components",total:"lifecycle_total",empty:"Software missing from inventory remains outside coverage."}};
    const labels={end_of_life:"Ended",upstream_end_of_life:"Upstream support ended",extended_support_unverified:"Verify extended support",ending_soon:"Ends within 90 days",supported:"Supported",unknown:"Needs review"};
    const freshness={fresh:"Fresh",stale:"Stale",unavailable:"Unavailable",unknown:"Unverified"};
    const severity={critical:"Critical",high:"High",medium:"Medium",low:"Low",none:"None",unknown:"Unknown"};
    function mount(){
        current?.destroy();
        if(location.hash==="#all-cves"||["severity","fix","distro","min_age","sort","page"].some(k=>new URLSearchParams(location.search).has(k))){location.replace("/vulnerabilities/all"+location.search+location.hash);return;}
        const life=root.BrowserEffects.create();life.init();
        const defaults={server:"",q:"",recent_days:30,view:"fixes",page:1};
        function locationQuery(){const p=new URLSearchParams(location.search);p.set("page",p.get("action_page")||"1");const q=root.CVEPageInteraction.parseQuery(p.toString(),defaults);if(!views[q.view])q.view="fixes";return q;}
        const store=root.CVEPageInteraction.createStore("",defaults);store.change(locationQuery());
        const el=id=>document.getElementById(id),tabs=Array.from(document.querySelectorAll("[data-view]"));
        const esc=v=>String(v??"").replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
        const note=v=>`<span class="security-note">${esc(v)}</span>`;
        const calendar=new Intl.DateTimeFormat("en-CA",{day:"numeric",month:"short",year:"numeric",timeZone:"UTC"});
        function supportDate(value){
            if(!/^\d{4}-\d{2}-\d{2}$/.test(value||""))return "";
            const date=new Date(value+"T00:00:00Z");
            return Number.isNaN(date.getTime())||date.toISOString().slice(0,10)!==value?"":calendar.format(date);
        }
        const path=(base,values)=>base+"?"+new URLSearchParams(values).toString();
        const plan=server=>path("/remediation",{server});
        const ledger=(server,q="")=>path("/vulnerabilities/all",{server,q});
        const urlLink=(url,label)=>{try{const u=new URL(url);if(!["https:","http:"].includes(u.protocol))return "";return `<a href="${esc(u.href)}" target="_blank" rel="noopener noreferrer">${esc(label)}</a>`;}catch{return "";}};
        let controller,timer=null,coverage=null,coverageError="",expanded=new Set();
        function sync(){const q=store.view().query;if(timer===null)el("security-search").value=q.q;const select=el("security-server");if(q.server&&!Array.from(select.options).some(o=>o.value===q.server))select.add(new Option(q.server,q.server));select.value=q.server;el("security-recent").value=q.recent_days;el("security-ledger-link").href=ledger(q.server,q.q);el("cve-plan-link").href=plan(q.server);
            for(const tab of tabs){const active=tab.dataset.view===q.view;tab.setAttribute("aria-selected",String(active));tab.tabIndex=active?0:-1;}el("security-panel").setAttribute("aria-labelledby",`security-tab-${q.view}`);el("security-options").hidden=q.view!=="alerts";el("security-list-title").textContent=views[q.view].title;el("security-list-help").textContent=views[q.view].help;el("security-situation-heading").textContent=q.view==="lifecycle"?"Support":"Status";}
        function health(data){
            const issues=(coverage?.coverage||[]).filter(h=>h.reason||h.state!=="complete"||h.freshness!=="fresh"||h.unknown||h.unsupported||h.error_count);
            const priority=data?.review_priority||0;
            const summary=coverageError?"Scan coverage unavailable":coverage?issues.length?`${issues.length} server${issues.length>1?"s":""} to review`:coverage.coverage.length?"Scan coverage verified":"No scans in this scope":"Scan coverage needs review";
            el("security-review").textContent=summary+(priority?` · ${priority} priority review${priority>1?"s":""}`:"");
            el("security-health").classList.toggle("needs-attention",!!issues.length||!!priority||!!coverageError||!coverage);
            el("security-health-status").textContent=coverageError?"Could not reload scan coverage. Retry with Refresh; absence of risk has not been established.":issues.length?"Review each server’s coverage before concluding that no fixes are available.":coverage?.coverage.length?"Stored scans are complete and recent. Fixes and alerts are assessed separately.":"Coverage will be known after stored results load.";
            el("security-health-hosts").innerHTML=issues.map(h=>{const reason=h.reason==="source_coverage_unavailable"?"CVE data unavailable for this distribution":h.reason==="unsupported_distribution"?"Unsupported distribution":h.state==="unavailable"?"Latest scan unavailable":h.state==="not_scanned"?"No recorded scan":h.state!=="complete"?"Incomplete scan":h.freshness!=="fresh"?"Stale results":(h.unknown||h.unsupported)?"Unevaluated packages":"Errors to review";return `<li><span><strong>${esc(h.server)}</strong>${note(reason)}</span><a href="${esc(ledger(h.server))}#cve-coverage">View coverage</a></li>`;}).join("");
        }
        function detail(f,view){
            const field=(label,value)=>`<div><dt>${esc(label)}</dt><dd>${esc(value||"Unknown")}</dd></div>`;
            if(view==="fixes")return `<dl>${field("Installed version",f.installed)}${field("Candidate version",f.candidate)}${field("Affected CVEs",f.cves.join(", ")+(f.cve_count>f.cves.length?` (sample of ${f.cve_count} CVEs)`:""))}${field("Simulated scopes",f.scopes.join(", "))}${field("Known exploitation",f.known_exploited?"CISA KEV — priority":"Not established")}${field("Evidence observed",f.observed_at)}</dl><p>Preparation rechecks the transaction. Explicit approval and a new scan are still required to confirm remediation.</p>`;
            if(view==="alerts")return `<dl>${field("Installed package",`${f.package} ${f.installed}`)}${field("Advisory published",f.published_at)}${field("Match observed",f.observed_at)}${field("Known exploitation",f.known_exploited?`CISA KEV — ${freshness[f.exploitation_freshness]||"Unverified"}`:"Not established")}</dl><p>${esc(f.required_action||"Consult the vendor advisory for a temporary mitigation and document the server follow-up.")}</p><p>No fix is announced by the source for this distribution. A temporary mitigation is not proof of remediation and is not executed automatically.</p><div class="security-detail-links">${urlLink(f.source,"Read advisory")}<a href="${esc(ledger(f.server,f.cves[0]))}">View CVE evidence</a><a href="${esc(plan(f.server))}">Record follow-up</a></div>`;
            return `<dl>${field("Observed version",f.version)}${field("Cycle",f.cycle)}${field("End of support",f.eol_at)}${field("Possible extended support until",f.extended_until)}${field("Metadata observed",f.observed_at)}${field("Metadata freshness",freshness[f.freshness]||"Unverified")}${field("Inventory freshness",freshness[f.inventory_freshness]||"Unverified")}${field("Package observed",f.inventory_observed_at)}</dl>${f.inventory_source==="recorded_findings"?"<p>Recorded CVE findings: partial historical inventory. This does not confirm the currently installed version or the presence of every application.</p>":""}${f.inventory_version_state==="conflicting"?"<p>Conflicting versions at the same observation time: version and release cycle are unknown.</p>":""}<p>${f.kind==="application"?"This calendar describes upstream support. Check Debian/Ubuntu fixes separately.":"Verify extended support for this server; its availability does not prove an active entitlement."}</p>${urlLink(f.source,"View endoflife.date calendar")}`;
        }
        function rows(data,view){
            return data[view].map((f,i)=>{
                const id=`security-detail-${i}`,open=expanded.has(i),isLife=view==="lifecycle";
                const component=isLife?({ubuntu:"Ubuntu",debian:"Debian"}[f.component]||f.component):f.package;
                const repeat=i>0&&data[view][i-1].server===f.server;
                let situation,annotation,action,componentNote;
                if(view==="fixes"){const count=f.cve_count||f.cves.length;situation=`${count} CVE${count===1?"":"s"} with a verified fix`;annotation=(f.known_exploited?"Known exploitation · ":"")+(severity[f.severity]||severity.unknown);action=`<a class="security-row-primary" href="${esc(plan(f.server))}">Prepare remediation</a>`;}
                else if(view==="alerts"){situation=f.cves[0];annotation=f.known_exploited?"Known exploitation · no announced fix":"Critical · no announced fix";action="";}
                else{
                    situation=labels[f.state]||labels.unknown;
                    annotation=supportDate(f.eol_at);
                    if(annotation&&f.state==="extended_support_unverified")annotation="Standard support: "+annotation;
                    if(f.freshness!=="fresh")annotation+=(annotation?" — ":"")+"Dates need review";
                    action="";
                }
                if(isLife){
                    componentNote=note(f.version||"Version unknown");
                    if(f.inventory_partial)componentNote+=note(f.inventory_freshness==="stale"?"Partial inventory, stale":"Partial inventory");
                    else if(f.inventory_freshness!=="fresh")componentNote+=note("Version needs review");
                }else componentNote=note(view==="fixes"?`${f.installed} → ${f.candidate}`:f.installed);
                const danger=view==="alerts"||(isLife&&["end_of_life","upstream_end_of_life"].includes(f.state));
                const detailName=`${isLife?"Support details for":view==="alerts"?"Details and mitigations for":"Fix details for"} ${component} on ${f.server}`;
                return `<tr class="security-item${repeat?" security-repeat-server":""}"><td class="security-server" data-label="Server"><strong class="security-server-name">${esc(f.server)}</strong></td><td class="security-component" data-label="Component"><strong>${esc(component)}</strong>${componentNote}</td><td class="security-situation" data-label="${isLife?"Support":"Status"}"><strong class="${danger?"security-priority":""}">${esc(situation)}</strong>${annotation?note(annotation):""}</td><td class="security-row-controls" data-label="Actions"><div class="security-row-actions">${action}<button type="button" class="security-detail-toggle" data-detail="${i}" aria-label="${esc(detailName)}" aria-expanded="${open}" aria-controls="${id}">Details</button></div></td></tr><tr id="${id}" class="security-evidence" ${open?"":"hidden"}><td colspan="4"><div class="security-evidence-content">${detail(f,view)}</div></td></tr>`;
            }).join("");
        }
        function render(){
            const {list,query}=store.view(),data=list.data;
            if(data){const select=el("security-server");select.replaceChildren(new Option("All servers",""),...data.servers.map(s=>new Option(s,s)));}
            sync();health(data);
            el("security-panel").setAttribute("aria-busy",String(list.loading));
            el("security-error").hidden=!list.error;el("security-error").textContent=list.error?`${list.error}${data?" Previous results remain visible.":""} Retry with Refresh.`:"";
            for(const [id,key] of [["security-fix-count","fix_total"],["security-alert-count","alert_total"],["security-lifecycle-count","lifecycle_total"]])el(id).textContent=data?String(data[key]):"—";
            el("security-prev").disabled=list.loading||!data||data.page<=1;el("security-next").disabled=list.loading||!data||data.page>=data.pages;
            el("security-page").textContent=data?`Page ${data.page} / ${data.pages}`:"Page —";
            el("security-navigation").hidden=!data||data.pages<=1;
            el("security-status").textContent=list.loading?"Loading results…":data?`${data[query.view].length} / ${data[views[query.view].total]} ${views[query.view].noun}`:"Results unavailable";
            el("security-status").title=data?`Results loaded at ${new Date(data.generated_at).toLocaleString("en-CA")}`:"";
            el("security-rows").innerHTML=data?(data[query.view].length?rows(data,query.view):`<tr><td colspan="4" class="security-empty"><strong>${query.view==="fixes"?"No confirmed fixes":query.view==="alerts"?"No confirmed alerts":"No components match this filter"}</strong><p>${esc(views[query.view].empty)}</p><button type="button" class="btn-ghost" data-checks>Review evidence</button></td></tr>`):`<tr><td colspan="4" class="security-empty">${list.loading?"Loading…":"Results unavailable. Retry with Refresh."}</td></tr>`;
            el("security-feeds").innerHTML=data?Object.entries(data.feeds).map(([name,f])=>`<li>${esc(name)} : ${esc(freshness[f.state]||"Unverified")} — ${esc(f.observed_at||"Never verified")}${f.last_error?note("Latest refresh failed; previous observation retained."):""}</li>`).join(""):"";
        }
        async function json(url,signal){const response=await life.fetch(url,{signal});if(!response.ok){life.discardResponse(response);throw new Error(`Results unavailable (HTTP ${response.status})`);}return response.json();}
        async function load(){
            controller?.abort();controller=new AbortController();const signal=controller.signal,id=store.begin("list"),ticket=life.capture(),q=store.view().query;render();
            // A slow coverage read must not hold back already accepted actions.
            // Both responses still belong to the same scope/request generation.
            const isCurrent=()=>life.isCurrent(ticket)&&!signal.aborted&&store.view().list.id===id;
            const actions=json(path("/api/security-actions",{...q,page_size:25}),signal).then(data=>{
                if(!isCurrent()||!store.receive("list",id,data))return;
                expanded.clear();render();
            },error=>{
                if(isCurrent()&&store.receive("list",id,null,error.message))render();
            });
            const scans=json(path("/api/vulnerabilities",{server:q.server,page_size:1}),signal).then(data=>{
                if(!isCurrent())return;
                coverage=data;coverageError="";health(store.view().list.data);
            },error=>{
                if(!isCurrent())return;
                coverageError=error.message;health(store.view().list.data);
            });
            await Promise.allSettled([actions,scans]);
        }
        function clearDraft(){if(timer!==null){life.clearTimeout(timer);timer=null;}}
        function change(patch){
            clearDraft();const old=store.view().query,next={q:el("security-search").value,...patch};
            if(next.q!==old.q||(next.server!==undefined&&next.server!==old.server)||(next.view!==undefined&&next.view!==old.view))next.page=1;
            store.change(next);coverage=null;coverageError="";expanded.clear();
            const q=store.view().query,p=new URLSearchParams();for(const key of ["server","q","view","recent_days"]){if(String(q[key])!==String(defaults[key]))p.set(key,q[key]);}if(q.page>1)p.set("action_page",q.page);
            const search=p.toString();history.pushState({},"",location.pathname+(search?"?"+search:""));load();
        }
        function refresh(){clearDraft();if(el("security-search").value!==store.view().query.q)change({});else load();}
        life.listen(el("security-filters"),"submit",e=>{e.preventDefault();refresh();});
        life.listen(el("security-server"),"change",()=>change({server:el("security-server").value}));
        life.listen(el("security-recent"),"change",()=>change({recent_days:el("security-recent").value}));
        life.listen(el("security-search"),"input",()=>{clearDraft();timer=life.setTimeout(()=>change({}),250);});
        life.listen(el("security-refresh"),"click",refresh);
        life.listen(el("security-prev"),"click",()=>change({page:store.view().list.data.page-1}));
        life.listen(el("security-next"),"click",()=>change({page:store.view().list.data.page+1}));
        for(const tab of tabs){life.listen(tab,"click",()=>change({view:tab.dataset.view}));life.listen(tab,"keydown",e=>{let i=tabs.indexOf(tab);if(e.key==="ArrowRight")i=(i+1)%tabs.length;else if(e.key==="ArrowLeft")i=(i+tabs.length-1)%tabs.length;else if(e.key==="Home")i=0;else if(e.key==="End")i=tabs.length-1;else return;e.preventDefault();tabs[i].focus();change({view:tabs[i].dataset.view});});}
        life.listen(el("security-rows"),"click",e=>{const button=e.target.closest("[data-detail]");if(button){const i=Number(button.dataset.detail);if(expanded.has(i))expanded.delete(i);else expanded.add(i);button.setAttribute("aria-expanded",String(expanded.has(i)));el(`security-detail-${i}`).hidden=!expanded.has(i);}if(e.target.closest("[data-checks]")){el("security-health").open=true;el("security-health").querySelector("summary").focus();}});
        life.listen(root,"popstate",()=>{clearDraft();controller?.abort();store.change(locationQuery());coverage=null;coverageError="";expanded.clear();load();});
        current={destroy(){clearDraft();controller?.abort();life.destroy();}};load();
    }
    root.addEventListener("pageshow",e=>{if(e.persisted)mount();});root.addEventListener("pagehide",e=>{if(!e.persisted)current?.destroy();});mount();
})(window);
