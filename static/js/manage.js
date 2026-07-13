const managePageInteraction = window.ManagePageInteraction.createStore();
window.managePageInteraction = managePageInteraction;
const manageAdapterState = managePageInteraction.adapterState;
let hostKeyModalPromise = null;
let hostKeyModalResolvers = [];
let editKnownHostCheckPromise = null;
let auditFetchHadError = false;

function activeEditorName() {
    const editor = managePageInteraction.getView().editor;
    return editor.open ? editor.originalName : "";
}

function commandExecution(command, payload = {}) {
    return managePageInteraction.dispatch({ type: 'commandRequested', command, payload })
        .find((effect) => effect.type === 'executeCommand');
}

async function executeManageEffects(effects) {
    const refreshStreams = new Set();
    for (const effect of effects || []) {
        if (effect.type === 'announce') window.notifyApp(effect.message || 'Manage action completed.');
        if (effect.type === 'refresh') (effect.streams || []).forEach(stream => refreshStreams.add(stream));
    }
    const refreshes = [];
    if (refreshStreams.has('inventory')) refreshes.push(fetchManageServers());
    if (refreshStreams.has('globalKey')) refreshes.push(fetchGlobalKeyStatus());
    if (refreshStreams.has('audit')) refreshes.push(fetchAuditEvents({ silent: true }));
    await Promise.all(refreshes);
}

async function settleCommand(type, plan, message) {
    const effects = managePageInteraction.dispatch({ type, plan, message });
    await executeManageEffects(effects);
}

const managePolicyOverrides = window.ManagePolicyOverrideAdapter.createAdapter({
    store: managePageInteraction,
    escapeHTML: escapeHtml,
    requestJSON: async (url, options, fallbackMessage) => {
        const response = await fetch(url, options);
        if (!response.ok) throw new Error(await parseErrorResponse(response, fallbackMessage));
        return response.json().catch(() => ({}));
    }
});

	        function escapeHtml(value) {
	            return String(value ?? "")
	                .replace(/&/g, "&amp;")
	                .replace(/</g, "&lt;")
                .replace(/>/g, "&gt;")
                .replace(/"/g, "&quot;")
	                .replace(/'/g, "&#39;");
	        }


	            function normalizePort(value, fallback = 22) {
	                const parsed = Number.parseInt(value, 10);
	                if (!Number.isFinite(parsed) || parsed <= 0 || parsed > 65535) return fallback;
                return parsed;
            }

            function isEditKnownHostTrusted(host, port) {
                const normalizedHost = String(host || '').trim();
                const normalizedPort = normalizePort(port, 22);
                const hostKey = managePageInteraction.getView().editor.hostKey;
                return !!hostKey?.alreadyTrusted &&
                    hostKey.host === normalizedHost &&
                    hostKey.port === normalizedPort;
            }

        async function scanHostKey(host, port) {
            const res = await fetch('/api/hostkeys/scan', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ host, port })
            });
            if (!res.ok) {
                throw new Error(await parseErrorResponse(res, 'Failed to scan host key.'));
            }
            return res.json();
        }

            async function trustHostKey(host, port, fingerprint) {
                const res = await fetch('/api/hostkeys/trust', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ host, port, fingerprint_sha256: fingerprint })
                });
                if (!res.ok) {
                    throw new Error(await parseErrorResponse(res, 'Failed to trust host key.'));
                }
                return res.json();
            }

            async function clearKnownHost(host, port) {
                const res = await fetch('/api/hostkeys/clear', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ host, port })
                });
                if (!res.ok) {
                    throw new Error(await parseErrorResponse(res, 'Failed to clear known host entry.'));
                }
                return res.json();
            }

        function hostKeyPromptText(scanned) {
            return (
                `Host: ${scanned.host}\n` +
                `Port: ${scanned.port}\n` +
                `Algorithm: ${scanned.algorithm}\n` +
                `Fingerprint: ${scanned.fingerprint_sha256}\n\n` +
                `Add this key to known_hosts?`
            );
        }

	        function closeHostKeyModal(confirmed) {
	            const modal = document.getElementById('hostkey-modal');
	            if (modal) {
	                modal.classList.remove('active');
	                releaseModalFocus(modal);
	            }
	            const resolvers = hostKeyModalResolvers;
            hostKeyModalResolvers = [];
            hostKeyModalPromise = null;
            for (const resolver of resolvers) {
                resolver(!!confirmed);
            }
        }

        function confirmHostKeyWithModal(scanned) {
            const modal = document.getElementById('hostkey-modal');
            const details = document.getElementById('hostkey-modal-details');
            if (!modal || !details) {
                return window.confirmAction(
                    `Verify SSH host key before trusting:\n\n${hostKeyPromptText(scanned)}`,
                    { confirmLabel: "Trust key" }
                );
            }
            if (hostKeyModalPromise) {
                return Promise.resolve(false);
	            }
	            details.textContent = hostKeyPromptText(scanned);
	            modal.classList.add('active');
	            activateModalFocus(modal, document.getElementById('hostkey-modal-cancel'));
	            hostKeyModalPromise = new Promise((resolve) => {
	                hostKeyModalResolvers = [resolve];
            });
            return hostKeyModalPromise;
        }

        async function trustHostKeyFlow(host, port, hooks = {}) {
            if (typeof hooks.onScanning === 'function') {
                hooks.onScanning();
            }
            const scanned = await scanHostKey(host, port);
            if (typeof hooks.onScanned === 'function') {
                hooks.onScanned(scanned);
            }
            if (scanned && scanned.already_trusted) {
                if (typeof hooks.onAlreadyTrusted === 'function') {
                    hooks.onAlreadyTrusted(scanned);
                }
                return { alreadyTrusted: true, scanned };
            }
            const confirmed = await confirmHostKeyWithModal(scanned);
            if (!confirmed) {
                throw new Error('Host key trust cancelled.');
            }
            if (typeof hooks.onTrusting === 'function') {
                hooks.onTrusting(scanned);
            }
            const trusted = await trustHostKey(scanned.host, scanned.port, scanned.fingerprint_sha256);
            return { alreadyTrusted: !!trusted?.already_trusted, scanned, trusted };
        }

        function saveWindowScroll() {
            return { x: window.scrollX, y: window.scrollY };
        }

        function restoreWindowScroll(pos) {
            if (!pos) return;
            window.scrollTo(pos.x, pos.y);
        }

        async function fetchManageServers() {
            const pageScroll = saveWindowScroll();
            const request = managePageInteraction.dispatch({ type: 'snapshotRequested', stream: 'inventory' })
                .find((effect) => effect.type === 'fetchSnapshot');
            if (!request) return;
            try {
                const response = await fetch('/api/servers');
                if (!response.ok) {
                    throw new Error(await parseErrorResponse(response, 'Failed to load servers.'));
                }
                const servers = await response.json();
                if (!Array.isArray(servers)) {
                    throw new Error('Invalid server list response.');
                }
                managePageInteraction.dispatch({ type: 'inventorySnapshotReceived', requestID: request.requestID, items: servers });
                const tbody = document.querySelector('#manage-servers-table tbody');
                tbody.innerHTML = '';
                renderTable();
                requestAnimationFrame(() => restoreWindowScroll(pageScroll));
            } catch (error) {
                managePageInteraction.dispatch({ type: 'snapshotFailed', stream: 'inventory', requestID: request.requestID, error: error?.message });
                window.notifyApp(error?.message || 'Failed to load servers.');
            }
        }

        function renderTable() {
            const tbody = document.querySelector('#manage-servers-table tbody');
            tbody.innerHTML = '';
            const projection = managePageInteraction.getView().inventory;
            document.getElementById('page-info').textContent = `Page ${projection.page} of ${projection.totalPages} (${projection.total} hosts)`;
            const groups = projection.groups;
            groups.forEach(group => {
                if (group.key) {
                    const groupRow = document.createElement('tr');
                    groupRow.className = 'group-row';
                    groupRow.innerHTML = `<td colspan="6">${escapeHtml(group.key)}</td>`;
                    tbody.appendChild(groupRow);
                }
                group.items.forEach(server => {
                    const row = document.createElement('tr');
                    row.dataset.name = server.name;
                    const safeName = escapeHtml(server.name);
                    const safeHost = escapeHtml(server.host);
                    const safeUser = escapeHtml(server.user);
                    const safeDataName = escapeHtml(server.name);
                    row.innerHTML = `
                        <td>${safeName}</td>
                        <td>${safeHost}</td>
                        <td>${safeUser}</td>
                        <td>${renderTags(server.tags)}</td>
                        <td>${renderAuth(server)}</td>
                        <td>
                            <button type="button" class="btn-ghost" data-action="edit-server" data-name="${safeDataName}">Edit</button>
                            <button type="button" class="btn-danger" data-action="delete-server" data-name="${safeDataName}">Delete</button>
                        </td>
                    `;
                    tbody.appendChild(row);
                });
            });
        }

        document.querySelector('#manage-servers-table tbody').addEventListener('click', (e) => {
            const button = e.target.closest('button[data-action]');
            if (!button) return;
            const name = button.dataset.name || "";
            if (!name) return;
            const action = button.dataset.action || "";
            if (action === "edit-server") {
                editServer(name);
                return;
            }
            if (action === "delete-server") {
                deleteServer(name);
            }
        });

        const applyManageSortFromHeader = (th) => {
            if (!th) return;
            const key = th.dataset.sortKey;
            managePageInteraction.dispatch({ type: 'sortChanged', key });
            renderTable();
        };

        document.querySelectorAll('#manage-servers-table th.sortable').forEach((th) => {
            const trigger = th.querySelector('.sort-header-btn');
            if (trigger) {
                trigger.addEventListener('click', () => {
                    applyManageSortFromHeader(th);
                });
                return;
            }
            th.addEventListener('click', () => {
                applyManageSortFromHeader(th);
            });
        });

        function syncInventoryFilters() {
            managePageInteraction.dispatch({ type: 'filtersChanged', patch: {
                search: document.getElementById('search').value,
                tag: document.getElementById('tag-filter').value,
                auth: document.getElementById('auth-filter').value,
                group: document.getElementById('group-by').value,
                pageSize: document.getElementById('page-size').value
            } });
            renderTable();
        }
        document.getElementById('search').addEventListener('input', syncInventoryFilters);
        document.getElementById('tag-filter').addEventListener('input', syncInventoryFilters);
        document.getElementById('auth-filter').addEventListener('change', syncInventoryFilters);
        document.getElementById('group-by').addEventListener('change', syncInventoryFilters);
        document.getElementById('page-size').addEventListener('change', syncInventoryFilters);

        document.getElementById('prev-page').addEventListener('click', () => {
            managePageInteraction.dispatch({ type: 'pageChanged', page: Math.max(1, managePageInteraction.getView().inventory.page - 1) });
            renderTable();
        });
        document.getElementById('next-page').addEventListener('click', () => {
            managePageInteraction.dispatch({ type: 'pageChanged', page: managePageInteraction.getView().inventory.page + 1 });
            renderTable();
        });
        document.getElementById('audit-prev-page').addEventListener('click', async () => {
            managePageInteraction.dispatch({ type: 'auditQueryChanged', patch: { page: Math.max(1, managePageInteraction.getView().audit.query.page - 1) } });
            await fetchAuditEvents();
        });
        document.getElementById('audit-next-page').addEventListener('click', async () => {
            managePageInteraction.dispatch({ type: 'auditQueryChanged', patch: { page: managePageInteraction.getView().audit.query.page + 1 } });
            await fetchAuditEvents();
        });
        document.getElementById('audit-target-filter').addEventListener('input', async () => {
            managePageInteraction.dispatch({ type: 'auditQueryChanged', patch: { page: 1 } });
            await fetchAuditEvents();
        });
        document.getElementById('audit-action-filter').addEventListener('input', async () => {
            managePageInteraction.dispatch({ type: 'auditQueryChanged', patch: { page: 1 } });
            document.getElementById('audit-action-preset').value = "";
            await fetchAuditEvents();
        });
        document.getElementById('audit-action-preset').addEventListener('change', async () => {
            document.getElementById('audit-action-filter').value = document.getElementById('audit-action-preset').value;
            managePageInteraction.dispatch({ type: 'auditQueryChanged', patch: { page: 1 } });
            await fetchAuditEvents();
        });
        document.getElementById('audit-status-filter').addEventListener('change', async () => {
            managePageInteraction.dispatch({ type: 'auditQueryChanged', patch: { page: 1 } });
            await fetchAuditEvents();
        });
        document.getElementById('audit-from-filter').addEventListener('change', async () => {
            managePageInteraction.dispatch({ type: 'auditQueryChanged', patch: { page: 1 } });
            await fetchAuditEvents();
        });
        document.getElementById('audit-from-filter').addEventListener('input', async () => {
            managePageInteraction.dispatch({ type: 'auditQueryChanged', patch: { page: 1 } });
            await fetchAuditEvents();
        });
        document.getElementById('audit-to-filter').addEventListener('change', async () => {
            managePageInteraction.dispatch({ type: 'auditQueryChanged', patch: { page: 1 } });
            await fetchAuditEvents();
        });
        document.getElementById('audit-to-filter').addEventListener('input', async () => {
            managePageInteraction.dispatch({ type: 'auditQueryChanged', patch: { page: 1 } });
            await fetchAuditEvents();
        });
        document.getElementById('audit-refresh').addEventListener('click', fetchAuditEvents);
        document.getElementById('audit-prune').addEventListener('click', async () => {
            if (!(await window.confirmTypedAction('Prune audit events older than the configured retention window?', 'PRUNE'))) {
                return;
            }
            const command = managePageInteraction.dispatch({ type: 'commandRequested', command: 'auditPrune' });
            const execution = command.find((effect) => effect.type === 'executeCommand');
            if (!execution) {
                window.notifyApp(command.find((effect) => effect.type === 'commandRejected')?.reason || 'Audit prune is unavailable.');
                return;
            }
            try {
                const res = await fetch('/api/audit-events/prune', { method: 'POST' });
                if (!res.ok) throw new Error(await parseErrorResponse(res, 'Failed to prune audit events.'));
                await settleCommand('commandCompleted', execution.plan, 'Audit events pruned.');
            } catch (err) {
                await settleCommand('commandFailed', execution.plan, err.message || 'Failed to prune audit events.');
            }
        });
        document.querySelector('#audit-table tbody').addEventListener('click', (e) => {
            const button = e.target.closest('button[data-audit-detail]');
            if (!button) return;
            openAuditDetailDrawer(auditEventByID(button.dataset.auditDetail));
        });

        function renderAuth(server) {
            const bits = [];
            if (server.has_password) {
                bits.push('<span class="pill pill-success">Password</span>');
            } else {
                bits.push('<span class="pill pill-muted">No Password</span>');
            }
            if (server.has_key) {
                bits.push('<span class="pill pill-success">Key</span>');
            } else if (managePageInteraction.getView().globalKeyAvailable && !server.has_key) {
                bits.push('<span class="pill pill-success">Global Key</span>');
            } else {
                bits.push('<span class="pill pill-muted">No Key</span>');
            }
            return bits.join(' ');
        }

        function renderTags(tags) {
            if (!tags || tags.length === 0) {
                return '<span class="pill pill-muted">None</span>';
            }
            return tags.map(tag => `<span class="pill">${escapeHtml(tag)}</span>`).join(' ');
        }

        function safeStatusClassToken(status) {
            const normalized = String(status || 'unknown').toLowerCase().replace(/[^a-z0-9_-]/g, '-');
            switch (normalized) {
                case 'success':
                case 'failure':
                case 'started':
                case 'ignored':
                case 'error':
                case 'pending':
                case 'unknown':
                    return normalized;
                default:
                    return 'unknown';
            }
        }

        function auditDateTimeToRFC3339(value) {
            const raw = String(value || '').trim();
            if (!raw) return '';
            const parsed = new Date(raw);
            if (Number.isNaN(parsed.getTime())) return '';
            return parsed.toISOString();
        }

        function prettyAuditMetadata(raw) {
            const text = String(raw || '').trim();
            if (!text) return '{}';
            try {
                return JSON.stringify(JSON.parse(text), null, 2);
            } catch (_) {
                return text;
            }
        }

        function auditEventByID(id) {
            return managePageInteraction.getView().audit.items.find(evt => String(evt.id) === String(id));
        }

        function openAuditDetailDrawer(evt) {
            if (!evt) return;
            managePageInteraction.dispatch({ type: 'auditDetailSelected', id: evt.id });
            const modal = document.getElementById('audit-detail-modal');
            const status = escapeHtml(evt.status || 'unknown');
            const statusClass = `status-${safeStatusClassToken(evt.status)}`;
            const createdAt = window.formatAppTimestamp
                ? window.formatAppTimestamp(evt.created_at, { titleUTC: true, preformattedPrimary: evt.created_at_display })
                : { primary: evt.created_at || '', title: evt.created_at || '' };
            document.getElementById('audit-detail-title').textContent = `Audit #${evt.id}`;
            document.getElementById('audit-detail-actor').textContent = evt.actor || '-';
            document.getElementById('audit-detail-status').innerHTML = `<span class="status-badge ${statusClass}">${status}</span>`;
            document.getElementById('audit-detail-action').textContent = evt.action || '-';
            document.getElementById('audit-detail-target').textContent = `${evt.target_type || '-'}: ${evt.target_name || '-'}`;
            document.getElementById('audit-detail-time').textContent = createdAt.primary || evt.created_at || '-';
            document.getElementById('audit-detail-client-ip').textContent = evt.client_ip || '-';
            document.getElementById('audit-detail-request-id').textContent = evt.request_id || '-';
            document.getElementById('audit-detail-message').textContent = evt.message || '-';
            document.getElementById('audit-detail-meta').textContent = prettyAuditMetadata(evt.meta_json);
            const report = document.getElementById('audit-detail-report');
            report.href = `/api/reports/audit/${encodeURIComponent(evt.id)}`;
            modal.classList.add('active');
            activateModalFocus(modal, document.getElementById('audit-detail-close'));
        }

        function closeAuditDetailDrawer() {
            const modal = document.getElementById('audit-detail-modal');
            modal.classList.remove('active');
            releaseModalFocus(modal);
            managePageInteraction.dispatch({ type: 'auditDetailSelected', id: '' });
        }

        function renderAuditTable() {
            const tbody = document.querySelector('#audit-table tbody');
            if (!tbody) return;
            const projection = managePageInteraction.getView().audit;
            tbody.innerHTML = '';
            if (!projection.items.length) {
                const row = document.createElement('tr');
                row.innerHTML = '<td colspan="8" class="subtle">No activity yet.</td>';
                tbody.appendChild(row);
            } else {
                projection.items.forEach(evt => {
                    const row = document.createElement('tr');
                    const status = escapeHtml(evt.status || 'unknown');
                    const statusClass = `status-${safeStatusClassToken(evt.status)}`;
                    const createdAt = window.formatAppTimestamp
                        ? window.formatAppTimestamp(evt.created_at, { titleUTC: true, preformattedPrimary: evt.created_at_display })
                        : { primary: evt.created_at || '', title: evt.created_at || '' };
                    row.innerHTML = `
                        <td title="${escapeHtml(createdAt.title || '')}">${escapeHtml(createdAt.primary || '')}</td>
                        <td>${escapeHtml(evt.actor || '')}</td>
                        <td>${escapeHtml(evt.action || '')}</td>
                        <td>${escapeHtml(evt.target_type || '')}: ${escapeHtml(evt.target_name || '')}</td>
                        <td><span class="status-badge ${statusClass}">${status}</span></td>
                        <td>${escapeHtml(evt.message || '')}</td>
                        <td><button class="inline-btn btn-ghost" type="button" data-audit-detail="${escapeHtml(String(evt.id))}">Details</button></td>
                        <td><a class="inline-btn btn-ghost" href="/api/reports/audit/${encodeURIComponent(evt.id)}">Report</a></td>
                    `;
                    tbody.appendChild(row);
                });
            }
            const totalPages = Math.max(1, Math.ceil(projection.total / projection.query.pageSize));
            const currentPage = Math.min(projection.query.page, totalPages);
            document.getElementById('audit-page-info').textContent = `Page ${currentPage} of ${totalPages} (${projection.total} events)`;
        }

        async function performAuditRequest(request, silent) {
            const query = request.query || managePageInteraction.getView().audit.query;
            try {
                if (window.ensureAppTimezoneLoaded) {
                    await window.ensureAppTimezoneLoaded();
                }
                const params = new URLSearchParams({
                    page: String(query.page),
                    page_size: String(query.pageSize)
                });
                if (query.targetName) params.set('target_name', query.targetName);
                if (query.action) params.set('action', query.action);
                if (query.status) params.set('status', query.status);
                if (query.from) params.set('from', query.from);
                if (query.to) params.set('to', query.to);
                const res = await fetch(`/api/audit-events?${params.toString()}`);
                if (!res.ok) {
                    throw new Error(await parseErrorResponse(res, 'Failed to load audit events.'));
                }
                const data = await res.json();
                const effects = managePageInteraction.dispatch({ type: 'auditSnapshotReceived', requestID: request.requestID, data });
                auditFetchHadError = false;
                renderAuditTable();
                const followup = effects.find(effect => effect.type === 'fetchSnapshot' && effect.stream === 'audit');
                if (followup) await performAuditRequest(followup, silent);
            } catch (err) {
                managePageInteraction.dispatch({ type: 'snapshotFailed', stream: 'audit', requestID: request.requestID, error: err?.message });
                const message = err && err.message ? err.message : 'Failed to load audit events.';
                const pageInfo = document.getElementById('audit-page-info');
                if (pageInfo) {
                    pageInfo.textContent = `Audit events unavailable: ${message}`;
                }
                if (!silent && !auditFetchHadError) {
                    console.warn('Failed to load audit events:', err);
                }
                auditFetchHadError = true;
            }
        }

        async function fetchAuditEvents(options = {}) {
            const current = managePageInteraction.getView().audit.query;
            const patch = {
                targetName: document.getElementById('audit-target-filter').value.trim(),
                action: document.getElementById('audit-action-filter').value.trim(),
                status: document.getElementById('audit-status-filter').value,
                from: auditDateTimeToRFC3339(document.getElementById('audit-from-filter').value),
                to: auditDateTimeToRFC3339(document.getElementById('audit-to-filter').value),
                page: current.page,
                pageSize: current.pageSize
            };
            managePageInteraction.dispatch({ type: 'auditQueryChanged', patch });
            const query = managePageInteraction.getView().audit.query;
            const request = managePageInteraction.dispatch({ type: 'snapshotRequested', stream: 'audit', payload: { query } })
                .find(effect => effect.type === 'fetchSnapshot');
            if (!request) return;
            await performAuditRequest(request, !!options.silent);
        }

        document.getElementById('add-server-form').addEventListener('submit', async (e) => {
            e.preventDefault();
            const name = document.getElementById('name').value;
            const host = document.getElementById('host').value;
            const portValue = document.getElementById('port').value;
            const port = portValue ? parseInt(portValue, 10) : 0;
            const user = document.getElementById('user').value;
            const pass = document.getElementById('pass').value;
            const tagsRaw = document.getElementById('tags').value;
            const tags = tagsRaw.split(',').map(t => t.trim()).filter(Boolean);
            const keyFileInput = document.getElementById('key_file');
            const trimmedName = name.trim();
            const execution = commandExecution('createServer', {
                name,
                host,
                port,
                user,
                tags,
                hasKeyFile: !!keyFileInput?.files?.length,
                trustHostKey: document.getElementById('trust-host-key').checked
            });
            if (!execution) {
                window.notifyApp('Name, host, and user are required, or a server create is already in progress.');
                return;
            }
            try {
                const accepted = execution.plan.payload;
                const createRes = await fetch('/api/servers', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ name: accepted.name, host: accepted.host, port: accepted.port, user: accepted.user, pass, tags: accepted.tags })
                });
                if (!createRes.ok) {
                    throw new Error(await parseErrorResponse(createRes, 'Failed to add server.'));
                }
                const created = await createRes.json().catch(() => ({
                    name: trimmedName || name,
                    host: host.trim(),
                    port: normalizePort(port, 22)
                }));
                if (accepted.uploadKey && keyFileInput?.files?.length) {
                    const form = new FormData();
                    form.append('key', keyFileInput.files[0]);
                    const serverName = created.name || trimmedName || name;
                    const res = await fetch(`/api/servers/${encodeURIComponent(serverName)}/key`, { method: 'POST', body: form });
                    if (!res.ok) {
                        const uploadError = await parseErrorResponse(res, 'Failed to upload key.');
                        const rollback = await fetch(`/api/servers/${encodeURIComponent(serverName)}`, { method: 'DELETE' }).catch(() => null);
                        managePageInteraction.dispatch({ type: 'commandFailed', plan: execution.plan, message: uploadError });
                        if (rollback && rollback.ok) {
                            window.notifyApp(`Server was not saved because key upload failed: ${uploadError}`);
                        } else {
                            window.notifyApp(`Key upload failed and the server could not be removed automatically: ${uploadError}`);
                            fetchManageServers();
                        }
                        return;
                    }
                }
                if (accepted.trustHostKey) {
                    try {
                        await trustHostKeyFlow(created.host || host.trim(), normalizePort(created.port, 22));
                    } catch (err) {
                        window.notifyApp(`Server added, but host key was not trusted: ${err.message || 'unknown error'}`);
                    }
                }
                managePageInteraction.dispatch({ type: 'commandCompleted', plan: execution.plan, message: 'Server added.' });
                if (keyFileInput) {
                    keyFileInput.value = '';
                    resetFileInputLabel(keyFileInput);
                }
                fetchManageServers();
                e.target.reset();
                document.getElementById('trust-host-key').checked = true;
            } catch (err) {
                managePageInteraction.dispatch({ type: 'commandFailed', plan: execution.plan, message: err?.message || 'Failed to add server.' });
                window.notifyApp(err?.message || 'Failed to add server.');
            }
        });

        document.addEventListener('change', (e) => {
            if (e.target && e.target.classList.contains('file-input')) {
                resetFileInputLabel(e.target);
            }
        });

        async function deleteServer(name) {
            if (await window.confirmTypedAction(`Delete server "${name}"?`, name)) {
                const execution = commandExecution('deleteServer', { serverName: name });
                if (!execution) {
                    window.notifyApp('This server action is already in progress.');
                    return;
                }
                try {
                    const response = await fetch(`/api/servers/${encodeURIComponent(name)}`, { method: 'DELETE' });
                    if (!response.ok) {
                        throw new Error(await parseErrorResponse(response, 'Failed to delete server.'));
                    }
                    managePageInteraction.dispatch({ type: 'commandCompleted', plan: execution.plan, message: 'Server deleted.' });
                    await fetchManageServers();
                } catch (error) {
                    managePageInteraction.dispatch({ type: 'commandFailed', plan: execution.plan, message: error?.message || 'Failed to delete server.' });
                    window.notifyApp(error?.message || 'Failed to delete server.');
                }
            }
        }

            async function editServer(name) {
                const current = managePageInteraction.getView().inventory.allItems.find(server => server.name === name) || {};
                managePageInteraction.dispatch({ type: 'editorOpened', name, server: current });
            document.getElementById('edit-name').value = current.name || name;
            document.getElementById('edit-host').value = current.host || '';
            document.getElementById('edit-port').value = current.port || '';
            document.getElementById('edit-user').value = current.user || '';
            document.getElementById('edit-tags').value = (current.tags || []).join(', ');
            document.getElementById('edit-pass').value = '';
            document.getElementById('edit-trust-host-key').checked = true;
            const keyInput = document.getElementById('edit-key');
            if (keyInput) {
                keyInput.value = '';
                resetFileInputLabel(keyInput);
                }
                setEditHostKeyStatus('');
                clearEditValidationState();
                setEditSaveButtonState(false);
                setEditKnownHostButtonsState(false);
	                const editModal = document.getElementById('edit-modal');
	                editModal.classList.add('active');
	                activateModalFocus(editModal, document.getElementById('edit-name'));
	                checkEditKnownHostStatus();
                document.getElementById('edit-policy-overrides').innerHTML = '<div class="subtle">Loading matching policies...</div>';
                try {
                    await managePolicyOverrides.fetchContext(name);
                    managePolicyOverrides.render(document.getElementById('edit-policy-overrides'));
                } catch (err) {
                    document.getElementById('edit-policy-overrides').innerHTML = `<div class="subtle">${escapeHtml(err.message || 'Failed to load scheduled policies.')}</div>`;
                }
            }

            function closeEditModal() {
	                const editModal = document.getElementById('edit-modal');
	                editModal.classList.remove('active');
	                releaseModalFocus(editModal);
	                setEditHostKeyStatus('');
                clearEditValidationState();
                setEditSaveButtonState(false);
                setEditKnownHostButtonsState(false);
                managePageInteraction.dispatch({ type: 'editorClosed' });
                const overrides = document.getElementById('edit-policy-overrides');
                if (overrides) {
                    overrides.innerHTML = '';
                }
            }

        function setEditHostKeyStatus(message) {
            const el = document.getElementById('edit-hostkey-status');
            if (!el) return;
            el.textContent = String(message || '').trim();
        }

            function setEditValidationError(message) {
                const el = document.getElementById('edit-error');
                if (!el) return;
                el.textContent = String(message || '').trim();
            }

            function setEditFieldInvalidState(fieldId, isInvalid) {
                const input = document.getElementById(fieldId);
                if (!input) return;
                input.classList.toggle('is-invalid', !!isInvalid);
                if (isInvalid) {
                    input.setAttribute('aria-invalid', 'true');
                } else {
                    input.removeAttribute('aria-invalid');
                }
            }

            function maybeClearEditValidationError() {
                const requiredFields = ['edit-name', 'edit-host', 'edit-user'];
                const hasInvalid = requiredFields.some((fieldId) => {
                    const input = document.getElementById(fieldId);
                    return !!input && input.classList.contains('is-invalid');
                });
                if (!hasInvalid) {
                    setEditValidationError('');
                }
            }

            function clearEditValidationState() {
                setEditValidationError('');
                setEditFieldInvalidState('edit-name', false);
                setEditFieldInvalidState('edit-host', false);
                setEditFieldInvalidState('edit-user', false);
            }

            function setEditSaveButtonState(isBusy, label) {
                const saveBtn = document.getElementById('edit-save');
                const cancelBtn = document.getElementById('edit-cancel');
                if (!saveBtn) return;
                saveBtn.disabled = !!isBusy;
                saveBtn.textContent = isBusy ? (label || 'Saving...') : 'Save';
                if (cancelBtn) {
                    cancelBtn.disabled = !!isBusy;
                }
            }

            function setEditKnownHostButtonsState(isBusy, checkLabel, clearLabel) {
                const checkBtn = document.getElementById('edit-check-known-host');
                const clearBtn = document.getElementById('edit-clear-known-host');
                if (checkBtn) {
                    checkBtn.disabled = !!isBusy;
                    checkBtn.textContent = isBusy ? (checkLabel || 'Checking...') : 'Check Known Host';
                }
                if (clearBtn) {
                    clearBtn.disabled = !!isBusy;
                    clearBtn.textContent = isBusy ? (clearLabel || 'Clearing...') : 'Clear Known Host';
                }
            }

            async function checkEditKnownHostStatus() {
                if (!activeEditorName()) return;
                const host = document.getElementById('edit-host').value.trim();
                const port = normalizePort(document.getElementById('edit-port').value, 22);
                if (!host) {
                    setEditHostKeyStatus('Known host status: host is required.');
                    return;
                }
                const request = managePageInteraction.dispatch({ type: 'snapshotRequested', stream: 'hostKey' })
                    .find((effect) => effect.type === 'fetchSnapshot');
                if (!request) return editKnownHostCheckPromise;
                const sessionID = managePageInteraction.getView().editor.sessionID;
                const currentCheck = (async () => {
                    setEditKnownHostButtonsState(true, 'Checking...', 'Clear Known Host');
                    setEditHostKeyStatus('Checking known_hosts entry...');
                    try {
                        const scanned = await scanHostKey(host, port);
                        const currentHost = document.getElementById('edit-host').value.trim();
                        const currentPort = normalizePort(document.getElementById('edit-port').value, 22);
                        if (currentHost !== host || currentPort !== port) {
                            return;
                        }
                        managePageInteraction.dispatch({ type: 'hostKeyReceived', requestID: request.requestID, sessionID, host, port, hostKey: { fingerprint: scanned?.fingerprint_sha256 || '', alreadyTrusted: !!scanned?.already_trusted } });
                        if (scanned?.already_trusted) {
                            setEditHostKeyStatus(`Known host saved for ${host}:${port} (${scanned.fingerprint_sha256}).`);
                        } else {
                            setEditHostKeyStatus(`Known host not saved for ${host}:${port} (${scanned.fingerprint_sha256}).`);
                        }
                    } catch (err) {
                        const currentHost = document.getElementById('edit-host').value.trim();
                        const currentPort = normalizePort(document.getElementById('edit-port').value, 22);
                        if (currentHost !== host || currentPort !== port) {
                            return;
                        }
                        managePageInteraction.dispatch({ type: 'snapshotFailed', stream: 'hostKey', requestID: request.requestID, error: err?.message });
                        setEditHostKeyStatus(`Known host check failed: ${err.message || 'unknown error'}`);
                    } finally {
                        if (editKnownHostCheckPromise === currentCheck) {
                            setEditKnownHostButtonsState(false);
                        }
                    }
                })();
                editKnownHostCheckPromise = currentCheck;
                try {
                    await currentCheck;
                } finally {
                    if (editKnownHostCheckPromise === currentCheck) {
                        editKnownHostCheckPromise = null;
                        setEditKnownHostButtonsState(false);
                    }
                }
            }

            async function clearEditKnownHost() {
                if (!activeEditorName()) return;
                const host = document.getElementById('edit-host').value.trim();
                const port = normalizePort(document.getElementById('edit-port').value, 22);
                if (!host) {
                    window.notifyApp('Host is required.');
                    return;
                }
                if (!(await window.confirmTypedAction(`Remove known_hosts entry for ${host}:${port}?`, `${host}:${port}`))) {
                    return;
                }
                const execution = commandExecution('clearHostKey', { serverName: activeEditorName(), host, port });
                if (!execution) {
                    window.notifyApp('Known host action is already in progress.');
                    return;
                }
                setEditKnownHostButtonsState(true, 'Check Known Host', 'Clearing...');
                try {
                    const result = await clearKnownHost(host, port);
                    managePageInteraction.dispatch({ type: 'hostKeyCleared', sessionID: managePageInteraction.getView().editor.sessionID, host, port });
                    managePageInteraction.dispatch({ type: 'commandCompleted', plan: execution.plan, message: 'Known host entry cleared.' });
                    if (Number(result?.removed_entries || 0) > 0) {
                        setEditHostKeyStatus(`Known host entry cleared for ${host}:${port}.`);
                    } else {
                        setEditHostKeyStatus(`Known host entry not found for ${host}:${port}.`);
                    }
                } catch (err) {
                    managePageInteraction.dispatch({ type: 'commandFailed', plan: execution.plan, message: err.message || 'Failed to clear known host entry.' });
                    window.notifyApp(err.message || 'Failed to clear known host entry.');
                } finally {
                    setEditKnownHostButtonsState(false);
                }
            }

        async function saveServerEdit() {
            const originalName = activeEditorName();
            if (!originalName) return;
            const newName = document.getElementById('edit-name').value.trim();
            const newHost = document.getElementById('edit-host').value.trim();
            const portValue = document.getElementById('edit-port').value;
            const newPort = portValue ? parseInt(portValue, 10) : 0;
            const newUser = document.getElementById('edit-user').value.trim();
            const tagsRaw = document.getElementById('edit-tags').value;
            const tags = tagsRaw.split(',').map(t => t.trim()).filter(Boolean);
            const newPass = document.getElementById('edit-pass').value;
            const current = managePageInteraction.getView().inventory.allItems.find(server => server.name === originalName) || {};
            const currentPort = normalizePort(current.port, 22);
            const targetPort = normalizePort(newPort || currentPort, 22);
            clearEditValidationState();
            const command = managePageInteraction.dispatch({ type: 'commandRequested', command: 'saveEditor' });
            const execution = command.find((effect) => effect.type === 'executeCommand');
            if (!execution) {
                const rejected = command.find((effect) => effect.type === 'commandRejected') || {};
                const fieldIDs = { name: 'edit-name', host: 'edit-host', user: 'edit-user' };
                for (const field of rejected.invalidFields || []) {
                    setEditFieldInvalidState(fieldIDs[field], true);
                }
                setEditValidationError(rejected.reason || 'This server action is already in progress.');
                const firstInvalid = document.getElementById(fieldIDs[(rejected.invalidFields || [])[0]]);
                if (firstInvalid) firstInvalid.focus();
                return;
            }
            setEditSaveButtonState(true, 'Saving...');
            try {
                const accepted = execution.plan.payload;
                const res = await fetch(`/api/servers/${encodeURIComponent(originalName)}`, {
                    method: 'PUT',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ name: accepted.name, host: accepted.host, port: accepted.port, user: accepted.user, pass: newPass, tags: accepted.tags })
                });
                if (!res.ok) throw new Error(await parseErrorResponse(res, 'Failed to save server.'));
                managePageInteraction.dispatch({ type: 'editorIdentityAccepted', sessionID: accepted.sessionID, name: accepted.name });
                if (accepted.trustHostKey) {
                    if (editKnownHostCheckPromise) await editKnownHostCheckPromise;
                    if (isEditKnownHostTrusted(newHost, targetPort)) {
                        setEditHostKeyStatus('Host key already saved in known_hosts.');
                    } else {
                        try {
                            const trustResult = await trustHostKeyFlow(newHost, targetPort, {
                                onScanning: () => {
                                    setEditHostKeyStatus('Checking known_hosts entry...');
                                },
                                onScanned: () => {
                                    setEditHostKeyStatus('Host key scanned. Waiting confirmation...');
                                },
                                onAlreadyTrusted: () => {
                                    setEditHostKeyStatus('Host key already saved in known_hosts.');
                                },
                                onTrusting: () => {
                                    setEditHostKeyStatus('Saving host key to known_hosts...');
                                }
                            });
                            const scannedFp = trustResult?.scanned?.fingerprint_sha256 || trustResult?.trusted?.fingerprint_sha256 || '';
                            managePageInteraction.dispatch({
                                type: 'hostKeyReceived',
                                sessionID: managePageInteraction.getView().editor.sessionID,
                                host: newHost,
                                port: targetPort,
                                hostKey: { fingerprint: scannedFp, alreadyTrusted: true }
                            });
                            if (!trustResult?.alreadyTrusted) {
                                setEditHostKeyStatus('Host key trusted.');
                            }
                        } catch (err) {
                            window.notifyApp(`Server saved, but host key was not trusted: ${err.message || 'unknown error'}`);
                            setEditHostKeyStatus('');
                        }
                    }
                }
                let overrideSaveError = null;
                try {
                    const outcome = await managePolicyOverrides.save(newName, accepted.policyOverrides);
                    if (outcome.status === 'partial' || outcome.status === 'failure') {
                        const failures = outcome.failures.map(failure => `${failure.policyID}: ${failure.error}`).join('; ');
                        throw new Error(`Failed to save scheduled update overrides for policy IDs ${failures}`);
                    }
                } catch (err) {
                    overrideSaveError = err;
                }
                managePageInteraction.dispatch({ type: 'commandCompleted', plan: execution.plan, message: 'Server saved.' });
                closeEditModal();
                fetchManageServers();
                if (overrideSaveError) {
                    window.notifyApp(`Server saved, but scheduled update overrides were not fully saved: ${overrideSaveError?.message || 'unknown error'}`);
                }
            } catch (err) {
                managePageInteraction.dispatch({ type: 'commandFailed', plan: execution.plan, message: err?.message || 'Failed to save server.' });
                window.notifyApp(err?.message || 'Failed to save server.');
                setEditHostKeyStatus('');
            } finally {
                setEditSaveButtonState(false);
            }
        }

        async function uploadServerKey(name) {
            const input = document.getElementById('edit-key');
            if (!input || !input.files || input.files.length === 0) {
                window.notifyApp('Select a private key file to upload.');
                return;
            }
            const execution = commandExecution('uploadServerKey', { serverName: name });
            if (!execution) { window.notifyApp('This server action is already in progress.'); return; }
            const form = new FormData();
            form.append('key', input.files[0]);
            try {
                const res = await fetch(`/api/servers/${encodeURIComponent(name)}/key`, { method: 'POST', body: form });
                if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || 'Failed to upload key.');
                managePageInteraction.dispatch({ type: 'commandCompleted', plan: execution.plan, message: 'Server key uploaded.' });
                input.value = '';
                resetFileInputLabel(input);
                fetchManageServers();
            } catch (err) {
                managePageInteraction.dispatch({ type: 'commandFailed', plan: execution.plan, message: err?.message || 'Failed to upload key.' });
                window.notifyApp(err?.message || 'Failed to upload key.');
            }
        }

        async function clearServerKey(name) {
            const execution = commandExecution('clearServerKey', { serverName: name });
            if (!execution) { window.notifyApp('This server action is already in progress.'); return; }
            try {
                const res = await fetch(`/api/servers/${encodeURIComponent(name)}/key`, { method: 'DELETE' });
                if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || 'Failed to clear key.');
                managePageInteraction.dispatch({ type: 'commandCompleted', plan: execution.plan, message: 'Server key cleared.' });
                fetchManageServers();
            } catch (err) {
                managePageInteraction.dispatch({ type: 'commandFailed', plan: execution.plan, message: err?.message || 'Failed to clear key.' });
                window.notifyApp(err?.message || 'Failed to clear key.');
            }
        }

        async function clearServerPassword(name) {
            const execution = commandExecution('clearServerPassword', { serverName: name });
            if (!execution) { window.notifyApp('This server action is already in progress.'); return; }
            try {
                const res = await fetch(`/api/servers/${encodeURIComponent(name)}/password`, { method: 'DELETE' });
                if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || 'Failed to clear password.');
                managePageInteraction.dispatch({ type: 'commandCompleted', plan: execution.plan, message: 'Server password cleared.' });
                fetchManageServers();
            } catch (err) {
                managePageInteraction.dispatch({ type: 'commandFailed', plan: execution.plan, message: err?.message || 'Failed to clear password.' });
                window.notifyApp(err?.message || 'Failed to clear password.');
            }
        }

        document.getElementById('edit-cancel').addEventListener('click', closeEditModal);
        document.getElementById('edit-save').addEventListener('click', saveServerEdit);
        document.getElementById('edit-upload-key').addEventListener('click', () => {
            if (activeEditorName()) {
                uploadServerKey(activeEditorName());
            }
        });
        document.getElementById('edit-clear-key').addEventListener('click', () => {
            if (activeEditorName()) {
                clearServerKey(activeEditorName());
            }
        });
            document.getElementById('edit-clear-password').addEventListener('click', () => {
                if (activeEditorName()) {
                    clearServerPassword(activeEditorName());
                }
            });
            document.getElementById('edit-name').addEventListener('input', () => {
                managePageInteraction.dispatch({ type: 'editorChanged', patch: { name: document.getElementById('edit-name').value } });
                setEditFieldInvalidState('edit-name', false);
                maybeClearEditValidationError();
            });
            document.getElementById('edit-host').addEventListener('input', () => {
                managePageInteraction.dispatch({ type: 'editorChanged', patch: { host: document.getElementById('edit-host').value } });
                setEditFieldInvalidState('edit-host', false);
                maybeClearEditValidationError();
                if (activeEditorName()) {
                    editKnownHostCheckPromise = null;
                    setEditKnownHostButtonsState(false);
                    setEditHostKeyStatus('Host/port changed. Click "Check Known Host" to refresh status.');
                }
            });
            document.getElementById('edit-port').addEventListener('input', () => {
                managePageInteraction.dispatch({ type: 'editorChanged', patch: { port: document.getElementById('edit-port').value } });
                if (activeEditorName()) {
                    editKnownHostCheckPromise = null;
                    setEditKnownHostButtonsState(false);
                    setEditHostKeyStatus('Host/port changed. Click "Check Known Host" to refresh status.');
                }
            });
            document.getElementById('edit-tags').addEventListener('input', () => {
                managePageInteraction.dispatch({ type: 'editorChanged', patch: { tags: document.getElementById('edit-tags').value } });
                if (activeEditorName()) {
                    managePolicyOverrides.render(document.getElementById('edit-policy-overrides'));
                }
            });
            document.getElementById('edit-policy-overrides').addEventListener('change', (event) => {
                const checkbox = event.target.closest('input[data-policy-id]');
                if (checkbox) managePolicyOverrides.change(checkbox.dataset.policyId, checkbox.checked);
            });
            document.getElementById('edit-user').addEventListener('input', () => {
                managePageInteraction.dispatch({ type: 'editorChanged', patch: { user: document.getElementById('edit-user').value } });
                setEditFieldInvalidState('edit-user', false);
                maybeClearEditValidationError();
            });
            document.getElementById('edit-trust-host-key').addEventListener('change', () => {
                managePageInteraction.dispatch({ type: 'editorOptionChanged', patch: { trustHostKey: document.getElementById('edit-trust-host-key').checked } });
            });
            document.getElementById('edit-check-known-host').addEventListener('click', () => {
                if (activeEditorName()) {
                    checkEditKnownHostStatus();
                }
            });
            document.getElementById('edit-clear-known-host').addEventListener('click', () => {
                if (activeEditorName()) {
                    clearEditKnownHost();
                }
            });
            document.getElementById('hostkey-modal-cancel').addEventListener('click', () => closeHostKeyModal(false));
        document.getElementById('hostkey-modal-trust').addEventListener('click', () => closeHostKeyModal(true));
        document.getElementById('audit-detail-close').addEventListener('click', closeAuditDetailDrawer);
        document.getElementById('audit-detail-modal').addEventListener('click', (e) => {
            if (e.target && e.target.id === 'audit-detail-modal') {
                closeAuditDetailDrawer();
            }
        });
        document.getElementById('hostkey-modal').addEventListener('click', (e) => {
            if (e.target && e.target.id === 'hostkey-modal') {
                closeHostKeyModal(false);
            }
	        });
        document.addEventListener('keydown', (e) => {
            if (e.key === 'Tab' && trapActiveModalFocus(e)) {
                return;
            }
            if (e.key === 'Escape') {
                const hostKeyModal = document.getElementById('hostkey-modal');
                if (hostKeyModal && hostKeyModal.classList.contains('active')) {
                    closeHostKeyModal(false);
                    return;
                }
                const auditDetailModal = document.getElementById('audit-detail-modal');
                if (auditDetailModal && auditDetailModal.classList.contains('active')) {
                    closeAuditDetailDrawer();
                    return;
                }
                const editModal = document.getElementById('edit-modal');
                if (editModal && editModal.classList.contains('active')) {
                    if (managePageInteraction.getView().commands.inFlight.some((key) => key.startsWith('saveEditor:'))) {
                        return;
                    }
                    closeEditModal();
                }
            }
        });

        async function uploadGlobalKey() {
            const input = document.getElementById('global-key-file');
            if (!input || !input.files || input.files.length === 0) {
                window.notifyApp('Select a private key file to upload.');
                return;
            }
            const command = managePageInteraction.dispatch({ type: 'commandRequested', command: 'globalKeyUpload' });
            const execution = command.find((effect) => effect.type === 'executeCommand');
            if (!execution) { window.notifyApp('Global key action is already in progress.'); return; }
            const form = new FormData();
            form.append('key', input.files[0]);
            const res = await fetch('/api/keys/global', { method: 'POST', body: form });
            if (!res.ok) {
                const data = await res.json().catch(() => ({}));
                await settleCommand('commandFailed', execution.plan, data.error || 'Failed to upload global key.');
                return;
            }
            await settleCommand('commandCompleted', execution.plan, 'Global key saved.');
            input.value = '';
            resetFileInputLabel(input);
        }

        async function clearGlobalKey() {
            if (!(await window.confirmTypedAction('Clear the global SSH key?', 'CLEAR GLOBAL KEY'))) {
                return;
            }
            const command = managePageInteraction.dispatch({ type: 'commandRequested', command: 'globalKeyClear' });
            const execution = command.find((effect) => effect.type === 'executeCommand');
            if (!execution) { window.notifyApp('Global key action is already in progress.'); return; }
            const res = await fetch('/api/keys/global', { method: 'DELETE' });
            if (!res.ok) {
                const data = await res.json().catch(() => ({}));
                await settleCommand('commandFailed', execution.plan, data.error || 'Failed to clear global key.');
                return;
            }
            await settleCommand('commandCompleted', execution.plan, 'Global key cleared.');
        }

        async function fetchGlobalKeyStatus() {
            const status = document.getElementById('global-key-status');
            if (!status) return;
            const request = managePageInteraction.dispatch({ type: 'snapshotRequested', stream: 'globalKey' })
                .find((effect) => effect.type === 'fetchSnapshot');
            if (!request) return;
            try {
                const res = await fetch('/api/keys/global');
                if (!res.ok) throw new Error(await parseErrorResponse(res, 'unknown'));
                const data = await res.json();
                managePageInteraction.dispatch({ type: 'globalKeySnapshotReceived', requestID: request.requestID, hasKey: !!data.has_key });
                status.textContent = data.has_key ? 'Global key: saved' : 'Global key: not set';
                if (managePageInteraction.getView().inventory.allItems.length > 0) renderTable();
            } catch (err) {
                managePageInteraction.dispatch({ type: 'snapshotFailed', stream: 'globalKey', requestID: request.requestID, error: err.message || 'unknown' });
                status.textContent = `Global key status: ${err.message || 'unknown'}`;
            }
        }

        document.getElementById('logout-btn').addEventListener('click', () => window.logout());
        document.getElementById('upload-global-key-btn').addEventListener('click', uploadGlobalKey);
        document.getElementById('clear-global-key-btn').addEventListener('click', clearGlobalKey);
        fetchManageServers();
        fetchGlobalKeyStatus();
        fetchAuditEvents();
        setInterval(fetchAuditEvents, 15000);
