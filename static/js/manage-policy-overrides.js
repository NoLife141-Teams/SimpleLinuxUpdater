// Manage page scheduled-policy override helpers. Loaded before manage.js.
            function currentEditingServerName() {
                const editor = window.managePageInteraction?.getView().editor;
                return editor?.open ? String(editor.originalName || '') : '';
            }

            function parseTagsInput(raw) {
                return String(raw || '')
                    .split(',')
                    .map((tag) => tag.trim())
                    .filter(Boolean);
            }

            function serverMatchesPolicyTags(tags, policy) {
                const targetTag = String(policy?.target_tag || '').trim().toLowerCase();
                const includeTags = Array.isArray(policy?.include_tags) ? policy.include_tags : [];
                const excludeTags = Array.isArray(policy?.exclude_tags) ? policy.exclude_tags : [];
                const targetServers = Array.isArray(policy?.target_servers) ? policy.target_servers : [];
                const loweredTags = tags.map((tag) => String(tag || '').trim().toLowerCase()).filter(Boolean);
                if (excludeTags.some((tag) => loweredTags.includes(String(tag || '').trim().toLowerCase()))) return false;
                const editingServerName = currentEditingServerName();
                const editingServerKey = editingServerName.trim().toLowerCase();
                if (editingServerKey && targetServers.some((name) => String(name || '').trim().toLowerCase() === editingServerKey)) return true;
                if (targetTag && loweredTags.includes(targetTag)) return true;
                if (includeTags.some((tag) => loweredTags.includes(String(tag || '').trim().toLowerCase()))) return true;
                const hasTargetFields = !!targetTag || includeTags.length > 0 || targetServers.length > 0;
                return !hasTargetFields && editingServerName && Array.isArray(policy?.matched_servers)
                    ? policy.matched_servers.some((name) => String(name || '') === editingServerName)
                    : false;
            }

            async function fetchEditPolicyContext(serverName) {
                const requestedServerName = String(serverName || '');
                const policiesRes = await fetch('/api/update-policies');
                if (!policiesRes.ok) {
                    throw new Error(await parseErrorResponse(policiesRes, 'Failed to load scheduled policies.'));
                }
                if (currentEditingServerName() !== requestedServerName) {
                    return;
                }
                const policiesData = await policiesRes.json().catch(() => ({}));
                const nextPolicies = Array.isArray(policiesData.items) ? policiesData.items : [];
                const nextOverrideStates = new Map();
                await Promise.all(nextPolicies.map(async (policy) => {
                    const res = await fetch(`/api/update-policies/${encodeURIComponent(policy.id)}/overrides`);
                    if (!res.ok) {
                        throw new Error(await parseErrorResponse(res, 'Failed to load policy overrides.'));
                    }
                    if (currentEditingServerName() !== requestedServerName) {
                        return;
                    }
                    const data = await res.json().catch(() => ({}));
                    if (currentEditingServerName() !== requestedServerName) {
                        return;
                    }
                    const match = Array.isArray(data.items)
                        ? data.items.find((item) => String(item.server_name || '') === requestedServerName)
                        : null;
                    nextOverrideStates.set(String(policy.id), !!match?.disabled);
                }));
                if (currentEditingServerName() !== requestedServerName) {
                    return;
                }
                editUpdatePolicies = nextPolicies;
                editPolicyOverrideStates = nextOverrideStates;
                if (window.managePageInteraction) {
                    window.managePageInteraction.dispatch({
                        type: 'policyContextReceived',
                        sessionID: window.managePageInteraction.getView().editor.sessionID,
                        context: { policies: nextPolicies, overrides: Object.fromEntries(nextOverrideStates) }
                    });
                }
            }

            function renderEditPolicyOverrides() {
                const container = document.getElementById('edit-policy-overrides');
                if (!container) return;
                const currentTags = parseTagsInput(document.getElementById('edit-tags').value);
                const matchingPolicies = editUpdatePolicies.filter((policy) => serverMatchesPolicyTags(currentTags, policy));
                if (!matchingPolicies.length) {
                    container.innerHTML = '<div class="subtle">No tag-based scheduled policies currently match this server.</div>';
                    return;
                }
                container.innerHTML = matchingPolicies.map((policy) => {
                    const checked = editPolicyOverrideStates.get(String(policy.id)) ? 'checked' : '';
                    const cadence = policy.cadence_kind === 'weekly'
                        ? `${(policy.weekdays || []).join(', ') || 'weekly'} @ ${policy.time_local || '--:--'}`
                        : `daily @ ${policy.time_local || '--:--'}`;
                    return `
                        <div class="policy-override-item">
                            <label class="checkbox-inline">
                                <input type="checkbox" data-policy-id="${escapeHtml(String(policy.id))}" ${checked}>
                                Disable "${escapeHtml(policy.name || '')}" for this server
                            </label>
                            <p class="subtle">${escapeHtml(policy.execution_mode || '')} / ${escapeHtml(policy.package_scope || '')} / ${escapeHtml(cadence)}</p>
                        </div>
                    `;
                }).join('');
            }

            async function saveEditPolicyOverrides(serverName) {
                const container = document.getElementById('edit-policy-overrides');
                if (!container) return;
                const currentTags = parseTagsInput(document.getElementById('edit-tags').value);
                const checkboxes = new Map(
                    Array.from(container.querySelectorAll('input[data-policy-id]')).map((checkbox) => [
                        String(checkbox.dataset.policyId || '').trim(),
                        checkbox
                    ])
                );
                const requests = [];
                for (const policy of editUpdatePolicies) {
                    const policyID = String(policy?.id || '').trim();
                    if (!policyID) {
                        continue;
                    }
                    const matchesPolicy = serverMatchesPolicyTags(currentTags, policy);
                    const checkbox = checkboxes.get(policyID);
                    if (!matchesPolicy && !editPolicyOverrideStates.get(policyID)) {
                        continue;
                    }
                    const disabled = matchesPolicy
                        ? (checkbox ? !!checkbox.checked : !!editPolicyOverrideStates.get(policyID))
                        : false;
                    const request = (async () => {
                        const res = await fetch(`/api/update-policies/${encodeURIComponent(policyID)}/overrides/${encodeURIComponent(serverName)}`, {
                            method: 'PUT',
                            headers: { 'Content-Type': 'application/json' },
                            body: JSON.stringify({ disabled })
                        });
                        if (!res.ok) {
                            throw new Error(await parseErrorResponse(res, `Failed to save scheduled update override for policy ${policyID}.`));
                        }
                    })();
                    requests.push({ policyID, disabled, request });
                }
                if (!requests.length) {
                    return;
                }
                const settled = await Promise.allSettled(requests.map((item) => item.request));
                const failures = [];
                settled.forEach((result, index) => {
                    const req = requests[index];
                    if (result.status === 'fulfilled') {
                        editPolicyOverrideStates.set(req.policyID, req.disabled);
                        return;
                    }
                    const reason = result.reason instanceof Error
                        ? result.reason.message
                        : String(result.reason || 'unknown error');
                    failures.push(`${req.policyID}: ${reason}`);
                });
                if (failures.length) {
                    throw new Error(`Failed to save scheduled update overrides for policy IDs ${failures.join('; ')}`);
                }
            }
