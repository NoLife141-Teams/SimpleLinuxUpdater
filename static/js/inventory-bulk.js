window.InventoryBulk = Object.freeze({
    mount({ store, refresh }) {
        const lifecycle = window.BrowserEffects.create();
        lifecycle.init();
        const area = document.getElementById("inventory-bulk");
        const get = id => document.getElementById(id);
        function text(tag, value) { const node = document.createElement(tag); node.textContent = value; return node; }
        function schedule(value) {
            if (!value) return "Unavailable";
            const next = value.NextRun || {};
            const noRun = value.NoRun || {};
            return [next.PolicyName, next.ScheduledForUTC, next.Summary, next.Reason, noRun.Active ? `${noRun.Scope} no-run window: ${noRun.Reason}` : ""].filter(Boolean).join(" · ") || "No eligible scheduled run";
        }
        function render() {
            const page = store.getView();
            const view = page.inventoryBulk;
            area.setAttribute("aria-busy", String(view.busy));
            get("bulk-selection-count").textContent = `${view.selected.length} selected (including any explicitly selected on other pages)`;
            get("bulk-selected-names").replaceChildren(...view.selected.map(name => text("li", name)));
            for (const id of ["bulk-add-tags", "bulk-remove-tags", "bulk-availability", "bulk-clear", "search", "tag-filter", "auth-filter", "group-by", "page-size"]) get(id).disabled = view.busy;
            get("clear-server-filters").disabled = view.busy || page.inventory.activeFilterCount === 0;
            get("bulk-preview-button").disabled = view.busy || !view.selected.length;
            get("bulk-review-button").disabled = view.busy || !view.preview?.can_confirm;
            get("bulk-confirm").disabled = view.busy;
            get("bulk-back").disabled = view.busy;
            get("bulk-feedback").textContent = view.busy ? (view.phase === "committing" ? "Applying reviewed changes…" : "Preparing preview…") : view.message;
            get("bulk-preview").hidden = !["preview", "result", "conflict"].includes(view.phase);
            get("bulk-preview-heading").textContent = view.phase === "result" ? "Applied inventory changes" : view.phase === "conflict" ? "Current values after conflict" : "Review bulk changes";
            get("bulk-review-button").hidden = view.phase !== "preview";
            get("bulk-confirmation").hidden = !["confirmation", "committing"].includes(view.phase);
            get("bulk-confirm-count").textContent = `${view.selected.length} explicit servers · batch ${view.preview?.batch_id || ""}`;
            get("bulk-notice").textContent = view.preview?.notice || "";
            const rows = (view.preview?.rows || []).map(row => {
                const tr = document.createElement("tr");
                const values = value => `${value?.disabled ? "Disabled" : "Enabled"} · tags: ${(value?.tags || []).join(", ") || "none"}`;
                tr.append(text("td", row.name), text("td", values(row.before)), text("td", values(row.after)), text("td", schedule(row.before_schedule)), text("td", schedule(row.after_schedule)), text("td", row.errors?.length ? row.errors.join(". ") : "Ready"));
                return tr;
            });
            get("bulk-preview-rows").replaceChildren(...rows);
            document.querySelectorAll("[data-bulk-name]").forEach(choice => { choice.disabled = view.busy; choice.checked = view.selected.includes(choice.dataset.bulkName); });
        }
        async function dispatch(event) {
            const lifetime = lifecycle.capture();
            const effects = store.dispatch(event);
            render();
            for (const effect of effects) {
                if (effect.type === "focusBulk") get(effect.id)?.focus();
                if (effect.type === "refresh") {
                    await refresh([effect]);
                    if (!lifecycle.isCurrent(lifetime)) return;
                }
                if (effect.type !== "bulkRequest") continue;
                try {
                    const response = await lifecycle.fetch(`/api/servers/bulk${effect.kind === "preview" ? "/preview" : ""}`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(effect.body) });
                    if (!lifecycle.isCurrent(lifetime)) return;
                    const result = await response.json();
                    if (!lifecycle.isCurrent(lifetime)) return;
                    if (effect.kind === "preview" && !response.ok) throw new Error(result.error || "Preview failed");
                    if (effect.kind === "commit" && !result.outcome) throw new Error("Commit response unavailable");
                    await dispatch({ type: effect.kind === "preview" ? "bulkPreviewReceived" : "bulkCommitReceived", generation: effect.generation, result });
                } catch (error) {
                    if (!lifecycle.isCurrent(lifetime)) return;
                    await dispatch({ type: "bulkRequestFailed", generation: effect.generation, uncertain: effect.kind === "commit", error: error.message });
                }
            }
        }
        lifecycle.listen(document.querySelector("#manage-servers-table tbody"), "change", event => {
            const choice = event.target.closest("[data-bulk-name]");
            if (choice) dispatch({ type: "bulkSelectionChanged", name: choice.dataset.bulkName, selected: choice.checked });
        });
        for (const [id, key] of [["bulk-add-tags", "addTags"], ["bulk-remove-tags", "removeTags"], ["bulk-availability", "availability"]]) lifecycle.listen(get(id), id === "bulk-availability" ? "change" : "input", event => dispatch({ type: "bulkDraftChanged", patch: { [key]: event.target.value } }));
        for (const [id, type] of [["bulk-clear", "bulkSelectionCleared"], ["bulk-preview-button", "bulkPreviewRequested"], ["bulk-review-button", "bulkConfirmationRequested"], ["bulk-confirm", "bulkCommitRequested"], ["bulk-back", "bulkBackRequested"]]) lifecycle.listen(get(id), "click", () => dispatch({ type }));
        render();
        return Object.freeze({ render, destroy: () => lifecycle.destroy() });
    }
});
