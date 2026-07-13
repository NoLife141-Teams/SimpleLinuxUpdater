(function initManagePolicyOverrideAdapter(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.ManagePolicyOverrideAdapter = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function managePolicyOverrideAdapterFactory() {
    "use strict";

    function createAdapter({ store, requestJSON, escapeHTML }) {
        function activeEditor() {
            return store.getView().editor;
        }

        async function fetchContext(serverName) {
            const editor = activeEditor();
            if (!editor.open || editor.originalName !== String(serverName || "")) return;
            const request = store.dispatch({ type: "snapshotRequested", stream: "policyContext" })
                .find(effect => effect.type === "fetchSnapshot");
            if (!request) return;
            const sessionID = editor.sessionID;
            try {
                const policiesData = await requestJSON("/api/update-policies", {}, "Failed to load scheduled policies.");
                const policies = Array.isArray(policiesData.items) ? policiesData.items : [];
                const entries = await Promise.all(policies.map(async policy => {
                    const data = await requestJSON(
                        `/api/update-policies/${encodeURIComponent(policy.id)}/overrides`,
                        {},
                        "Failed to load policy overrides."
                    );
                    const match = Array.isArray(data.items)
                        ? data.items.find(item => String(item.server_name || "") === String(serverName || ""))
                        : null;
                    return [String(policy.id), !!match?.disabled];
                }));
                store.dispatch({
                    type: "policyContextReceived",
                    requestID: request.requestID,
                    sessionID,
                    context: { policies, overrides: Object.fromEntries(entries) }
                });
            } catch (error) {
                store.dispatch({
                    type: "snapshotFailed",
                    stream: "policyContext",
                    requestID: request.requestID,
                    error: error?.message
                });
                throw error;
            }
        }

        function render(container) {
            if (!container) return;
            const context = activeEditor().policyContext;
            const policies = context.visiblePolicies || [];
            if (!policies.length) {
                container.innerHTML = '<div class="subtle">No tag-based scheduled policies currently match this server.</div>';
                return;
            }
            container.innerHTML = policies.map(policy => {
                const policyID = String(policy.id);
                const checked = context.overrides[policyID] ? "checked" : "";
                const cadence = policy.cadence_kind === "weekly"
                    ? `${(policy.weekdays || []).join(", ") || "weekly"} @ ${policy.time_local || "--:--"}`
                    : `daily @ ${policy.time_local || "--:--"}`;
                return `
                    <div class="policy-override-item">
                        <label class="checkbox-inline">
                            <input type="checkbox" data-policy-id="${escapeHTML(policyID)}" ${checked}>
                            Disable "${escapeHTML(policy.name || "")}" for this server
                        </label>
                        <p class="subtle">${escapeHTML(policy.execution_mode || "")} / ${escapeHTML(policy.package_scope || "")} / ${escapeHTML(cadence)}</p>
                    </div>
                `;
            }).join("");
        }

        function change(policyID, disabled) {
            store.dispatch({ type: "policyOverrideChanged", policyID, disabled });
        }

        async function save(serverName, changes) {
            const editor = activeEditor();
            const sessionID = editor.sessionID;
            const results = await Promise.all((changes || []).map(async change => {
                try {
                    await requestJSON(
                        `/api/update-policies/${encodeURIComponent(change.policyID)}/overrides/${encodeURIComponent(serverName)}`,
                        {
                            method: "PUT",
                            headers: { "Content-Type": "application/json" },
                            body: JSON.stringify({ disabled: !!change.disabled })
                        },
                        `Failed to save scheduled update override for policy ${change.policyID}.`
                    );
                    return { ...change, ok: true };
                } catch (error) {
                    return { ...change, ok: false, error: error?.message || "unknown error" };
                }
            }));
            store.dispatch({ type: "policyOverrideBatchCompleted", sessionID, results });
            return store.getView().editor.policyContext.outcome;
        }

        return Object.freeze({ fetchContext, render, change, save });
    }

    return Object.freeze({ createAdapter });
}));
