(function (root, factory) {
    const api = factory();
    if (typeof module === 'object' && module.exports) module.exports = api;
    else root.PendingUpdatesInteraction = api;
}(typeof globalThis !== 'undefined' ? globalThis : this, function () {
    'use strict';
    const packagePageSize = 15, cvePageSize = 5;
    const packageKey = update => JSON.stringify([update.install_package || update.package || '', update.current_version || '', update.candidate_version || '']);
    const text = value => String(value || '');
    const paginate = (items, requested, size) => {
        const page = Math.min(Math.max(0, Number.isFinite(requested) ? Math.floor(requested) : 0), Math.max(0, Math.ceil(items.length / size) - 1));
        return { page, total: items.length, pages: Math.ceil(items.length / size), start: items.length ? page * size + 1 : 0,
            end: Math.min((page + 1) * size, items.length), items: items.slice(page * size, (page + 1) * size) };
    };
    function createStore() {
        let host = '', state;
        function reset() { state = { mode: 'list', filter: 'all', query: '', page: 0, selectedKey: '', cveQuery: '', cvePage: 0, evidenceID: '' }; }
        reset();
        function view(server) {
            if (host !== text(server?.name)) { host = text(server?.name); reset(); }
            const updates = Array.isArray(server?.pending_updates) ? server.pending_updates : [];
            const selected = updates.find(update => packageKey(update) === state.selectedKey) || null;
            if (!selected && state.mode === 'package') { state.mode = 'list'; state.selectedKey = ''; state.evidenceID = ''; }
            const packages = paginate(updates.filter(update => (state.filter !== 'security' || update.security) && text(update.package).toLowerCase().includes(state.query.toLowerCase())), state.page, packagePageSize);
            state.page = packages.page;
            const allFindings = Array.isArray(selected?.cve_findings) ? selected.cve_findings : [];
            const findings = paginate(allFindings.filter(finding => text(finding.id).toLowerCase().includes(state.cveQuery.toLowerCase())), state.cvePage, cvePageSize);
            state.cvePage = findings.page;
            const evidence = findings.items.find(finding => text(finding.id) === state.evidenceID) || null;
            if (!evidence) state.evidenceID = '';
            return { ...state, selected, packages, findings, evidence, allFindings,
                total: updates.length, security: updates.filter(update => update.security).length,
                keptBack: updates.filter(update => update.kept_back || update.requires_full_upgrade).length,
                pending: updates.filter(update => update.cve_state === 'pending').length,
                unavailable: updates.filter(update => update.cve_state === 'unavailable').length,
                skipped: updates.filter(update => !['ready', 'pending', 'unavailable', 'unsupported'].includes(update.cve_state)).length,
                unverified: updates.filter(update => update.cve_state === 'ready' && update.cve_coverage === 'official_installed_unverified').length,
                unsupported: updates.filter(update => update.cve_state === 'unsupported').length };
        }
        function dispatch(intent, server) {
            const current = view(server);
            const value = text(intent?.value);
            switch (intent?.action) {
            case 'filter': state.filter = value === 'security' ? 'security' : 'all'; state.page = 0; break;
            case 'search': state.query = value.slice(0, 200); state.page = 0; break;
            case 'page': state.page += value === 'next' ? 1 : -1; break;
            case 'package':
                if (!(server?.pending_updates || []).some(update => packageKey(update) === value)) break;
                state.mode = 'package'; state.selectedKey = value; state.cveQuery = ''; state.cvePage = 0; state.evidenceID = ''; break;
            case 'back': state.mode = 'list'; state.evidenceID = ''; break;
            case 'coverage': state.mode = 'coverage'; break;
            case 'cve-search': state.cveQuery = value.slice(0, 200); state.cvePage = 0; state.evidenceID = ''; break;
            case 'cve-page': state.cvePage += value === 'next' ? 1 : -1; state.evidenceID = ''; break;
            case 'evidence':
                if (current.findings.items.some(finding => text(finding.id) === value)) state.evidenceID = state.evidenceID === value ? '' : value;
                break;
            }
        }
        return { view, dispatch };
    }
    return { createStore, packageKey };
}));
