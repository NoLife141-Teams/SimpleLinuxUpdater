// Transport, download and cancellation effects belong to this browser adapter.
window.OperationalExports = Object.freeze({
    mount({ store }) {
        const lifecycle = window.BrowserEffects.create();
        lifecycle.init();
        const get = id => document.getElementById(id);
        let controller = null;
        function render() {
            const view = store.getView();
            const scope = get("inventory-export-scope").value;
            const count = scope === "all" ? view.inventory.allItems.length : scope === "filtered" ? view.inventory.filteredNames.length : view.inventoryBulk.selected.length;
            get("inventory-export-count").textContent = scope ? `${count} hosts will be exported (${scope}).` : "Choose an export scope to review its count.";
            get("inventory-export-download").disabled = !scope || !!controller;
        }
        async function download(intent, feedback) {
            if (controller) return;
            const effect = store.dispatch(intent).find(effect => effect.type === "downloadExport");
            if (!effect) return;
            const lifetime = lifecycle.capture();
            controller = new AbortController();
            get("export-cancel").hidden = false;
            get("inventory-export-cancel").hidden = false;
            get(feedback).textContent = "Preparing export…";
            render();
            try {
                const response = await lifecycle.fetch(effect.path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(effect.body), signal: controller.signal });
                if (!response.ok) { const error = await response.json(); throw new Error(error.error || "Export failed"); }
                const blob = await response.blob();
                if (!lifecycle.isCurrent(lifetime)) return;
                const url = URL.createObjectURL(blob);
                try {
                    const link = document.createElement("a");
                    link.href = url; link.download = effect.filename;
                    document.body.appendChild(link); link.click(); link.remove();
                } finally { URL.revokeObjectURL(url); }
                get(feedback).textContent = `Export ready: ${response.headers.get("X-SLU-Export-Count") || "0"} ${intent.type === "inventoryExportRequested" ? "hosts" : "events"}.`;
            } catch (error) {
                if (lifecycle.isCurrent(lifetime)) get(feedback).textContent = error.name === "AbortError" ? "Export cancelled." : error.message;
            } finally {
                controller = null;
                if (lifecycle.isCurrent(lifetime)) { get("export-cancel").hidden = true; get("inventory-export-cancel").hidden = true; render(); }
            }
        }
        lifecycle.listen(get("inventory-export-scope"), "change", render);
        lifecycle.listen(document.querySelector("#manage-servers-table tbody"), "change", () => queueMicrotask(render));
        lifecycle.listen(get("bulk-clear"), "click", () => queueMicrotask(render));
        lifecycle.listen(get("inventory-export-download"), "click", () => download({ type: "inventoryExportRequested", scope: get("inventory-export-scope").value }, "inventory-export-feedback"));
        for (const format of ["json", "csv"]) lifecycle.listen(get(`audit-export-${format}`), "click", () => download({ type: "auditExportRequested", format }, "audit-export-feedback"));
        for (const id of ["export-cancel", "inventory-export-cancel"]) lifecycle.listen(get(id), "click", () => controller?.abort());
        lifecycle.own(() => controller?.abort());
        render();
        return Object.freeze({ render, destroy: () => lifecycle.destroy() });
    }
});
