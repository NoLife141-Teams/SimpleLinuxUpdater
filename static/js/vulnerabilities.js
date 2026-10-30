(function (root) {
    "use strict";
    let current;
    function mount() {
        current?.destroy();
        const life = root.BrowserEffects.create();
        life.init();
        const store = root.CVEPageInteraction.createStore(location.search);
        const byId = id => document.getElementById(id);
        const dialog = byId("cve-detail");
        const esc = value => String(value ?? "").replace(/[&<>"']/g, char => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[char]));
        const controllers = {};
        let searchTimer = null, previousFocus = null;
        const labels = { fixed_by_candidate: "Fix available in candidate", still_affected: "Candidate still affected", no_fix_available: "No newer candidate available", candidate_unverified: "Candidate could not be verified" };
        const states = { complete: "Complete scan", partial: "Incomplete scan", not_scanned: "Never scanned", unavailable: "Scan unavailable", cancelled: "Scan cancelled", interrupted: "Scan interrupted", running: "Scan running" };
        function badge(severity) { return `<span class="cve-severity cve-severity-${esc(severity)}">${esc(severity)}</span>`; }
        function note(text) { return `<span class="cve-cell-note">${esc(text)}</span>`; }
        function optionList(id, values, selected, allLabel) {
            const select = byId(id);
            const options = selected && !values.includes(selected) ? [selected,...values] : values;
            select.innerHTML = `<option value="">${allLabel}</option>` + options.map(value => `<option value="${esc(value)}">${esc(value)}</option>`).join("");
            select.value = selected;
        }
        function syncFilters() {
            const query = store.view().query;
            for (const input of byId("cve-filters").elements) { if (input.name) input.value = query[input.name]; }
            byId("cve-plan-link").href = "/remediation" + (query.server ? `?server=${encodeURIComponent(query.server)}` : "");
        }
        function renderList() {
            const {list, query} = store.view();
            byId("cve-table-wrap").setAttribute("aria-busy", String(list.loading));
            byId("cve-error").hidden = !list.error;
            byId("cve-error").textContent = list.error ? `${list.error}${list.data ? " Showing the previous successful results; refresh to retry." : " Use Refresh results to retry."}` : "";
            const data = list.data;
            const coverageIssues = data?.coverage.filter(host => host.reason || host.state !== "complete" || host.freshness !== "fresh") || [];
            byId("cve-coverage-notice").hidden = !coverageIssues.length;
            byId("cve-coverage-notice").textContent = coverageIssues.length === 1 && coverageIssues[0].reason === "source_coverage_unavailable"
                ? `${coverageIssues[0].server}: CVE data unavailable for this distribution release. Exposure cannot be assessed from the current feed.`
                : `Scan coverage needs review for ${coverageIssues.length} of ${data?.coverage.length || 0} hosts in this scope. Findings remain visible; check scan coverage below for missing, incomplete or stale evidence.`;
            const metrics = {"cve-count":"cves","cve-host-count":"hosts","cve-priority-count":"critical_high","cve-fix-count":"with_fix","cve-unknown-count":"unknown_severity","cve-finding-count":"findings"};
            for (const [id,key] of Object.entries(metrics)) byId(id).textContent = data ? Number(data.summary[key] || 0).toLocaleString() : "—";
            byId("cve-prev").disabled = list.loading || !data || data.page <= 1;
            byId("cve-next").disabled = list.loading || !data || data.page >= data.pages;
            if (!data) {
                byId("cve-results-status").textContent = list.loading ? "Loading CVE evidence…" : "CVE evidence unavailable";
                byId("cve-rows").innerHTML = `<tr><td colspan="6">${list.loading ? "Loading CVE evidence…" : "No accepted response. Exposure is unknown."}</td></tr>`;
                byId("cve-page").textContent = "Page —";
                byId("cve-coverage-rows").innerHTML = '<tr><td colspan="4">Coverage unavailable until results load.</td></tr>';
                return;
            }
            optionList("cve-server", data.servers, query.server, "All hosts (including disabled)");
            optionList("cve-distro", data.distros, query.distro, "All distributions");
            byId("cve-refreshed").textContent = `Results loaded ${new Date(data.generated_at).toLocaleString()}`;
            byId("cve-results-status").textContent = list.loading ? "Refreshing evidence…" : data.total ? `${data.total.toLocaleString()} matching CVEs · ${(data.page - 1) * data.page_size + 1}–${Math.min(data.page * data.page_size, data.total)} shown` : "No matching CVE findings. Check scan coverage before interpreting this result.";
            byId("cve-page").textContent = `Page ${data.page} of ${data.pages}`;
            byId("cve-rows").innerHTML = data.rows.length ? data.rows.map(row => `<tr>
                <td><button type="button" class="cve-open" data-cve="${esc(row.id)}">${esc(row.id)}</button>${note(`${row.findings} package findings`)}</td>
                <td>${badge(row.severity)}${row.unknown_severity ? note(`${row.unknown_severity} with unknown severity`) : ""}</td>
                <td>${row.hosts}${note(row.host_preview.join(", ") + (row.hosts > row.host_preview.length ? ` +${row.hosts - row.host_preview.length}` : ""))}</td>
                <td>${row.packages}</td>
                <td>${row.fix_available ? `${row.fix_available} / ${row.findings} package findings` : "No verified candidate fix"}${row.unverified ? note(`${row.unverified} unverified candidates`) : ""}${row.still_affected ? note(`${row.still_affected} candidates still affected`) : ""}</td>
                <td>${row.age_days} days${row.overdue ? note(`${row.overdue} overdue`) : ""}${row.stale ? note(`${row.stale} stale / unknown evidence`) : ""}</td>
            </tr>`).join("") : '<tr><td colspan="6">No findings match these filters. This does not establish absence of exposure.</td></tr>';
            byId("cve-coverage-rows").innerHTML = data.coverage.length ? data.coverage.map(host => {
                const reason = host.reason === "source_coverage_unavailable" ? "CVE data unavailable for this distribution release" : host.reason === "unsupported_distribution" ? "Distribution not supported for CVE scanning" : "";
                const notes = [reason, `${host.unknown} unknown; ${host.unsupported} unsupported`, ...(host.errors || []), host.error_count > (host.errors || []).length ? `${host.error_count - host.errors.length} additional scan issues` : ""].filter(Boolean);
                return `<tr><td><a href="/vulnerabilities?server=${encodeURIComponent(host.server)}" title="Analyse CVEs for this host">${esc(host.server)}</a>${note(host.distro || "Distribution unknown")}</td><td>${esc(states[host.state] || "Never scanned")}${note(`Freshness: ${host.freshness}`)}</td><td>${host.evaluated} / ${host.inventoried}</td><td>${notes.map(esc).join(" · ")}</td></tr>`;
            }).join("") : '<tr><td colspan="4">No hosts match this server/distribution scope.</td></tr>';
        }
        function safeLink(value) {
            try { const url = new URL(value); return url.protocol === "https:" ? url.href : ""; } catch { return ""; }
        }
        function proofNotes(f) {
 const a=f.availability||{},published=a.published||{},r=f.runtime_view||{},triage=f.assessment||{};
 return note(`Qualification: ${f.qualification||"needs_review"} · ${triage.vendor_status||"unknown"} · ${triage.vendor_reason||"Review publisher evidence"}`)+note(`Assessment source: ${triage.source||"unknown"} · ${triage.ecosystem||"unknown"} / ${triage.source_package||"unknown"} ${triage.source_version||"unknown"} · observed ${triage.observed_at||"unknown"}`)+note(`Component: ${r.component||"unknown"} · execution ${r.execution||"unknown"} · owner ${r.owner||"unknown"} · exposure ${r.exposure||"unknown"}`)+note(r.reason||"Runtime applicability needs review")+note(`Published fix: ${published.state||"unknown"} · ${published.fixed_versions?.join(", ")||"no established version"} · source ${published.source||"unknown"}`)+note(`Candidate: ${a.candidate_state||"unverified"} · ${a.reason||"No verified availability"}`)+note(`Source candidate: ${a.candidate_source_package||"unknown"} ${a.candidate_source_version||"unknown"} · binary ${a.candidate_binary_version||"unknown"} · ${a.candidate_origin||"unknown origin"}`)+note(`APT index freshness: ${a.apt?.state||"unknown"} · last known refresh outcome: ${a.apt?.last_refresh_state||"unknown"}`)+(a.scopes||[]).map(s=>note(`Approval Scope ${s.scope}: ${s.state} · ${s.reason} · removals ${s.removed_packages?.join(", ")||"none"}`)).join("")+note(`Package evaluation: ${f.evidence_state||"not_reverified"} · candidate evidence ${f.availability_freshness||"unknown"}`);
 }
 function renderDetail() {
            const {selected, detail} = store.view();
            if (!selected) return;
            byId("cve-detail-title").textContent = selected;
            byId("cve-detail-error").hidden = !detail.error;
            byId("cve-detail-error").textContent = detail.error ? `${detail.error}${detail.data ? " Previous evidence is retained." : " Close and reopen to retry."}` : "";
            const data = detail.data;
            byId("cve-detail-prev").disabled = detail.loading || !data || data.page <= 1;
            byId("cve-detail-next").disabled = detail.loading || !data || data.page >= data.pages;
            if (!data) {
                byId("cve-detail-summary").textContent = detail.loading ? "Loading package evidence…" : "Package evidence unavailable";
                byId("cve-detail-page").textContent = "Page —";
                byId("cve-detail-rows").innerHTML = "";
                return;
            }
            const row = data.rows[0];
            byId("cve-detail-summary").textContent = `${row.hosts} affected hosts · ${row.packages} package names · ${row.findings} findings · ${row.fix_available} with current addressing candidates · ${row.unknown_severity} with unknown severity${detail.loading ? " · Refreshing…" : ""}`;
            byId("cve-detail-page").textContent = `Packages ${(data.page-1)*data.page_size+1}–${Math.min(data.page*data.page_size,data.total)} of ${data.total} · Page ${data.page} of ${data.pages}`;
            byId("cve-detail-rows").innerHTML = (data.exposures || []).map(finding => {
                const link = safeLink(finding.advisory_url);
                return `<tr><td><strong>${esc(finding.server)}</strong>${note(finding.distro)}${note(finding.package)}${note(`Source package: ${finding.source_package || "unknown"}`)}</td>
                    <td>Installed: ${esc(finding.installed_version || "unknown")}${note(`Candidate: ${finding.candidate_version || "none"}`)}${proofNotes(finding)}</td>
                    <td>${badge(finding.severity || "unknown")}${note(labels[finding.disposition] || "Fix status unknown")}${finding.fixed_by_version ? note(`Fixed by: ${finding.fixed_by_version}`) : ""}</td>
                    <td>First seen: ${esc(finding.first_seen || "unknown")}${note(`Last seen: ${finding.last_seen || "unknown"}`)}${note(`Evidence: ${finding.freshness || "unknown"}; severity: ${finding.severity_freshness || "unknown"}`)}${note(`Source: ${finding.source || "unknown"}`)}${finding.severity_value ? note(`${finding.severity_type}: ${finding.severity_value} (${finding.severity_source}; observed ${finding.severity_observed_at || "unknown"})`) : ""}${finding.target_at ? note(`Target: ${finding.target_at}${finding.overdue ? " · overdue" : ""}`) : ""}${link ? `<a href="${esc(link)}" target="_blank" rel="noopener noreferrer">Distribution advisory</a>` : ""}</td></tr>`;
            }).join("");
        }
        async function load(kind) {
            controllers[kind]?.abort();
            const controller = new AbortController(); controllers[kind] = controller;
            const id = store.begin(kind), lifetime = life.capture(), view = store.view();
            const query = new URLSearchParams({ ...view.query, page: kind === "detail" ? view.detailPage : view.query.page, page_size: 50 });
            const path = kind === "detail" ? `/api/vulnerabilities/${encodeURIComponent(view.selected)}` : "/api/vulnerabilities";
            if (kind === "list") renderList(); else renderDetail();
            try {
                const response = await life.fetch(`${path}?${query}`, { signal: controller.signal });
                if (!response.ok) { await response.text(); throw new Error(`CVE evidence unavailable (HTTP ${response.status})`); }
                const data = await response.json();
                if (!life.isCurrent(lifetime)) return;
                if (store.receive(kind,id,data)) { if (kind === "list") renderList(); else renderDetail(); }
            } catch (error) {
                if (!life.isCurrent(lifetime) || error.name === "AbortError") return;
                if (store.receive(kind,id,null,error.message)) { if (kind === "list") renderList(); else renderDetail(); }
            }
        }
        function change(patch) {
            if (searchTimer !== null) { life.clearTimeout(searchTimer); searchTimer = null; }
            const patchWithSearch = { q: byId("cve-search").value, ...patch };
            store.change(patchWithSearch);
            controllers.detail?.abort();
            if (dialog.open) dialog.close();
            const query = root.CVEPageInteraction.queryString(store.view().query);
            history.replaceState(null,"",location.pathname + (query ? `?${query}` : ""));
            syncFilters(); load("list");
        }
        life.listen(byId("cve-filters"),"submit", event => { event.preventDefault(); change({}); });
        life.listen(byId("cve-search"),"input", () => { if (searchTimer !== null) life.clearTimeout(searchTimer); searchTimer = life.setTimeout(() => change({}),200); });
        life.listen(byId("cve-filters"),"change", event => { if (event.target.name && event.target.name !== "q") change({ [event.target.name]: event.target.value }); });
        life.listen(byId("cve-clear"),"click", () => { byId("cve-search").value = ""; store.reset(); syncFilters(); change(root.CVEPageInteraction.parseQuery("")); });
        life.listen(byId("cve-refresh"),"click", () => { if (searchTimer !== null) change({}); else { load("list"); if (store.view().selected) load("detail"); } });
        for (const [id,delta] of [["cve-prev",-1],["cve-next",1]]) life.listen(byId(id),"click", () => change({page:store.view().list.data.page+delta}));
        life.listen(byId("cve-rows"),"click", event => {
            const button = event.target.closest("button[data-cve]"); if (!button) return;
            previousFocus = button.dataset.cve; store.select(previousFocus); renderDetail(); dialog.showModal(); byId("cve-detail-title").focus(); load("detail");
        });
        life.listen(byId("cve-detail-close"),"click", () => dialog.close());
        life.listen(dialog,"close", () => { controllers.detail?.abort(); store.close(); const button = Array.from(document.querySelectorAll("button[data-cve]")).find(item => item.dataset.cve === previousFocus); button?.focus({preventScroll:true}); });
        for (const [id,delta] of [["cve-detail-prev",-1],["cve-detail-next",1]]) life.listen(byId(id),"click", () => { store.detailPage(store.view().detail.data.page+delta); load("detail"); });
        life.listen(root,"popstate", () => { store.change(root.CVEPageInteraction.parseQuery(location.search)); syncFilters(); if (dialog.open) dialog.close(); load("list"); });
        current = { destroy() { Object.values(controllers).forEach(controller=>controller.abort()); if (dialog.open) dialog.close(); life.destroy(); } };
        syncFilters(); load("list");
        return current;
    }
    root.CVEPage = {mount,destroy() {current?.destroy();current=null;}};
    mount();
    root.addEventListener("pagehide",event=>{if(!event.persisted)root.CVEPage.destroy();});
})(window);
