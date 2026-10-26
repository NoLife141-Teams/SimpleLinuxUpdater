window.InventoryImport = Object.freeze({
    mount({ store, workspaceChanged, refresh, editServer }) {
        const file = document.getElementById('inventory-import-file');
        const previewButton = document.getElementById('import-preview-button');
        const previewPanel = document.getElementById('import-preview');
        const confirmation = document.getElementById('import-confirmation');
        const resultPanel = document.getElementById('import-result');
        const rows = document.getElementById('import-rows');
        let previewSignature = '';
        let createdSignature = '';

        function text(tag, value) {
            const node = document.createElement(tag);
            node.textContent = value;
            return node;
        }

        function render() {
            const view = store.getView().inventoryImport;
            document.getElementById('inventory-import').setAttribute('aria-busy', String(view.busy));
            file.disabled = view.phase === 'committing';
            if (!view.fileName) file.value = '';
            document.getElementById('import-file-name').textContent = view.fileName || 'No file selected';
            previewButton.disabled = !view.fileValid || view.busy || view.phase !== 'file';
            previewButton.textContent = view.busy && view.phase === 'file' ? 'Validating…' : 'Validate file';
            document.getElementById('import-reset').disabled = view.phase === 'committing';
            document.getElementById('import-feedback').textContent = view.error;
            previewPanel.hidden = view.phase !== 'preview';
            confirmation.hidden = !['confirmation', 'committing'].includes(view.phase);
            resultPanel.hidden = view.phase !== 'result';
            const entries = view.preview?.rows || [];
            const validCount = entries.filter(row => !row.errors?.length).length;
            document.getElementById('import-preview-summary').textContent = `${entries.length} rows · ${validCount} valid · ${entries.length - validCount} rejected · ${view.selected.length} selected`;
            document.getElementById('import-review-button').disabled = !view.selected.length || view.busy;
            document.getElementById('import-confirm-count').textContent = String(view.selected.length);
            document.getElementById('import-confirm-button').disabled = view.busy;
            document.getElementById('import-back-button').disabled = view.busy;
            document.getElementById('import-confirm-button').textContent = view.busy ? 'Importing…' : 'Import disabled servers';
            document.getElementById('import-result-count').textContent = String(view.created.length);
            const signature = JSON.stringify(entries);
            if (previewSignature !== signature) {
                previewSignature = signature;
                rows.replaceChildren();
                for (const row of entries) {
                    const tr = document.createElement('tr');
                    const choice = document.createElement('input');
                    choice.type = 'checkbox';
                    choice.dataset.importRow = String(row.row);
                    choice.setAttribute('aria-label', `Import row ${row.row}: ${row.name}`);
                    choice.disabled = !!row.errors?.length;
                    const cell = document.createElement('td');
                    cell.append(choice);
                    const endpoint = `${row.host.includes(':') ? `[${row.host}]` : row.host}:${row.port}`;
                    tr.append(cell, text('td', row.row), text('td', row.name), text('td', endpoint), text('td', row.user), text('td', row.tags.join(', ') || '—'));
                    const status = text('td', row.errors?.length ? row.errors.join('. ') : 'Valid · will be disabled');
                    status.className = row.errors?.length ? 'bad' : 'good';
                    tr.append(status);
                    rows.append(tr);
                }
            }
            rows.querySelectorAll('[data-import-row]').forEach(choice => {
                choice.checked = view.selected.includes(Number(choice.dataset.importRow));
            });
            const created = JSON.stringify(view.created);
            if (createdSignature !== created) {
                createdSignature = created;
                const list = document.getElementById('import-created');
                list.replaceChildren();
                view.created.forEach(name => {
                    const item = document.createElement('li');
                    const button = text('button', `Configure ${name}`);
                    button.type = 'button';
                    button.className = 'btn-ghost inline-btn';
                    button.addEventListener('click', () => editServer(name));
                    item.append(text('span', name), button);
                    list.append(item);
                });
            }
            workspaceChanged();
        }

        async function run(effects) {
            for (const effect of effects) {
                if (effect.type === 'render') render();
                if (effect.type === 'focusImport'
                    && store.getView().workspace.activeSection === 'import'
                    && !document.querySelector('.modal-backdrop.active')) {
                    document.getElementById(effect.id)?.focus();
                }
                if (effect.type === 'refresh') await refresh(effects);
                if (!['previewImport', 'commitImport'].includes(effect.type)) continue;
                try {
                    const preview = effect.type === 'previewImport';
                    const response = await fetch(preview ? `/api/servers/import/preview?format=${effect.format}` : '/api/servers/import', {
                        method: 'POST',
                        headers: { 'Content-Type': preview ? 'application/octet-stream' : 'application/json' },
                        body: preview ? file.files[0] : JSON.stringify(effect.selection)
                    });
                    const data = await response.json();
                    if (preview && !response.ok) throw new Error(data.error || 'File validation failed. Retry shortly.');
                    if (!preview && !data.preview) throw new Error(data.error || 'Import response unavailable. Refresh inventory before retrying.');
                    await run(store.dispatch(preview
                        ? { type: 'importPreviewReceived', generation: effect.generation, preview: data }
                        : { type: 'importCommitReceived', generation: effect.generation, result: data }));
                } catch (error) {
                    await run(store.dispatch({ type: 'importRequestFailed', generation: effect.generation, error: error.message }));
                }
            }
        }

        function dispatchIntent(event) {
            // A direct interaction owns the section even if viewport tracking
            // still points at a neighbor after scrolling or resizing.
            store.dispatch({ type: 'sectionActivated', sectionID: 'import' });
            return run(store.dispatch(event));
        }

        file.addEventListener('change', () => {
            const selected = file.files[0];
            void dispatchIntent({ type: 'importFileSelected', name: selected?.name, size: selected?.size });
        });
        previewButton.addEventListener('click', () => void dispatchIntent({ type: 'importPreviewRequested' }));
        rows.addEventListener('change', event => {
            const row = event.target.dataset.importRow;
            if (row) void dispatchIntent({ type: 'importRowSelectionChanged', row, selected: event.target.checked });
        });
        for (const [id, type] of Object.entries({ 'import-review-button': 'importConfirmationRequested', 'import-back-button': 'importSelectionRequested', 'import-confirm-button': 'importCommitRequested', 'import-reset': 'importReset' })) {
            document.getElementById(id).addEventListener('click', () => void dispatchIntent({ type }));
        }
        render();
    }
});
