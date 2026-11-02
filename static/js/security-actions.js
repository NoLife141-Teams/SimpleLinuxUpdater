(function(root){
    "use strict";
    let current;
    function mount(){
        current?.destroy();
        const life=root.BrowserEffects.create();life.init();
        const defaults={server:"",q:"",recent_days:30,page:1};
        const store=root.CVEPageInteraction.createStore(location.search,defaults);
        const el=id=>document.getElementById(id);
        const esc=v=>String(v??"").replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
        const note=v=>`<span class="security-note">${esc(v)}</span>`;
        const empty=text=>`<tr><td colspan="4" class="security-empty">${esc(text)}</td></tr>`;
        const plan=server=>`/remediation?${new URLSearchParams({server})}`;
        const detail=(server,id)=>`/vulnerabilities?${new URLSearchParams({server,q:id})}#all-cves`;
        const freshness={fresh:"récent",stale:"à actualiser",unavailable:"indisponible",unknown:"non vérifié"};
        const labels={end_of_life:"Fin de support",upstream_end_of_life:"Fin de support amont",extended_support_unverified:"Support standard terminé ; extension à vérifier",ending_soon:"Fin de support dans moins de 90 jours",supported:"Calendrier de support en cours",unknown:"Support à vérifier"};
        let controller,timer;
        function render(){
            const {list,query}=store.view(),data=list.data;
            el("security-search").value=query.q;el("security-server").value=query.server;el("security-recent").value=query.recent_days;
            el("security-error").hidden=!list.error;
            el("security-error").textContent=list.error?`${list.error}${data?" Les derniers résultats restent affichés.":""} Utilisez Actualiser les résultats pour réessayer.`:"";
            el("security-prev").disabled=list.loading||!data||data.page<=1;
            el("security-next").disabled=list.loading||!data||data.page>=data.pages;
            el("security-filters").setAttribute("aria-busy",String(list.loading));
            if(!data){
                for(const id of ["security-fix-count","security-alert-count","security-lifecycle-count"])el(id).textContent="—";
                el("security-review").textContent="Les résultats de ce filtre doivent être vérifiés avant de conclure à l’absence de correctifs ou d’alertes.";
                el("security-page").textContent="Page —";
                el("security-status").textContent=list.loading?"Chargement des actions…":"Résultats indisponibles";
                for(const id of ["security-fixes","security-alerts","security-lifecycle"])el(id).innerHTML=empty(list.loading?"Chargement…":"Résultats indisponibles. Réessayez l’actualisation.");
                return;
            }
            const names=query.server&&!data.servers.includes(query.server)?[query.server,...data.servers]:data.servers;
            el("security-server").innerHTML='<option value="">Tous les serveurs</option>'+names.map(n=>`<option value="${esc(n)}">${esc(n)}</option>`).join("");
            el("security-server").value=query.server;el("security-search").value=query.q;el("security-recent").value=query.recent_days;
            el("security-status").textContent=`${list.loading?"Actualisation… ":""}Vue calculée le ${data.generated_at}`;
            el("security-review").textContent=`${data.review_findings} occurrences demandent une vérification avant de conclure à un correctif disponible ou absent, dont ${data.review_priority} prioritaires. Consultez le détail et la couverture des scans ci-dessous. Une liste vide ne prouve pas l’absence de risque.`;
            el("security-page").textContent=`Page ${data.page} / ${data.pages}`;
            el("security-fix-count").textContent=`${data.fix_total} paquets / serveurs`;
            el("security-alert-count").textContent=`${data.alert_total} occurrences prioritaires`;
            el("security-lifecycle-count").textContent=`${data.lifecycle_total} composants reconnus`;
            const notOnPage="Aucun résultat sur cette page. Revenez aux pages précédentes.";
            el("security-fixes").innerHTML=data.fixes.length?data.fixes.map(f=>`<tr><td><strong>${esc(f.server)}</strong>${note(f.package)}</td><td>${esc(f.installed)} → ${esc(f.candidate)}${note(`Simulation : ${f.scopes.join(", ")}`)}</td><td>${esc(f.severity)}${note(`${f.cve_count||f.cves.length} CVE : ${f.cves.join(", ")}`)}${f.known_exploited?note("Exploitation connue — priorité"):""}</td><td><a href="${esc(plan(f.server))}">Préparer la correction</a>${note("Simulation à revérifier puis approbation explicite")}</td></tr>`).join(""):empty(data.fix_total?notOnPage:"Aucun correctif installable confirmé. Vérifiez la fraîcheur des scans et les informations APT ; cela ne signifie pas qu’aucun correctif n’a été publié.");
            el("security-alerts").innerHTML=data.alerts.length?data.alerts.map(f=>`<tr><td><a href="${esc(detail(f.server,f.cves[0]))}">${esc(f.cves[0])}</a>${note(f.known_exploited?`Exploitation connue — CISA KEV (${freshness[f.exploitation_freshness]||"non vérifié"})`:"Critique")}</td><td><strong>${esc(f.server)}</strong>${note(`${f.package} ${f.installed}`)}</td><td>${esc(f.published_at||"Publication non renseignée")}${note(`Correspondance vérifiée : ${f.observed_at}`)}${note("Aucun correctif annoncé dans la source pour cette distribution")}</td><td>${esc(f.required_action||"Consulter l’avis éditeur pour les mesures temporaires ; documenter le suivi dans le plan du serveur.")} ${note("Les mesures ne sont pas exécutées automatiquement.")}<a href="${esc(plan(f.server))}">Examiner le suivi</a></td></tr>`).join(""):empty(data.alert_total?notOnPage:"Aucune alerte sans correctif confirmée dans ce périmètre. Les données inconnues, anciennes ou non qualifiées restent à vérifier ; la veille ne détecte pas tous les zero-days.");
            el("security-lifecycle").innerHTML=data.lifecycle.length?data.lifecycle.map(f=>`<tr><td><strong>${esc(f.server)}</strong>${note(f.kind==="os"?`OS · ${f.component}`:`Application · ${f.component}`)}</td><td>${esc(f.version||"Version inconnue")}${note(`Cycle ${f.cycle||"non identifié"}`)}</td><td><span class="${["end_of_life","upstream_end_of_life"].includes(f.state)?"security-lifecycle-eol":""}">${esc(labels[f.state]||labels.unknown)}</span>${f.eol_at?note(`Fin : ${f.eol_at}`):""}${f.extended_until?note(`Extension possible jusqu’au ${f.extended_until} ; activation non vérifiée`):""}${f.kind==="application"?note("Calendrier amont ; les correctifs Debian/Ubuntu doivent être vérifiés séparément."):""}</td><td>${f.source?`<a href="${esc(f.source)}" target="_blank" rel="noopener noreferrer">endoflife.date</a>`:"Source indisponible"}${note(`Métadonnées : ${freshness[f.freshness]||"non vérifié"} · ${f.observed_at||"aucune observation"}`)}${note(`Inventaire : ${freshness[f.inventory_freshness]||"non vérifié"}`)}</td></tr>`).join(""):empty(data.lifecycle_total?notOnPage:"Aucun composant reconnu dans ce filtre. Les logiciels non inventoriés ne sont pas déclarés pris en charge.");
            el("security-feeds").innerHTML=Object.entries(data.feeds).map(([name,f])=>`<li>${esc(name)} : ${esc(freshness[f.state]||"non vérifié")} · ${esc(f.observed_at||"jamais vérifié")}${f.last_error?note("Échec de la dernière actualisation ; observation précédente conservée."):""}</li>`).join("");
        }
        async function load(){
            controller?.abort();controller=new AbortController();const id=store.begin("list"),ticket=life.capture();render();
            try{const params=new URLSearchParams({...store.view().query,page_size:25});const response=await life.fetch(`/api/security-actions?${params}`,{signal:controller.signal});if(!response.ok)throw new Error(`Actions indisponibles (HTTP ${response.status})`);const data=await response.json();if(life.isCurrent(ticket)&&store.receive("list",id,data))render();}
            catch(e){if(e.name!=="AbortError"&&life.isCurrent(ticket)&&store.receive("list",id,null,e.message))render();}
        }
        function change(patch){if(timer){life.clearTimeout(timer);timer=null;}store.change(patch);const qs=root.CVEPageInteraction.queryString(store.view().query,defaults);history.pushState({},"",`${location.pathname}${qs?`?${qs}`:""}${location.hash}`);load();}
        life.listen(el("security-filters"),"submit",e=>e.preventDefault());
        life.listen(el("security-server"),"change",()=>change({server:el("security-server").value}));
        life.listen(el("security-recent"),"change",()=>change({recent_days:el("security-recent").value}));
        life.listen(el("security-search"),"input",()=>{if(timer)life.clearTimeout(timer);timer=life.setTimeout(()=>change({q:el("security-search").value}),250);});
        life.listen(el("cve-refresh"),"click",()=>load());
        life.listen(el("security-refresh"),"click",()=>{if(timer){life.clearTimeout(timer);timer=null;}load();});
        life.listen(el("security-prev"),"click",()=>change({page:store.view().query.page-1}));
        life.listen(el("security-next"),"click",()=>change({page:store.view().query.page+1}));
        life.listen(root,"popstate",()=>{controller?.abort();store.change(root.CVEPageInteraction.parseQuery(location.search,defaults));load();});
        life.listen(root,"hashchange",()=>{if(location.hash==="#all-cves")el("all-cves").open=true;});
        if(location.hash==="#all-cves"||new URLSearchParams(location.search).has("fix")||new URLSearchParams(location.search).has("severity"))el("all-cves").open=true;
        current={destroy(){controller?.abort();life.destroy();}};load();
    }
    root.addEventListener("pageshow",e=>{if(e.persisted)mount();});
    root.addEventListener("pagehide",e=>{if(!e.persisted)current?.destroy();});
    mount();
})(window);
