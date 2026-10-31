(function (root) {
    "use strict";
    const defaults = {server:"",q:"",state:"",page:1};
    let current;
    function mount() {
        current?.destroy();
        const life=root.BrowserEffects.create();life.init();
        const store=root.CVEPageInteraction.createStore(location.search,defaults);
        const el=id=>document.getElementById(id);
        const esc=value=>String(value??"").replace(/[&<>"']/g,char=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[char]));
        const note=value=>`<span class="cve-cell-note">${esc(value)}</span>`;
        const actions={update_candidate:"Review candidate update",no_verified_fix:"No verified candidate fix",verify:"Verify evidence first",published_inaccessible:"Published fix inaccessible",applicability_review:"Review component applicability",vendor_qualified:"Vendor qualification: no demonstrated practical impact",external_owner:"Contact external component owner"};
        const assessments={candidate_fix:"Recorded candidate addresses this finding",remaining:"Would remain / no verified fix",unverified:"Evidence is unverified"};
        const dispositions={fixed_by_candidate:"Candidate recorded as a fix",still_affected:"Candidate still affected",no_fix_available:"No newer candidate available",candidate_unverified:"Candidate not verified"};
        const dialog=el("plan-detail"),controllers={};
        let timer=null,previousPackage="";
        function sync() {
            const q=store.view().query;
            el("plan-server").value=q.server;el("plan-search").value=q.q;el("plan-state").value=q.state;
            el("plan-search").disabled=!q.server;el("plan-state").disabled=!q.server;
            el("plan-back").href="/vulnerabilities"+(q.server?`?server=${encodeURIComponent(q.server)}`:"");
        }
        function render() {
            const {list,query}=store.view(),data=list.data;
            el("plan-table-wrap").setAttribute("aria-busy",String(list.loading));
            el("plan-error").hidden=!list.error;el("plan-error").textContent=list.error?`${list.error}${data?" Showing the previous successful plan.":" Use Refresh plan to retry."}`:"";
            el("plan-prev").disabled=list.loading||!data||data.page<=1;el("plan-next").disabled=list.loading||!data||data.page>=data.pages;
            el("plan-server-context").hidden=!data?.selected;
            if(!data){if(root.RemediationWorkflow?.server()!==query.server)root.RemediationWorkflow?.select("");el("plan-status").textContent=list.loading?"Loading recorded evidence…":"Plan evidence unavailable";el("plan-page").textContent="Page —";el("plan-rows").innerHTML='<tr><td>No accepted plan response yet.</td></tr>';return;}
            const names=query.server&&!data.servers.includes(query.server)?[query.server,...data.servers]:data.servers;
            el("plan-server").innerHTML='<option value="">Choose a server</option>'+names.map(name=>`<option value="${esc(name)}">${esc(name)}</option>`).join("");
            sync();
            el("plan-page").textContent=`Page ${data.page} of ${data.pages}`;
            if(!data.selected){
 root.RemediationWorkflow?.select("");
                el("plan-headings").innerHTML='<tr><th scope="col">Server / distribution</th><th scope="col">Known CVEs</th><th scope="col">Recorded candidate packages</th><th scope="col">Potentially addressed CVEs</th><th scope="col">Remaining / inconclusive CVEs</th><th scope="col">Evidence</th></tr>';
                el("plan-status").textContent=list.loading?"Refreshing plans…":`${data.total} servers · choose one to review its package plan`;
                el("plan-rows").innerHTML=data.hosts.length?data.hosts.map(host=>`<tr><td><button type="button" class="cve-open" data-plan-server="${esc(host.server)}">${esc(host.server)}</button>${note(host.distro||"Distribution unknown")}</td><td>${host.cves}</td><td>${host.candidate_packages}</td><td>${host.potential_cves}</td><td>${host.remaining_cves}</td><td>${esc(host.coverage.state)}${note(`Freshness: ${host.coverage.freshness}`)}${note(host.incomplete_coverage?"Incomplete / unavailable scan coverage":"Complete recorded coverage")}</td></tr>`).join(""):'<tr><td colspan="6">No servers in this snapshot.</td></tr>';
                return;
            }
            const host=data.selected,coverage=host.coverage;
 root.RemediationWorkflow?.select(host.server);
            el("plan-server-title").textContent=`${host.server} · ${host.distro||"Distribution unknown"}`;
            el("plan-recorded-at").textContent=`Accepted inventory: ${host.accepted_at||"none"} · Results loaded: ${new Date(data.generated_at).toLocaleString()}`;
            const reason=coverage.reason==="source_coverage_unavailable"?"CVE data unavailable for this distribution release. ":coverage.reason==="unsupported_distribution"?"Distribution not supported for CVE scanning. ":"";
            const notes=[reason+`${coverage.state} scan · ${coverage.freshness} evidence · ${coverage.evaluated}/${coverage.inventoried} packages evaluated · ${coverage.unknown} unknown · ${coverage.unsupported} unsupported`,...(coverage.errors||[])];
            if(host.needs_refresh)notes.push("Refresh recorded evidence before approving changes.");
            if(host.incomplete_coverage)notes.push("These estimates cover known findings only; unassessed packages may contain additional exposure.");
            el("plan-coverage-notice").textContent=notes.join(" · ");
            for(const[id,key]of Object.entries({"plan-cves":"cves","plan-packages":"packages","plan-candidates":"candidate_packages","plan-potential":"potential_cves","plan-remaining":"remaining_cves","plan-unverified":"unverified_cves"}))el(id).textContent=Number(host[key]).toLocaleString();
            el("plan-steps").innerHTML=host.steps.map(step=>`<li>${esc(step)}</li>`).join("");
            el("plan-headings").innerHTML='<tr><th scope="col">Package</th><th scope="col">Installed → candidate</th><th scope="col">Highest known severity</th><th scope="col">Recorded candidate-fix CVEs</th><th scope="col">Remaining CVEs</th><th scope="col">Unverified CVEs</th><th scope="col">Review step</th></tr>';
            el("plan-status").textContent=list.loading?"Refreshing package plan…":data.total?`${data.total} matching packages · ${(data.page-1)*data.page_size+1}–${Math.min(data.page*data.page_size,data.total)} shown · whole-server totals are unchanged by package filters`:"No package findings match this view. This does not establish absence of exposure; check scan coverage.";
            el("plan-rows").innerHTML=data.rows.length?data.rows.map(row=>`<tr><td><button type="button" class="cve-open" data-plan-package="${esc(row.package)}" ${row.package?"":"disabled"}>${esc(row.package||"Unknown package")}</button>${note(`${row.cves} known CVEs`)}${note(row.cve_sample.join(", "))}</td>
                <td class="plan-versions">${esc(row.installed_versions.join(", ")||"unknown")} → ${esc(row.candidate_versions.join(", ")||"none / unknown")}${row.version_conflict?note(`Versions conflict (${row.installed_variants} installed / ${row.candidate_variants} candidate variants)`):""}</td>
                <td><span class="cve-severity cve-severity-${esc(row.severity)}">${esc(row.severity)}</span></td><td>${row.fixed_cves}</td><td>${row.remaining_cves}</td><td>${row.unverified_cves}</td><td>${esc(actions[row.action]||actions.verify)}${row.needs_refresh?note("Refresh evidence before approval"):""}</td></tr>`).join(""):'<tr><td colspan="7">No package findings in this view. No correction is inferred from an incomplete or unavailable scan.</td></tr>';
        }
        function renderDetail() {
            const {selected,detail}=store.view();if(!selected)return;
            el("plan-detail-title").textContent=selected;
            el("plan-detail-error").hidden=!detail.error;el("plan-detail-error").textContent=detail.error?`${detail.error}${detail.data?" Previous evidence is retained.":" Close and reopen to retry."}`:"";
            const data=detail.data;
            el("plan-detail-prev").disabled=detail.loading||!data||data.page<=1;el("plan-detail-next").disabled=detail.loading||!data||data.page>=data.pages;
            if(!data){el("plan-detail-summary").textContent=detail.loading?"Loading recorded package evidence…":"Package evidence unavailable";el("plan-detail-rows").innerHTML="";el("plan-detail-page").textContent="Page —";return;}
            const row=data.rows[0];
            el("plan-detail-summary").textContent=`${actions[row.action]} · ${row.fixed_cves} candidate-fix CVEs · ${row.remaining_cves} remaining · ${row.unverified_cves} unverified${row.version_conflict?" · Versions conflict":""}${row.needs_refresh?" · Refresh evidence before approval":""}`;
            el("plan-detail-page").textContent=`Findings ${(data.page-1)*data.page_size+1}–${Math.min(data.page*data.page_size,data.total)} of ${data.total} · Page ${data.page} of ${data.pages}`;
            el("plan-detail-rows").innerHTML=(data.exposures||[]).map(f=>`<tr><td><a href="/vulnerabilities?${new URLSearchParams({server:f.server,q:f.id}).toString()}">${esc(f.id)}</a>${note(assessments[f.treatment]||assessments.unverified)}</td><td>${esc(f.installed_version||"unknown")} → ${esc(f.candidate_version||"none / unknown")}${note(`Source package: ${f.source_package||"unknown"}`)}</td><td>${esc(dispositions[f.disposition]||"Unknown fix state")}${note(`Fixed by source version: ${f.fixed_by_version||"unverified"}`)}${note(`Severity: ${f.severity_source?f.severity:"unknown"}`)}</td><td>${esc(f.source||"Source unknown")}${note(`Evidence: ${f.freshness||"unknown"}`)}${note(`Last seen: ${f.last_seen||"unknown"}`)}${note(`Severity source: ${f.severity_source||"unknown"}`)}</td></tr>`).join("");
        }
        async function load(kind) {
            controllers[kind]?.abort();const controller=new AbortController();controllers[kind]=controller;
            const id=store.begin(kind),ticket=life.capture(),view=store.view();
            const query=new URLSearchParams({...view.query,page_size:50,page:kind==="detail"?view.detailPage:view.query.page});
            if(kind==="detail")query.set("package",view.selected);
            if(kind==="list")render();else renderDetail();
            try{
                const response=await life.fetch(`/api/vulnerabilities/plan?${query}`,{signal:controller.signal});
                if(!response.ok){await response.text();throw new Error(`Plan evidence unavailable (HTTP ${response.status})`);}
                const data=await response.json();if(!life.isCurrent(ticket))return;
                if(store.receive(kind,id,data)){if(kind==="list")render();else renderDetail();}
            }catch(error){if(!life.isCurrent(ticket)||error.name==="AbortError")return;if(store.receive(kind,id,null,error.message)){if(kind==="list")render();else renderDetail();}}
        }
        function change(patch) {
            if(timer!==null){life.clearTimeout(timer);timer=null;}
            const next={q:el("plan-search").value,...patch};
            if(Object.hasOwn(next,"server")&&!next.server){next.q="";next.state="";}
            store.change(next);controllers.detail?.abort();if(dialog.open)dialog.close();
            const search=root.CVEPageInteraction.queryString(store.view().query,defaults);
            history.replaceState(null,"",location.pathname+(search?`?${search}`:""));sync();load("list");
        }
        life.listen(el("plan-filters"),"submit",event=>{event.preventDefault();change({});});
        life.listen(el("plan-filters"),"change",event=>{if(event.target.name&&event.target.name!=="q")change({[event.target.name]:event.target.value});});
        life.listen(el("plan-search"),"input",()=>{if(timer!==null)life.clearTimeout(timer);timer=life.setTimeout(()=>change({}),200);});
        life.listen(el("plan-clear"),"click",()=>{el("plan-search").value="";change({q:"",state:""});});
        life.listen(el("plan-refresh"),"click",()=>{if(timer!==null)change({});else{load("list");if(store.view().selected)load("detail");}});
        for(const[id,delta]of [["plan-prev",-1],["plan-next",1]])life.listen(el(id),"click",()=>change({page:store.view().list.data.page+delta}));
        life.listen(el("plan-rows"),"click",event=>{
            const host=event.target.closest("button[data-plan-server]");if(host){change({server:host.dataset.planServer,q:"",state:""});return;}
            const button=event.target.closest("button[data-plan-package]");if(!button)return;
            previousPackage=button.dataset.planPackage;store.select(previousPackage);renderDetail();dialog.showModal();el("plan-detail-title").focus();load("detail");
        });
        life.listen(el("plan-detail-close"),"click",()=>dialog.close());
        life.listen(dialog,"close",()=>{controllers.detail?.abort();store.close();Array.from(document.querySelectorAll("button[data-plan-package]")).find(button=>button.dataset.planPackage===previousPackage)?.focus({preventScroll:true});});
        for(const[id,delta]of [["plan-detail-prev",-1],["plan-detail-next",1]])life.listen(el(id),"click",()=>{store.detailPage(store.view().detail.data.page+delta);load("detail");});
        life.listen(root,"popstate",()=>{store.change(root.CVEPageInteraction.parseQuery(location.search,defaults));sync();if(dialog.open)dialog.close();load("list");});
        current={destroy(){Object.values(controllers).forEach(controller=>controller.abort());if(dialog.open)dialog.close();life.destroy();}};
        sync();load("list");return current;
    }
    root.RemediationPage={mount,destroy(){current?.destroy();current=null;}};
    mount();root.addEventListener("pagehide",event=>{if(!event.persisted)root.RemediationPage.destroy();});
})(window);
