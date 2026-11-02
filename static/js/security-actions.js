(function(root){
    "use strict";
    let current;
    const views={fixes:{title:"Correctifs disponibles",help:"Mises à jour avec correctif confirmé et installation simulée.",empty:"Aucun correctif installable confirmé. Des preuves APT anciennes ou manquantes ne signifient pas qu’aucun correctif n’a été publié."},alerts:{title:"Alertes prioritaires sans correctif",help:"Critiques récentes ou exploitation connue, sans correctif annoncé pour les paquets observés.",empty:"Aucune alerte sans correctif confirmée. Les données inconnues ou anciennes restent à vérifier."},lifecycle:{title:"Fin de support des OS et applications",help:"Calendriers de support pour les versions observées sur vos serveurs.",empty:"Aucun composant reconnu dans ce filtre. Les logiciels non inventoriés restent hors couverture."}};
    const labels={end_of_life:"Fin de support",upstream_end_of_life:"Fin de support amont",extended_support_unverified:"Extension de support à vérifier",ending_soon:"Fin de support sous 90 jours",supported:"Support en cours",unknown:"Support à vérifier"};
    const freshness={fresh:"récent",stale:"à actualiser",unavailable:"indisponible",unknown:"non vérifié"};
    const severity={critical:"Critique",high:"Élevée",medium:"Moyenne",low:"Faible",none:"Aucune",unknown:"Non renseignée"};
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
        const path=(base,values)=>base+"?"+new URLSearchParams(values).toString();
        const plan=server=>path("/remediation",{server});
        const ledger=(server,q="")=>path("/vulnerabilities/all",{server,q});
        const urlLink=(url,label)=>{try{const u=new URL(url);if(!["https:","http:"].includes(u.protocol))return "";return `<a href="${esc(u.href)}" target="_blank" rel="noopener noreferrer">${esc(label)}</a>`;}catch{return "";}};
        let controller,timer=null,coverage=null,coverageError="",expanded=new Set();
        function sync(){const q=store.view().query;if(timer===null)el("security-search").value=q.q;const select=el("security-server");if(q.server&&!Array.from(select.options).some(o=>o.value===q.server))select.add(new Option(q.server,q.server));select.value=q.server;el("security-recent").value=q.recent_days;el("security-ledger-link").href=ledger(q.server,q.q);el("cve-plan-link").href=plan(q.server);
            for(const tab of tabs){const active=tab.dataset.view===q.view;tab.setAttribute("aria-selected",String(active));tab.tabIndex=active?0:-1;}el("security-panel").setAttribute("aria-labelledby",`security-tab-${q.view}`);el("security-options").hidden=q.view!=="alerts";el("security-list-title").textContent=views[q.view].title;el("security-list-help").textContent=views[q.view].help;}
        function health(data){
            const issues=(coverage?.coverage||[]).filter(h=>h.reason||h.state!=="complete"||h.freshness!=="fresh"||h.unknown||h.unsupported||h.error_count);
            const priority=data?.review_priority||0;
            const summary=coverageError?"Couverture des scans indisponible":coverage?issues.length?`${issues.length} serveur${issues.length>1?"s":""} à vérifier`:coverage.coverage.length?"Couverture des scans vérifiée":"Aucun scan dans ce périmètre":"Couverture des scans à vérifier";
            el("security-review").textContent=summary+(priority?` · ${priority} priorité${priority>1?"s":""} à réévaluer`:"");
            el("security-health").classList.toggle("needs-attention",!!issues.length||!!priority||!!coverageError||!coverage);
            el("security-health-status").textContent=coverageError?"La couverture n’a pas pu être relue. Réessayez avec Actualiser ; aucune absence de risque n’est établie.":issues.length?"Consultez les raisons par serveur avant de conclure à l’absence de correctifs.":coverage?.coverage.length?"Les scans enregistrés sont complets et récents. Les correctifs et les alertes restent évalués séparément.":"La couverture sera connue après la lecture des résultats.";
            el("security-health-hosts").innerHTML=issues.map(h=>{const reason=h.reason==="source_coverage_unavailable"?"Données CVE indisponibles pour cette distribution":h.reason==="unsupported_distribution"?"Distribution non couverte":h.state==="unavailable"?"Dernier scan indisponible":h.state==="not_scanned"?"Aucun scan enregistré":h.state!=="complete"?"Scan incomplet":h.freshness!=="fresh"?"Résultats anciens":(h.unknown||h.unsupported)?"Paquets non évalués":"Erreurs à examiner";return `<li><span><strong>${esc(h.server)}</strong>${note(reason)}</span><a href="${esc(ledger(h.server))}#cve-coverage">Voir la couverture</a></li>`;}).join("");
        }
        function detail(f,view){
            const field=(label,value)=>`<div><dt>${esc(label)}</dt><dd>${esc(value||"Non renseigné")}</dd></div>`;
            if(view==="fixes")return `<dl>${field("Version installée",f.installed)}${field("Version proposée",f.candidate)}${field("CVE concernées",f.cves.join(", ")+(f.cve_count>f.cves.length?` (extrait de ${f.cve_count} CVE)`:""))}${field("Scopes simulés",f.scopes.join(", "))}${field("Preuve observée",f.observed_at)}</dl><p>La préparation revérifie la transaction. Une approbation explicite et un nouveau scan restent nécessaires pour confirmer la correction.</p>`;
            if(view==="alerts")return `<dl>${field("Paquet installé",`${f.package} ${f.installed}`)}${field("Publication de l’avis",f.published_at)}${field("Correspondance observée",f.observed_at)}${field("Exploitation connue",f.known_exploited?`CISA KEV — ${freshness[f.exploitation_freshness]||"non vérifié"}`:"Non établie")}</dl><p>${esc(f.required_action||"Consultez l’avis éditeur pour une mesure temporaire et documentez le suivi du serveur.")}</p><p>Aucun correctif annoncé par la source pour cette distribution. Une mesure temporaire n’est pas une preuve de correction et n’est pas exécutée automatiquement.</p><div class="security-detail-links">${urlLink(f.source,"Lire l’avis")}<a href="${esc(ledger(f.server,f.cves[0]))}">Voir les preuves CVE</a><a href="${esc(plan(f.server))}">Documenter le suivi</a></div>`;
            return `<dl>${field("Version observée",f.version)}${field("Cycle",f.cycle)}${field("Fin de support",f.eol_at)}${field("Extension possible jusqu’au",f.extended_until)}${field("Métadonnées vérifiées",f.observed_at)}${field("Fraîcheur des métadonnées",freshness[f.freshness]||"non vérifié")}${field("Fraîcheur de l’inventaire",freshness[f.inventory_freshness]||"non vérifié")}${field("Paquet observé",f.inventory_observed_at)}</dl>${f.inventory_source==="recorded_findings"?"<p>Constats CVE enregistrés : inventaire partiel et historique. Il ne confirme pas la version actuellement installée ni la présence de toutes les applications.</p>":""}${f.inventory_version_state==="conflicting"?"<p>Versions contradictoires à la même date : version et cycle non établis.</p>":""}<p>${f.kind==="application"?"Ce calendrier décrit le support amont. Les correctifs Debian/Ubuntu doivent être vérifiés séparément.":"Un support étendu doit être vérifié pour ce serveur ; sa disponibilité ne prouve pas son activation."}</p>${urlLink(f.source,"Consulter le calendrier endoflife.date")}`;
        }
        function rows(data,view){
            return data[view].map((f,i)=>{
                const id=`security-detail-${i}`,open=expanded.has(i),isLife=view==="lifecycle";
                const component=isLife?f.component:f.package;
                let situation,annotation,action;
                if(view==="fixes"){situation=`${f.cve_count||f.cves.length} CVE avec correctif vérifié`;annotation=severity[f.severity]||severity.unknown;action=`<a class="security-row-primary" href="${esc(plan(f.server))}">Préparer la correction</a>`;}
                else if(view==="alerts"){situation=f.cves[0];annotation=f.known_exploited?"Exploitation connue · sans correctif annoncé":"Critique · sans correctif annoncé";action="";}
                else{situation=labels[f.state]||labels.unknown;annotation=f.inventory_partial?"Inventaire partiel":f.inventory_freshness!=="fresh"?"Version à revérifier":f.eol_at?`Échéance : ${f.eol_at}`:"";action="";}
                const danger=view==="alerts"||(isLife&&["end_of_life","upstream_end_of_life"].includes(f.state));
                return `<tr class="security-item"><td data-label="Serveur / composant"><strong>${esc(f.server)}</strong>${note((isLife?(f.kind==="os"?"OS · ":"Application · "):"")+component+(isLife?` · ${f.version||"version non établie"}`:""))}</td><td data-label="Situation"><strong class="${danger?"security-priority":""}">${esc(situation)}</strong>${note(annotation)}</td><td data-label="Prochaine action"><div class="security-row-actions">${action}<button type="button" class="btn-ghost security-detail-toggle" data-detail="${i}" aria-expanded="${open}" aria-controls="${id}">${isLife?"Vérifier le support":view==="alerts"?"Détails et mesures":"Détails"}</button></div></td></tr><tr id="${id}" class="security-evidence" ${open?"":"hidden"}><td colspan="3"><div class="security-evidence-content">${detail(f,view)}</div></td></tr>`;
            }).join("");
        }
        function render(){
            const {list,query}=store.view(),data=list.data;
            if(data){const select=el("security-server");select.replaceChildren(new Option("Tous les serveurs",""),...data.servers.map(s=>new Option(s,s)));}
            sync();health(data);
            el("security-panel").setAttribute("aria-busy",String(list.loading));
            el("security-error").hidden=!list.error;el("security-error").textContent=list.error?`${list.error}${data?" Les derniers résultats restent affichés.":""} Réessayez avec Actualiser.`:"";
            for(const [id,key] of [["security-fix-count","fix_total"],["security-alert-count","alert_total"],["security-lifecycle-count","lifecycle_total"]])el(id).textContent=data?String(data[key]):"—";
            el("security-prev").disabled=list.loading||!data||data.page<=1;el("security-next").disabled=list.loading||!data||data.page>=data.pages;
            el("security-page").textContent=data?`Page ${data.page} / ${data.pages}`:"Page —";
            el("security-status").textContent=list.loading?"Lecture des résultats…":data?`Dernière lecture : ${new Date(data.generated_at).toLocaleString("fr-CA")}`:"Résultats indisponibles";
            el("security-rows").innerHTML=data?(data[query.view].length?rows(data,query.view):`<tr><td colspan="3" class="security-empty"><strong>${query.view==="fixes"?"Aucun correctif prêt à préparer":query.view==="alerts"?"Aucune alerte sans correctif confirmée":"Aucun composant dans ce filtre"}</strong><p>${esc(views[query.view].empty)}</p><button type="button" class="btn-ghost" data-checks>Vérifier les données</button></td></tr>`):`<tr><td colspan="3" class="security-empty">${list.loading?"Chargement…":"Les résultats sont indisponibles. Réessayez avec Actualiser."}</td></tr>`;
            el("security-feeds").innerHTML=data?Object.entries(data.feeds).map(([name,f])=>`<li>${esc(name)} : ${esc(freshness[f.state]||"non vérifié")} — ${esc(f.observed_at||"jamais vérifié")}${f.last_error?note("Échec de la dernière actualisation ; observation précédente conservée."):""}</li>`).join(""):"";
        }
        async function json(url,signal){const response=await life.fetch(url,{signal});if(!response.ok)throw new Error(`Résultats indisponibles (HTTP ${response.status})`);return response.json();}
        async function load(){
            controller?.abort();controller=new AbortController();const signal=controller.signal,id=store.begin("list"),ticket=life.capture(),q=store.view().query;render();
            const results=await Promise.allSettled([json(path("/api/security-actions",{...q,page_size:25}),signal),json(path("/api/vulnerabilities",{server:q.server,page_size:1}),signal)]);
            if(!life.isCurrent(ticket)||signal.aborted)return;
            if(results[0].status==="fulfilled"){
                if(!store.receive("list",id,results[0].value))return;
                expanded.clear();
            }else{if(!store.receive("list",id,null,results[0].reason.message))return;}
            if(results[1].status==="fulfilled"){coverage=results[1].value;coverageError="";}else coverageError=results[1].reason.message;
            render();
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
