// Reusable cookie-identity transport. The server owns all authentication policy.
(function (root) {
    'use strict';
    function create(lifecycle) {
        const lifetime = lifecycle.capture();
        const current = () => lifecycle.isCurrent(lifetime);
        const transport = root.BrowserEffects.create({ fetch: (...args) => (root.__nativeFetch || root.fetch.bind(root))(...args) });
        transport.init();
        let dialog = null, settle = null, resumePending = false;
        function close(value = false) {
            if (!dialog) return;
            dialog.querySelector('form').reset();
            dialog.close();
            dialog.remove();
            dialog = null;
            const resolve = settle; settle = null; resolve?.(value);
        }
        lifecycle.own(() => {
            close();
            if (resumePending) document.getElementById('action-confirm-cancel')?.click();
            transport.destroy();
        });
        async function request(path, options = {}) {
            const { responseType, ...requestOptions } = options;
            const response = await transport.fetch(path, { cache: 'no-store', ...requestOptions });
            if (response.ok && responseType === 'blob') return response.blob();
            const data = await response.json().catch(error => { if (error.name === 'AbortError') throw error; return {}; });
            if (options.signal?.aborted) throw new DOMException('Request superseded', 'AbortError');
            if (!response.ok) {
                if (response.status === 401 && !data.reauthentication_required) {
                    // Rejected credentials use 401 too; check the cookie separately.
                    let sessionEnded = true;
                    if (['/api/auth/reauthenticate', '/api/auth/mfa/confirm', '/api/auth/password'].includes(path)) {
                        const status = await transport.fetch('/api/auth/status', { cache: 'no-store' });
                        const identity = await status.json().catch(error => {
                            if (error.name === 'AbortError') throw error;
                            return {};
                        });
                        // An unavailable status check is not proof of sign-out.
                        sessionEnded = status.ok && identity.authenticated === false;
                    }
                    if (sessionEnded) { close(); root.location.assign('/login'); throw new DOMException('Session ended', 'AbortError'); }
                }
                const error = new Error(data.error || `Request failed (${response.status})`);
                error.reauthenticationRequired = Boolean(data.reauthentication_required);
                throw error;
            }
            return data;
        }
        async function confirmIdentity() {
            if (dialog || !current()) return false;
            const previousFocus = document.activeElement;
            dialog = document.createElement('dialog');
            dialog.className = 'reauthentication-dialog';
            dialog.setAttribute('aria-labelledby', 'reauthentication-title');
            dialog.innerHTML = '<form><h2 id="reauthentication-title">Confirm your identity</h2><p>Confirm your password and a fresh authenticator or recovery code, if enabled. This does not repeat the action.</p><p id="reauthentication-error" role="alert" tabindex="-1"></p><label for="reauthentication-password">Current password</label><input id="reauthentication-password" type="password" autocomplete="current-password" required maxlength="64"><label for="reauthentication-code">Authenticator or recovery code (if enabled)</label><input id="reauthentication-code" autocomplete="one-time-code" maxlength="32"><div class="modal-actions"><button type="button" data-cancel>Cancel</button><button class="primary-action" type="submit">Confirm identity</button></div></form>';
            document.body.append(dialog);
            const node = dialog, form = node.querySelector('form');
            node.querySelector('[data-cancel]').onclick = () => close();
            node.addEventListener('cancel', e => { e.preventDefault(); close(); });
            form.onsubmit = async e => {
                e.preventDefault();
                if (form.getAttribute('aria-busy') === 'true') return;
                const password = node.querySelector('#reauthentication-password'), code = node.querySelector('#reauthentication-code');
                const body = JSON.stringify({ password: password.value, code: code.value });
                form.reset(); form.setAttribute('aria-busy', 'true');
                node.querySelector('[type=submit]').disabled = true;
                try {
                    await request('/api/auth/reauthenticate', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body });
                    if (dialog === node) close(true);
                } catch (error) {
                    if (dialog === node && error.name !== 'AbortError') {
                        const region = node.querySelector('[role=alert]'); region.textContent = error.message; region.focus();
                    }
                } finally {
                    if (dialog === node) { form.setAttribute('aria-busy', 'false'); node.querySelector('[type=submit]').disabled = false; }
                }
            };
            const result = new Promise(resolve => { settle = resolve; });
            node.showModal(); node.querySelector('input').focus();
            const confirmed = await result;
            if (current() && previousFocus?.isConnected) previousFocus.focus();
            return current() && confirmed;
        }
        async function sensitive(path, options, operation) {
            try { return await request(path, options); }
            catch (error) {
                if (!current()) throw new DOMException('Page ended', 'AbortError');
                if (!error.reauthenticationRequired) throw error;
                if (!(await confirmIdentity()) || !current()) throw new DOMException('Confirmation cancelled', 'AbortError');
                let confirmed;
                resumePending = true;
                try {
                    confirmed = await root.confirmAction('Identity confirmed. Review and explicitly resume this action once.', { title: 'Resume sensitive action', operation, confirmLabel: 'Resume action' });
                } finally { resumePending = false; }
                if (!confirmed || !current()) throw new DOMException('Action cancelled', 'AbortError');
                // One explicit resubmission; a second expiry is returned, never looped.
                return request(path, options);
            }
        }
        return Object.freeze({ request, sensitive, confirmIdentity });
    }
    root.Reauthentication = Object.freeze({ create });
})(window);
