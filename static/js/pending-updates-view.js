(function (root, factory) {
    const model = typeof module === 'object' && module.exports ? require('./pending-updates-interaction.js') : root.PendingUpdatesInteraction;
    const api = factory(model);
    if (typeof module === 'object' && module.exports) module.exports = api;
    else root.PendingUpdatesView = api;
}(typeof globalThis !== 'undefined' ? globalThis : this, function (model) {
    'use strict';
    const escape = value => String(value ?? '').replace(/[&<>"']/g, char => ({'&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;'}[char]));
    const button = (action, value, label, extra = '') => `<button type="button" class="pending-control" data-pending-action="${action}" data-pending-value="${escape(value)}" ${extra}>${label}</button>`;
    function pagination(page, type, noun) {
        return `<nav class="pending-pagination" aria-label="${noun} pages"><span role="status" aria-live="polite">${page.start}–${page.end} of ${page.total} ${noun.toLowerCase()}</span><div>
            ${button(type, 'previous', '← Previous', page.page === 0 ? 'disabled' : '')}
            ${button(type, 'next', 'Next →', page.page + 1 >= page.pages ? 'disabled' : '')}</div></nav>`;
    }
    function search(id, label, value, action) {
        return `<label class="pending-search"><span class="sr-only">${label}</span><input id="${id}" type="search" aria-label="${label}" placeholder="${label}…" value="${escape(value)}" data-pending-input="${action}" maxlength="200"></label>`;
    }
    function badges(update) {
        return `${update.security ? '<span class="pending-kind">Security</span>' : ''}
            ${update.deferred ? '<span class="pending-kind pending-kind-warning">Deferred — security exposure remains</span>' : ''}
            ${update.kept_back || update.requires_full_upgrade ? '<span class="pending-kind pending-kind-warning">Kept back · Requires full-upgrade</span>' : ''}`;
    }
    function lookupNotice(view) {
        return `${view.pending ? `<p class="pending-note" role="status">Checking CVEs for ${view.pending} available package${view.pending === 1 ? '' : 's'}…</p>` : ''}
            ${view.unavailable ? `<p class="pending-lookup-error" role="status">CVE lookup unavailable for ${view.unavailable} package${view.unavailable === 1 ? '' : 's'}. Run a fresh update check to retry.</p>` : ''}
            ${view.skipped ? `<p class="pending-note">CVE information is not available for ${view.skipped} package${view.skipped === 1 ? '' : 's'}.</p>` : ''}`;
    }
    function list(view) {
        const partial = view.unverified || view.unsupported;
        return `<div class="pending-workspace-head"><h4 tabindex="-1" data-pending-heading>Available updates</h4><span>${view.total} package${view.total === 1 ? '' : 's'} · ${view.security} security${view.keptBack ? ` · ${view.keptBack} kept back` : ''}</span></div>
            <div class="pending-toolbar"><div class="pending-filters" role="group" aria-label="Package filters">
                ${button('filter', 'all', `All <span>${view.total}</span>`, `aria-pressed="${view.filter === 'all'}"`)}
                ${button('filter', 'security', `Security <span>${view.security}</span>`, `aria-pressed="${view.filter === 'security'}"`)}</div>
                ${search('pending-package-search', 'Filter packages', view.query, 'search')}</div>
            ${lookupNotice(view)}<div class="pending-package-table-wrap"><table class="pending-package-table"><thead><tr><th>Package</th><th>Available version</th><th><span class="sr-only">Details</span></th></tr></thead><tbody>
            ${view.packages.items.map(update => `<tr><td><span class="pending-package-name">${escape(update.package || 'Unknown package')}</span><div class="pending-kinds">${badges(update)}</div></td><td>${escape(update.candidate_version || 'Unknown')}</td><td>
                ${button('package', model.packageKey(update), 'Details →', `aria-label="Details for ${escape(update.package || 'unknown package')}"`)}</td></tr>`).join('')}</tbody></table></div>
            ${!view.packages.total ? '<p class="drawer-empty">No matching packages.</p>' : ''}${pagination(view.packages, 'page', 'Packages')}
            <div class="pending-coverage-note"><span>${partial ? 'CVE coverage is partial.' : 'CVE information supplements APT update classification.'}</span>${button('coverage', '', 'View coverage')}</div>`;
    }
    function qualification(finding) {
        const assessment = finding.assessment || {};
        if (['unimportant', 'not_affected', 'withdrawn'].includes(assessment.vendor_status)) return `Distribution: ${assessment.vendor_status}`;
        return assessment.confidence === 'vendor_confirmed' ? 'Distribution-confirmed source match' : 'Distribution assessment needs review';
    }
    function disposition(finding) {
        return finding.disposition === 'fixed_by_candidate' ? 'Not reported for the proposed version'
            : finding.disposition === 'still_affected' ? 'Still reported for the proposed version' : 'Correction could not be verified';
    }
    function evidence(finding) {
        const assessment = finding.assessment || {};
        const url = String(finding.advisory_url || '').trim();
        return `<section class="pending-cve-evidence" id="pending-cve-evidence" aria-label="CVE source evidence">
            <div class="pending-workspace-head"><h5>${escape(finding.id || 'Unknown CVE')}</h5>${button('evidence', finding.id, 'Close source')}</div>
            <p>${escape(qualification(finding))}</p>
            <p>${escape(assessment.vendor_reason || 'Distribution assessment unavailable')}</p>
            <dl><div><dt>Assessed versions</dt><dd>${escape(finding.installed_version || "Unknown")} → ${escape(finding.candidate_version || "Unknown")}</dd></div><div><dt>Source</dt><dd>${escape(assessment.ecosystem || 'Unknown distribution')} / ${escape(assessment.source_package || finding.source_package || 'Unknown package')} · ${escape(finding.source || 'Distribution data')}</dd></div>
            <div><dt>Observed</dt><dd>${escape(assessment.observed_at || 'Unknown')}</dd></div></dl>
            ${/^https?:\/\//i.test(url) ? `<a href="${escape(url)}" target="_blank" rel="noopener noreferrer">Open original advisory ↗</a>` : ''}</section>`;
    }
    function packageView(view) {
        const update = view.selected;
        const findings = view.allFindings;
        const groups = [...new Set(findings.map(disposition))];
        const qualifications = [...new Set(findings.map(qualification))];
        const reviewQualification = 'Distribution assessment needs review';
        const sharedQualification = qualifications.length === 1 ? qualifications[0]
            : qualifications.includes(reviewQualification) ? 'Some distribution assessments need review.' : '';
        const unknown = groups.length === 1 && groups[0] === 'Correction could not be verified';
        const state = String(update.cve_state || 'unknown');
        const context = state === 'pending' ? 'CVE lookup in progress.' : state === 'unavailable' ? 'CVE lookup unavailable. Run a fresh update check to retry.'
            : state === 'unsupported' ? 'Distribution coverage is unavailable for this package source.'
            : state === 'skipped' ? 'CVE lookup was skipped for this package.'
            : state !== 'ready' ? 'CVE lookup status is unknown.'
            : findings.length ? 'These IDs are listed by the distribution source. Their presence alone does not confirm that this update fixes them.'
            : (update.cves || []).length ? 'CVE details need a fresh update check.'
            : 'No source matches reported. This does not establish that the package is vulnerability-free.';
        return `${button('back', view.selectedKey, '← All updates')}
            <div class="pending-package-heading"><h4 tabindex="-1" data-pending-heading>${escape(update.package || 'Unknown package')}</h4><div class="pending-kinds">${badges(update)}</div></div>
            <dl class="pending-version-pair"><div><dt>Installed version</dt><dd>${escape(update.current_version || 'Unknown')}</dd></div><div><dt>Available version</dt><dd>${escape(update.candidate_version || 'Unknown')}</dd></div></dl>
            <div class="pending-workspace-head"><h4>${findings.length ? `CVE references <span>${findings.length}</span>` : 'CVE information'}</h4>${findings.length ? `<span>${unknown ? 'Update effect unconfirmed' : 'Source comparison'}</span>` : ''}</div>
            <p class="${state === 'unavailable' ? 'pending-lookup-error' : 'pending-note'}">${context}</p>
            ${sharedQualification ? `<p class="pending-qualification">${escape(sharedQualification)}</p>` : ''}
            ${findings.length ? `${search('pending-cve-search', 'Find a CVE', view.cveQuery, 'cve-search')}<div class="pending-cve-list">
                ${view.findings.items.map(finding => `<div class="pending-cve-item ${finding.disposition === 'still_affected' ? 'pending-cve-reported' : ''}">
                    ${button('evidence', finding.id, `<span>${escape(finding.id || 'Unknown CVE')}</span><span>View source ↗</span>`, `aria-expanded="${view.evidenceID === String(finding.id || '')}" ${view.evidenceID === String(finding.id || '') ? 'aria-controls="pending-cve-evidence"' : ''}`)}
                    ${!unknown ? `<small>${escape(disposition(finding))}</small>` : ''}
                    ${qualifications.length !== 1 && qualification(finding) !== reviewQualification ? `<small>${escape(qualification(finding))}</small>` : ''}</div>`).join('')}</div>
                ${!view.findings.total ? '<p class="drawer-empty">No matching CVE references.</p>' : ''}${pagination(view.findings, 'cve-page', 'References')}` : ''}
            ${view.evidence ? evidence(view.evidence) : ''}
            <div class="pending-source-context">${escape(update.source || 'Package source unavailable')}${update.cve_coverage === 'official_installed_unverified' ? ' · Installed origin unverified' : ''}
            ${update.cve_source ? `<br>${escape(update.cve_source)}` : ''}${update.cve_scanned_at ? ` · Checked ${escape(update.cve_scanned_at)}` : ''}</div>`;
    }
    function coverage(view) {
        return `${button('back', '', '← All updates')}<h4 class="pending-coverage-heading" tabindex="-1" data-pending-heading>CVE coverage</h4>
            <p class="pending-note">CVE information is supplementary to the APT update list.</p>
            <dl class="pending-coverage-counts"><div><dt>Installed origin unverified</dt><dd>${view.unverified} packages</dd></div><div><dt>Distribution coverage unavailable</dt><dd>${view.unsupported} packages</dd></div></dl>
            ${view.unverified ? '<p class="pending-note">OSV results use official distribution data, but APT no longer exposes the origin of the installed version.</p>' : ''}
            ${view.unsupported ? '<p class="pending-note">Installed package provenance or candidate repository is not officially verified; no vulnerability claim is made.</p>' : ''}
            <p class="pending-note">An unavailable assessment does not mean the package is vulnerable or safe. Package-level source information remains available in Details.</p>${lookupNotice(view)}`;
    }
    function render(view, { deferrals = '', driftReason = '' } = {}) {
        if (!view.total) return '<p class="drawer-empty">No pending update details for this server.</p>';
        return `<div class="pending-workspace">${deferrals}${driftReason ? `<p class="pending-note pending-drift-note">${escape(driftReason)}. Approval actions stay disabled until the host is pending approval again.</p>` : ''}
            ${view.mode === 'package' ? packageView(view) : view.mode === 'coverage' ? coverage(view) : list(view)}</div>`;
    }
    return { render };
}));
