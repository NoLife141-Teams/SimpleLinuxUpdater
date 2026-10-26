(() => {
    const byID = id => document.getElementById(id);
    const status = message => { byID('account-status').textContent = message; };
    const nativeFetch = window.__nativeFetch || window.fetch.bind(window);
    async function request(path, method = 'GET', body) {
        const response = await nativeFetch(path, { method, cache: 'no-store', headers: { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
        const data = await response.json().catch(() => ({}));
        if (!response.ok) {
            if (data.reauthentication_required) byID('reauth-password').focus();
            throw new Error(data.error || `Request failed (${response.status})`);
        }
        return data;
    }
    function on(id, event, fn) { byID(id).addEventListener(event, async e => { e.preventDefault(); try { await fn(e); } catch (error) { status(error.message); } }); }
    function button(label, action) { const el = document.createElement('button'); el.type = 'button'; el.textContent = label; el.addEventListener('click', async () => { try { await action(); } catch (e) { status(e.message); } }); return el; }
    function row(text) { const el = document.createElement('li'); el.textContent = text; return el; }
    async function loadTokens() {
        const { tokens } = await request('/api/auth/tokens'); byID('token-list').replaceChildren(...tokens.map(t => {
            const el = row(`${t.name} · ${t.scopes.join(', ')} · expires ${t.expires_at} · ${t.revoked ? 'revoked' : `last used ${t.last_used_at || 'never'} ${t.last_used_origin || ''}`}`);
            if (!t.revoked) el.append(button('Revoke', async () => { await request(`/api/auth/tokens/${encodeURIComponent(t.id)}`, 'DELETE'); await loadTokens(); status('Token revoked.'); })); return el;
        }));
    }
    async function loadAccounts() {
        const { accounts } = await request('/api/auth/accounts'); byID('account-list').replaceChildren(...accounts.map(a => {
            const el = row(`${a.username} · account ${a.id} · ${a.disabled ? 'disabled' : 'enabled'} · MFA ${a.mfa_enabled ? 'enabled' : 'disabled'}`);
            const select = document.createElement('select'); select.setAttribute('aria-label', `Role for ${a.username}`);
            for (const role of ['reader', 'operator', 'admin']) { const option = document.createElement('option'); option.value = role; option.textContent = role; option.selected = a.role === role; select.append(option); }
            el.append(select, button('Save role', async () => { await request(`/api/auth/accounts/${a.id}`, 'PUT', { role: select.value, disabled: a.disabled }); await loadAccounts(); status('Role updated. Existing sessions were revoked.'); }), button(a.disabled ? 'Enable' : 'Disable', async () => { await request(`/api/auth/accounts/${a.id}`, 'PUT', { role: a.role, disabled: !a.disabled }); await loadAccounts(); status('Account updated.'); })); return el;
        }));
    }
    async function loadSessions() {
        const { sessions } = await request('/api/auth/sessions'); byID('session-list').replaceChildren(...sessions.map(s => {
            const el = row(`${s.current ? 'Current session' : s.client_label || 'Session'} · ${s.client_ip || ''} · expires ${s.expires_at}`);
            el.append(button('Revoke session', async () => { await request(`/api/auth/sessions/${s.id}`, 'DELETE'); if (s.current) location.href = '/login'; else await loadSessions(); })); return el;
        }));
    }
    on('reauth-form', 'submit', async () => { await request('/api/auth/reauthenticate', 'POST', { password: byID('reauth-password').value, code: byID('reauth-code').value }); byID('reauth-form').reset(); status('Identity confirmed for five minutes.'); });
    on('mfa-enroll', 'click', async () => { const data = await request('/api/auth/mfa/enroll', 'POST'); byID('mfa-secret').textContent = data.secret; byID('mfa-enrollment').hidden = false; byID('mfa-confirm-code').focus(); });
    on('mfa-confirm-form', 'submit', async () => { const data = await request('/api/auth/mfa/confirm', 'POST', { code: byID('mfa-confirm-code').value }); byID('mfa-secret').textContent = ''; byID('mfa-enrollment').hidden = true; byID('recovery-codes').textContent = data.recovery_codes.join('\n'); byID('recovery-result').hidden = false; status(data.message); });
    on('mfa-disable', 'click', async () => { await request('/api/auth/mfa', 'DELETE'); location.href = '/login'; });
    on('token-form', 'submit', async () => {
        const scopes = [...byID('token-scopes').querySelectorAll('input:checked')].map(el => el.value);
        const data = await request('/api/auth/tokens', 'POST', { name: byID('token-name').value, scopes, expires_at: new Date(byID('token-expiry').value).toISOString() });
        byID('token-secret').textContent = data.secret; byID('token-result').hidden = false; await loadTokens(); status('Personal token created. Save its secret now.');
    });
    on('token-dismiss', 'click', () => { byID('token-secret').textContent = ''; byID('token-result').hidden = true; status('Token secret cleared from this page.'); });
    on('account-create-form', 'submit', async () => { await request('/api/auth/accounts', 'POST', { username: byID('new-account-name').value, password: byID('new-account-password').value, role: byID('new-account-role').value }); byID('account-create-form').reset(); await loadAccounts(); status('Named account created.'); });
    on('account-password-form', 'submit', async () => { await request('/api/auth/password', 'PUT', { current_password: byID('account-current-password').value, new_password: byID('account-new-password').value, confirm_password: byID('account-confirm-password').value, invalidate_other_sessions: true }); byID('account-password-form').reset(); await loadSessions(); status('Password changed; other sessions revoked.'); });
    byID('logout-btn').addEventListener('click', window.logout);
    window.addEventListener('pagehide', () => { byID('token-secret').textContent = ''; byID('mfa-secret').textContent = ''; byID('recovery-codes').textContent = ''; });
    (async () => {
        const { account, scopes } = await request('/api/auth/identity'); byID('identity-summary').textContent = `${account.username} · ${account.role} · account ${account.id}`;
        byID('mfa-status').textContent = account.mfa_enabled ? 'Authenticator enabled.' : 'Authenticator is optional and currently disabled.'; byID('mfa-enroll').hidden = account.mfa_enabled; byID('mfa-disable').hidden = !account.mfa_enabled;
        for (const scope of scopes) { const label = document.createElement('label'); const input = document.createElement('input'); input.type = 'checkbox'; input.value = scope; input.checked = scope === 'operational:read'; label.append(input, document.createTextNode(scope)); byID('token-scopes').append(label); }
        const expiry = new Date(Date.now() + 30 * 86400000); expiry.setMinutes(expiry.getMinutes() - expiry.getTimezoneOffset()); byID('token-expiry').value = expiry.toISOString().slice(0, 16);
        await Promise.all([loadTokens(), loadSessions()]); if (account.role === 'admin') { byID('accounts-section').hidden = false; await loadAccounts(); }
    })().catch(error => status(error.message));
})();
