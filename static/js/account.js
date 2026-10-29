(() => {
    const byID = id => document.getElementById(id);
    const feedbackTargets = {
        'reauth-form': ['reauth-feedback', 'reauth-password'],
        'mfa-enroll': ['mfa-feedback', 'mfa-enroll'],
        'mfa-confirm': ['mfa-feedback', 'mfa-confirm-code'],
        'mfa-disable': ['mfa-feedback', 'mfa-disable'],
        'token-form': ['token-feedback', 'token-name'],
        'token-revoke': ['token-feedback', 'token-list'],
        'account-password-form': ['password-feedback', 'account-current-password'],
        'session-revoke': ['session-feedback', 'session-list'],
        'account-create-form': ['accounts-feedback', 'new-account-name'],
        'account-update': ['accounts-feedback', 'account-list'],
    };
    function status(message, state = 'success', operation = commands.view().busy, fieldID = '') {
        const target = feedbackTargets[operation];
        const summary = byID('account-status');
        // Keep the overview useful without announcing the same message twice.
        summary.setAttribute('aria-live', target ? 'off' : 'polite');
        summary.dataset.state = state;
        summary.textContent = message;
        if (!target) return;
        const region = byID(target[0]);
        region.hidden = !message;
        region.dataset.state = state;
        region.setAttribute('role', state === 'error' ? 'alert' : 'status');
        region.setAttribute('aria-live', state === 'error' ? 'assertive' : 'polite');
        region.textContent = message;
        if (message && state === 'error') {
            const link = document.createElement('a');
            const field = byID(fieldID || target[1]);
            link.href = `#${fieldID || target[1]}`;
            link.textContent = 'Review the affected fields';
            link.setAttribute('aria-label', `${link.textContent}: ${operationLabels[operation] || 'Account action'}`);
            lifecycle.listen(link, 'click', event => {
                event.preventDefault();
                field?.scrollIntoView({ block: 'center' });
                field?.focus({ preventScroll: true });
            });
            region.append(link);
        }
    }
    const lifecycle = window.BrowserEffects.create();
    lifecycle.init();
    const commands = window.AccountCommandState.create();
    const auth = window.Reauthentication.create(lifecycle);
    const operationLabels = {
        'mfa-enroll': 'Set up authenticator', 'mfa-confirm': 'Enable MFA',
        'mfa-disable': 'Disable MFA', 'token-form': 'Create personal API token',
        'token-revoke': 'Revoke personal API token', 'account-create-form': 'Create named account',
        'account-update': 'Update account', 'account-password-form': 'Change password',
        'session-revoke': 'Revoke browser session',
    };
    let ownAccountID;
    const snapshots = { tokens: 0, accounts: 0, sessions: 0 };
    const snapshotRequests = {};
    const interruptedSnapshots = new Set();
    const lists = {
        tokens: { id: 'token-list', singular: 'personal token', plural: 'personal tokens', loaded: false },
        sessions: { id: 'session-list', singular: 'session', plural: 'sessions', loaded: false },
        accounts: { id: 'account-list', singular: 'named account', plural: 'named accounts', loaded: false },
    };
    function listState(name, state, message = '') {
        const config = lists[name], list = byID(config.id);
        byID(`${name}-state`).dataset.state = state;
        list.setAttribute('aria-busy', String(state === 'loading'));
        const count = list.children.length;
        byID(`${name}-status`).textContent = state === 'loading' ? `Loading ${config.plural}…`
            : state === 'paused' ? `Refresh of ${config.plural} will resume after the current account action.`
            : state === 'failed' ? `Could not load ${config.plural}. ${message}${config.loaded && count ? ' Previous results remain visible.' : ''}`
            : state === 'empty' ? `No ${config.plural}.`
            : `${count} ${count === 1 ? config.singular : config.plural}.`;
        const retry = byID(`${name}-state`).querySelector('button');
        retry.hidden = state !== 'failed';
        retry.setAttribute('aria-label', `Retry ${config.plural}`);
    }
    function invalidateSnapshots() {
        for (const name of Object.keys(snapshots)) {
            snapshots[name]++;
            if (snapshotRequests[name]) {
                interruptedSnapshots.add(name);
                snapshotRequests[name].abort();
                if (lifecycle.active) listState(name, 'paused');
            }
        }
    }
    lifecycle.own(invalidateSnapshots);
    function render() {
        const view = commands.view();
        document.querySelectorAll('main button, main form input, main form select').forEach(el => {
            const operation = el.closest('[data-account-command]')?.dataset.accountCommand;
            const ack = ['token-dismiss', 'mfa-cancel', 'recovery-dismiss'].includes(el.id);
            el.disabled = !ownAccountID || (ack ? Boolean(view.busy) : !commands.allowed(operation));
        });
        document.querySelectorAll('[data-account-command]').forEach(el => el.setAttribute('aria-busy', String(view.busy === el.dataset.accountCommand)));
    }
    async function request(path, method = 'GET', body, signal) {
        const options = { method, signal, headers: { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) };
        return method === 'GET' || path === '/api/auth/reauthenticate'
            ? auth.request(path, options)
            : auth.sensitive(path, options, operationLabels[commands.view().busy] || 'Account security action');
    }
    function clearPasswords() {
        document.querySelectorAll('main input[type=password], #reauth-code, #mfa-confirm-code').forEach(el => { el.value = ''; });
    }
    async function run(operation, fn) {
        // Identity initializes scopes and self-account handling before admission.
        // It must finish before a mutation can deliberately revoke the cookie.
        if (!ownAccountID) return;
        const ticket = commands.begin(operation);
        if (!ticket) return;
        const previousFocus = document.activeElement;
        let failed = false;
        status('', 'success', operation);
        invalidateSnapshots();
        render();
        try { await fn(); }
        catch (error) {
            if (commands.current(ticket) && error.name !== 'AbortError') {
                failed = true;
                status(error.message, 'error', operation, error.fieldID);
                byID(feedbackTargets[operation]?.[0] || 'account-status').focus();
            }
        } finally {
            clearPasswords();
            commands.finish(ticket);
            if (lifecycle.active) {
                render();
                const view = commands.view();
                if (view.secret === 'token') byID('token-dismiss').focus();
                else if (view.secret === 'recovery') byID('recovery-dismiss').focus();
                else if (view.secret === 'enrollment') byID('mfa-confirm-code').focus();
                else if (!failed && previousFocus?.isConnected) previousFocus.focus();
                resumeSnapshots();
            }
        }
    }
    function on(id, event, fn, operation = id) {
        const el = byID(id); el.dataset.accountCommand = operation;
        lifecycle.listen(el, event, e => { e.preventDefault(); void run(operation, () => fn(e)); });
    }
    function button(label, action, operation, className = 'btn-ghost') {
        const el = document.createElement('button'); el.type = 'button'; el.textContent = label;
        el.className = className;
        el.dataset.accountCommand = operation;
        lifecycle.listen(el, 'click', () => { void run(operation, action); }); return el;
    }
    function confirm(operation, consequences) {
        return window.confirmAction(consequences, { title: operation, operation, consequences, authentication: 'Recent password and factor confirmation required for sensitive account changes.', danger: true });
    }
    function row(text) { const el = document.createElement('li'); el.textContent = text; return el; }
    function formatTimestamp(node) {
        const raw = node.dataset.accountTimestamp;
        const formatted = window.formatAppTimestamp(raw, { titleUTC: true });
        node.textContent = formatted.primary;
        node.title = `${raw} · ${formatted.title}`;
    }
    function timestamp(raw, missing = 'Unavailable') {
        if (!raw) return document.createTextNode(missing);
        if (Number.isNaN(new Date(raw).getTime())) {
            const unavailable = document.createElement('span');
            unavailable.textContent = 'Unavailable';
            unavailable.title = String(raw);
            return unavailable;
        }
        const node = document.createElement('time');
        node.dateTime = new Date(raw).toISOString();
        node.dataset.accountTimestamp = raw;
        formatTimestamp(node);
        return node;
    }
    function refreshDatePresentation() {
        if (!lifecycle.active) return;
        byID('account-timezone').textContent = `Dates shown in ${window.getAppTimezoneLabel()}. Token expiry entry uses your browser timezone.`;
        document.querySelectorAll('time[data-account-timestamp]').forEach(formatTimestamp);
    }
    async function discoverDatePresentation() {
        // Optional display metadata must not use auth.js's redirecting fetch:
        // MFA can revoke the cookie while recovery codes await acknowledgement.
        const transport = window.BrowserEffects.create({ fetch: (...args) => (window.__nativeFetch || window.fetch.bind(window))(...args) });
        transport.init();
        const release = lifecycle.own(() => transport.destroy());
        try {
            const response = await transport.fetch('/api/app-settings/timezone', { cache: 'no-store' });
            if (!response.ok) {
                transport.discardResponse(response);
                return;
            }
            const data = await response.json();
            if (!lifecycle.active) return;
            window.setAppTimezoneCache(data);
            refreshDatePresentation();
        } catch (_) {
            // Keep the explicit UTC fallback if this passive read is unavailable.
        } finally {
            transport.destroy();
            release();
        }
    }
    async function snapshot(name, path) {
        const version = ++snapshots[name];
        snapshotRequests[name]?.abort();
        interruptedSnapshots.delete(name);
        const controller = new AbortController();
        snapshotRequests[name] = controller;
        listState(name, 'loading');
        try {
            const data = await request(path, 'GET', undefined, controller.signal);
            return lifecycle.active && snapshots[name] === version ? data : null;
        } catch (error) {
            if (!lifecycle.active || snapshots[name] !== version || error.name === 'AbortError') return null;
            throw error;
        } finally {
            if (snapshotRequests[name] === controller) delete snapshotRequests[name];
        }
    }
    async function loadList(name, path, apply) {
        try {
            const data = await snapshot(name, path);
            if (!data) return;
            if (!Array.isArray(data[name])) throw new Error('The list is unavailable. Retry to load it again.');
            apply(data);
            lists[name].loaded = true;
            listState(name, data[name].length ? 'current' : 'empty');
        } catch (error) {
            if (lifecycle.active && error.name !== 'AbortError') listState(name, 'failed', error.message);
        } finally {
            if (lifecycle.active) render();
        }
    }
    function resumeSnapshots() {
        const view = commands.view();
        if (!lifecycle.active || view.busy || view.secret || view.frozen) return;
        const loaders = { tokens: loadTokens, accounts: loadAccounts, sessions: loadSessions };
        for (const name of [...interruptedSnapshots]) {
            void loaders[name]().then(() => { if (lifecycle.active) render(); }).catch(error => {
                if (lifecycle.active && error.name !== 'AbortError') status(error.message, 'error');
            });
        }
    }
    async function loadTokens() {
        return loadList('tokens', '/api/auth/tokens', ({ tokens }) => {
            byID('token-list').replaceChildren(...tokens.map(t => {
                const el = row(`${t.name} · ${t.scopes.join(', ')} · expires `);
                el.append(timestamp(t.expires_at), document.createTextNode(t.revoked ? ' · revoked' : ' · last used '));
                if (!t.revoked) el.append(timestamp(t.last_used_at, 'never'), document.createTextNode(` ${t.last_used_origin || ''}`));
                if (!t.revoked) el.append(button('Revoke', async () => { if (!(await confirm('Revoke personal token', 'This token immediately stops authenticating. Existing admitted operations may finish.'))) return; await request(`/api/auth/tokens/${encodeURIComponent(t.id)}`, 'DELETE'); await loadTokens(); status('Token revoked.'); }, 'token-revoke', 'btn-danger')); return el;
            }));
        });
    }
    async function loadAccounts() {
        return loadList('accounts', '/api/auth/accounts', ({ accounts }) => {
            byID('account-list').replaceChildren(...accounts.map(a => {
                const el = row(`${a.username} · account ${a.id} · ${a.disabled ? 'disabled' : 'enabled'} · MFA ${a.mfa_enabled ? 'enabled' : 'disabled'}`);
                const select = document.createElement('select'); select.setAttribute('aria-label', `Role for ${a.username}`);
                for (const role of ['reader', 'operator', 'admin']) { const option = document.createElement('option'); option.value = role; option.textContent = role; option.selected = a.role === role; select.append(option); }
                el.append(select, button('Save role', async () => { if (!(await confirm('Change account role', 'All browser sessions for this account will be revoked. Personal tokens use the new role immediately.'))) return; await request(`/api/auth/accounts/${a.id}`, 'PUT', { role: select.value, disabled: a.disabled }); if (a.id === ownAccountID) { location.assign('/login'); return; } await loadAccounts(); status('Role updated. Existing sessions were revoked.'); }, 'account-update'), button(a.disabled ? 'Enable' : 'Disable', async () => { if (!(await confirm(a.disabled ? 'Enable account' : 'Disable account', 'Browser sessions for this account are revoked. Disabled accounts and their personal tokens cannot authenticate.'))) return; await request(`/api/auth/accounts/${a.id}`, 'PUT', { role: a.role, disabled: !a.disabled }); if (a.id === ownAccountID) { location.assign('/login'); return; } await loadAccounts(); status('Account updated.'); }, 'account-update', a.disabled ? 'btn-ghost' : 'btn-danger')); return el;
            }));
        });
    }
    async function loadSessions() {
        return loadList('sessions', '/api/auth/sessions', ({ sessions }) => {
            byID('session-list').replaceChildren(...sessions.map(s => {
                const el = row(`${s.current ? 'Current session' : s.client_label || 'Session'} · ${s.client_ip || ''} · expires `);
                el.append(timestamp(s.expires_at));
                el.append(button('Revoke session', async () => { if (!(await confirm('Revoke session', s.current ? 'This signs you out of the current browser session.' : 'This browser session will be signed out. Personal tokens are unchanged.'))) return; await request(`/api/auth/sessions/${s.id}`, 'DELETE'); if (s.current) location.href = '/login'; else { await loadSessions(); status('Browser session revoked.'); } }, 'session-revoke', 'btn-danger')); return el;
            }));
        });
    }
    on('reauth-form', 'submit', async () => { await request('/api/auth/reauthenticate', 'POST', { password: byID('reauth-password').value, code: byID('reauth-code').value }); byID('reauth-form').reset(); status('Identity confirmed for five minutes.'); });
    on('mfa-enroll', 'click', async () => { const data = await request('/api/auth/mfa/enroll', 'POST'); commands.protect('enrollment'); byID('mfa-secret').textContent = data.secret; byID('mfa-enrollment').hidden = false; status('Authenticator setup is ready. Save the secret and enter a fresh code to continue.'); byID('mfa-confirm-code').focus(); });
    on('mfa-confirm-form', 'submit', async () => { const data = await request('/api/auth/mfa/confirm', 'POST', { code: byID('mfa-confirm-code').value }); byID('mfa-secret').textContent = ''; byID('mfa-enrollment').hidden = true; commands.protect('recovery'); byID('recovery-codes').textContent = data.recovery_codes.join('\n'); byID('recovery-result').hidden = false; status(data.message); byID('recovery-dismiss').focus(); }, 'mfa-confirm');
    on('mfa-disable', 'click', async () => { if (!(await confirm('Disable MFA', 'MFA will be disabled. All your browser sessions and personal tokens will be revoked.'))) return; await request('/api/auth/mfa', 'DELETE'); location.href = '/login'; });
    on('token-form', 'submit', async () => {
        const scopes = [...byID('token-scopes').querySelectorAll('input:checked')].map(el => el.value);
        const data = await request('/api/auth/tokens', 'POST', { name: byID('token-name').value, scopes, expires_at: new Date(byID('token-expiry').value).toISOString() });
        commands.protect('token'); byID('token-secret').textContent = data.secret; byID('token-result').hidden = false; status('Personal token created. Save its secret now.'); byID('token-dismiss').focus();
    });
    lifecycle.listen(byID('token-dismiss'), 'click', () => {
        if (commands.view().busy) return;
        byID('token-secret').textContent = ''; byID('token-result').hidden = true;
        commands.acknowledge(); render(); status('Token secret cleared from this page.', 'success', 'token-form');
        byID('token-name').focus();
        interruptedSnapshots.add('tokens'); resumeSnapshots();
    });
    lifecycle.listen(byID('mfa-cancel'), 'click', () => {
        if (commands.view().busy) return;
        byID('mfa-secret').textContent = ''; byID('mfa-enrollment').hidden = true;
        byID('mfa-confirm-form').reset(); commands.acknowledge(); render(); status('Authenticator setup canceled; enrollment secret cleared.', 'success', 'mfa-enroll'); byID('mfa-enroll').focus(); resumeSnapshots();
    });
    lifecycle.listen(byID('recovery-dismiss'), 'click', () => {
        byID('recovery-codes').textContent = ''; byID('recovery-result').hidden = true;
        commands.acknowledge(); location.assign('/login');
    });
    on('account-create-form', 'submit', async () => { await request('/api/auth/accounts', 'POST', { username: byID('new-account-name').value, password: byID('new-account-password').value, role: byID('new-account-role').value }); byID('account-create-form').reset(); await loadAccounts(); status('Named account created.'); });
    on('account-password-form', 'submit', async () => {
        if (byID('account-new-password').value !== byID('account-confirm-password').value) {
            const error = new Error('New passwords do not match. Re-enter your passwords.');
            error.fieldID = 'account-confirm-password';
            throw error;
        }
        await request('/api/auth/password', 'PUT', { current_password: byID('account-current-password').value, new_password: byID('account-new-password').value, confirm_password: byID('account-confirm-password').value, invalidate_other_sessions: true });
        byID('account-password-form').reset(); await loadSessions(); status('Password changed; other sessions revoked.');
    });
    document.querySelectorAll('[data-account-list-retry]').forEach(retry => {
        lifecycle.listen(retry, 'click', () => {
            const view = commands.view();
            if (!ownAccountID || view.busy || view.secret || view.frozen) return;
            const loaders = { tokens: loadTokens, accounts: loadAccounts, sessions: loadSessions };
            void loaders[retry.dataset.accountListRetry]();
        });
    });
    lifecycle.listen(byID('logout-btn'), 'click', window.logout);
    window.addEventListener('pagehide', () => {
        for (const name of ['action-confirm', 'typed-confirm']) byID(`${name}-cancel`)?.click();
        lifecycle.destroy(); commands.destroy(); clearPasswords();
        for (const id of ['token-secret', 'mfa-secret', 'recovery-codes']) byID(id).textContent = '';
        for (const id of ['token-result', 'mfa-enrollment', 'recovery-result']) byID(id).hidden = true;
    });
    window.addEventListener('pageshow', event => { if (event.persisted) location.reload(); });
    render();
    refreshDatePresentation();
    // Timezone discovery only reformats accepted DOM facts. It never starts
    // another identity/list read or touches an unacknowledged secret.
    void discoverDatePresentation();
    (async () => {
        const { account, scopes } = await request('/api/auth/identity'); ownAccountID = account.id; byID('identity-summary').textContent = `${account.username} · ${account.role} · account ${account.id}`;
        byID('mfa-status').textContent = account.mfa_enabled ? 'Authenticator enabled.' : 'Authenticator is optional and currently disabled.'; byID('mfa-enroll').hidden = account.mfa_enabled; byID('mfa-disable').hidden = !account.mfa_enabled;
        for (const scope of scopes) { const label = document.createElement('label'); const input = document.createElement('input'); input.type = 'checkbox'; input.value = scope; input.checked = scope === 'operational:read'; label.append(input, document.createTextNode(scope)); byID('token-scopes').append(label); }
        const expiry = new Date(Date.now() + 30 * 86400000); expiry.setMinutes(expiry.getMinutes() - expiry.getTimezoneOffset()); byID('token-expiry').value = expiry.toISOString().slice(0, 16);
        if (account.role === 'admin') byID('accounts-section').hidden = false;
        render();
        await Promise.all([loadTokens(), loadSessions(), ...(account.role === 'admin' ? [loadAccounts()] : [])]); render();
    })().catch(error => { if (lifecycle.active && error.name !== 'AbortError') status(error.message, 'error'); });
})();
