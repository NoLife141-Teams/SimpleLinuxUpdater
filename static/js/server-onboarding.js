window.ServerOnboarding = Object.freeze({
    mount(options) {
        const wizard = window.ServerOnboardingInteraction.createStore();
        const form = document.getElementById('add-server-form');
        const next = document.getElementById('onboarding-next');
        const back = document.getElementById('onboarding-back');
        const reset = document.getElementById('onboarding-reset');
        const retry = document.getElementById('onboarding-retry');
        const trust = document.getElementById('trust-host-key');
        const error = document.getElementById('add-server-error');
        let draft = null;

        function render(focus = false) {
            const view = wizard.getView();
            form.setAttribute('aria-busy', String(view.busy));
            document.querySelectorAll('[data-onboarding-panel]').forEach(panel => {
                panel.hidden = Number(panel.dataset.onboardingPanel) !== view.step;
            });
            document.querySelectorAll('[data-onboarding-step]').forEach(step => {
                if (Number(step.dataset.onboardingStep) === view.step) step.setAttribute('aria-current', 'step');
                else step.removeAttribute('aria-current');
            });
            document.getElementById('onboarding-connection').disabled = view.busy;
            document.getElementById('onboarding-status').textContent = view.busy
                ? (view.step === 1 ? 'Reading the SSH identity…' : view.step === 4 ? 'Verifying again and saving the server…' : 'Checking SSH and maintenance prerequisites…')
                : view.step === 5 ? 'Server added. No update has been started.' : `Step ${view.step} of 4`;
            trust.checked = view.trusted;
            trust.disabled = view.busy;
            document.getElementById('onboarding-fingerprint').textContent = view.scan?.fingerprint_sha256 || '';
            document.getElementById('onboarding-identity-target').textContent = draft ? `${draft.host}:${draft.port}` : '';
            document.getElementById('onboarding-identity-note').textContent = view.scan?.already_trusted
                ? 'This fingerprint matches the saved SSH identity. Confirm it before continuing.'
                : 'Compare this fingerprint with a trusted source, such as the server console. Reading it over the network alone does not establish trust.';
            next.textContent = view.busy ? 'Please wait…' : ({ 1: 'Continue', 2: 'Run checks', 3: 'Continue', 4: 'Add Server' }[view.step] || 'Add Server');
            next.hidden = view.step === 5;
            next.disabled = view.busy || (view.step === 2 && !view.trusted) || (view.step === 3 && !view.report?.ready);
            back.hidden = view.step === 1 || view.step === 5;
            back.textContent = view.step === 4 ? 'Back to checks' : 'Edit connection';
            back.disabled = view.busy;
            reset.hidden = view.step !== 5;
            retry.hidden = view.step !== 3 || view.busy || !view.report;
            retry.disabled = view.busy;
            if (view.step === 3) renderChecks(view.report);
            if (view.step === 4 && draft) {
                document.getElementById('onboarding-summary').textContent = `${draft.name} · ${draft.host}:${draft.port} · ${draft.user}\n${view.report.distribution || ''} · ${draft.auth_method}\nTags: ${draft.tags.join(', ') || 'none'}`;
            }
            document.getElementById('onboarding-saved-name').textContent = view.savedName;
            if (focus && !view.busy) form.querySelector('[data-onboarding-panel]:not([hidden]) h3')?.focus();
        }

        function renderChecks(report) {
            const list = document.getElementById('onboarding-checks');
            list.replaceChildren();
            const labels = { ssh: 'SSH connection and identity', distribution: 'Linux distribution', apt: 'APT tools', sudo: 'Maintenance permissions', disk: 'Free disk space', packages: 'Package state' };
            for (const check of report?.checks || []) {
                const row = document.createElement('li');
                row.className = `onboarding-check is-${check.status === 'passed' ? 'passed' : check.status === 'warning' ? 'warning' : 'failed'}`;
                const title = document.createElement('strong');
                title.textContent = `${check.status === 'passed' ? '✓' : check.status === 'warning' ? '!' : '✕'} ${labels[check.id] || check.id}`;
                const detail = document.createElement('p');
                detail.textContent = check.message;
                row.append(title, detail);
                if (check.remediation) {
                    const hint = document.createElement('p');
                    hint.className = 'muted';
                    hint.textContent = check.remediation;
                    row.append(hint);
                }
                list.append(row);
            }
            document.getElementById('onboarding-check-result').textContent = !report ? 'Checks are running…' : report.ready ? 'Prerequisites verified. Review and confirm to save this server.' : 'Correct the failed checks, then retry. The server has not been saved.';
        }

        function details() {
            const creation = options.store.getView().creation;
            const keyFile = document.getElementById('key_file').files?.[0];
            const pass = document.getElementById('pass').value;
            const payload = {
                name: document.getElementById('name').value,
                host: document.getElementById('host').value,
                port: document.getElementById('port').value,
                user: document.getElementById('user').value,
                tags: document.getElementById('tags').value.split(',').map(tag => tag.trim()).filter(Boolean),
                hasPassword: !!pass,
                hasKeyFile: !!keyFile,
                trustHostKey: true
            };
            const plan = options.store.dispatch({ type: 'creationValidationRequested', payload })[0];
            if (!plan.enabled) { options.showValidation(plan); return null; }
            if (creation.authenticationMethod === 'per-server-key' && keyFile?.size > 64 * 1024) { options.showValidation({ reason: 'SSH key files must be at most 64 KiB.', invalidFields: ['key'] }); return null; }
            return { payload, keyFile, draft: { ...plan.payload, auth_method: creation.authenticationMethod, pass: creation.authenticationMethod === 'password' ? pass : '', key: '' } };
        }

        async function request(url, body) {
            const response = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
            const result = await response.json().catch(() => ({}));
            if (!response.ok) {
                const failure = new Error(result.error || 'Server verification failed. Retry shortly.');
                failure.report = result.report;
                throw failure;
            }
            return result;
        }

        async function scan() {
            const values = details();
            if (!values) return;
            const pending = wizard.begin('scan');
            if (!pending) return;
            options.clearValidation();
            render();
            try {
                draft = values.draft;
                if (draft.auth_method === 'per-server-key') draft.key = await values.keyFile.text();
                const scanned = await request('/api/hostkeys/scan', { host: draft.host, port: draft.port });
                if (scanned.host_entry_exists && !scanned.already_trusted) throw new Error('The SSH identity differs from the saved key. Review the existing host trust before adding this server.');
                if (!scanned.fingerprint_sha256) throw new Error('No SSH fingerprint was returned. Retry the scan.');
                wizard.finish(pending, scanned);
            } catch (failure) {
                wizard.finish(pending, null, true);
                error.textContent = failure.message;
            }
            render(true);
        }

        function verifiedDraft() {
            return { ...draft, fingerprint_sha256: wizard.getView().scan.fingerprint_sha256, confirmed: wizard.getView().trusted };
        }

        async function check() {
            const pending = wizard.begin('check');
            if (!pending) return;
            error.textContent = '';
            render();
            try { wizard.finish(pending, await request('/api/servers/onboarding/check', verifiedDraft())); }
            catch (failure) { wizard.finish(pending, { ready: false, checks: [] }); error.textContent = failure.message; }
            render(true);
        }

        async function save() {
            const values = details();
            if (!values) { wizard.reset(); render(); return; }
            const command = options.store.dispatch({ type: 'commandRequested', command: 'createServer', payload: values.payload });
            const execution = command.find(effect => effect.type === 'executeCommand');
            if (!execution) { options.showValidation(command.find(effect => effect.type === 'commandRejected')); return; }
            const pending = wizard.begin('create');
            if (!pending) { await options.settle('commandFailed', execution.plan, 'Creation is already in progress.', { announce: false }); return; }
            error.textContent = '';
            render();
            try {
                const saved = await request('/api/servers/onboarding', verifiedDraft());
                wizard.finish(pending, saved);
                draft = null;
                form.reset();
                options.onSaved();
                await options.settle('commandCompleted', execution.plan, 'Server verified and added. No update was started.', { announce: false });
            } catch (failure) {
                wizard.finish(pending, null, true);
                if (failure.report) { wizard.back(); const failed = wizard.begin('check'); wizard.finish(failed, failure.report); }
                await options.settle('commandFailed', execution.plan, failure.message, { announce: false });
                error.textContent = failure.message;
            }
            render(true);
        }

        form.addEventListener('submit', event => {
            event.preventDefault();
            const view = wizard.getView();
            if (view.busy) return;
            if (view.step === 1) scan();
            else if (view.step === 2) check();
            else if (view.step === 3) { wizard.next(); render(true); }
            else if (view.step === 4) save();
        });
        trust.addEventListener('change', () => { wizard.trust(trust.checked); render(); });
        document.getElementById('onboarding-connection').addEventListener('input', () => {
            if (!wizard.getView().busy && wizard.getView().step !== 1) { wizard.reset(); draft = null; render(); }
        });
        back.addEventListener('click', () => { if (wizard.back()) { if (wizard.getView().step === 1) draft = null; error.textContent = ''; render(true); } });
        retry.addEventListener('click', check);
        reset.addEventListener('click', () => { if (wizard.reset()) { draft = null; error.textContent = ''; render(true); document.getElementById('name').focus(); } });
        render();
    }
});
