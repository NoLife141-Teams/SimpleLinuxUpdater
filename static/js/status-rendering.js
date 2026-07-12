(function initStatusRendering(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.StatusRendering = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function statusRenderingFactory() {
    "use strict";

    function create(documentRef) {
        function text(id, value) {
            const element = documentRef.getElementById(id);
            if (element) element.textContent = String(value ?? "");
        }

        function html(element, markup) {
            if (element) element.innerHTML = String(markup ?? "");
        }

        function visible(element, show) {
            if (element) element.classList.toggle("hidden", !show);
        }

        function metricFilterLabel(key) {
            switch (key) {
            case "pending_approval": return "Show hosts waiting for approval";
            case "active": return "Show hosts with active maintenance phases";
            case "stale_facts": return "Show hosts with stale facts";
            case "high_risk": return "Show hosts with high risk CVE exposure";
            default: return "Show all hosts";
            }
        }

        function metricFilterState(quickFilter) {
            documentRef.querySelectorAll("[data-metric-filter]").forEach(button => {
                const key = button.dataset.metricFilter || "";
                const active = quickFilter === key;
                const label = metricFilterLabel(key);
                button.classList.toggle("active", active);
                button.closest(".metric-item")?.classList.toggle("active", active);
                button.setAttribute("aria-pressed", active ? "true" : "false");
                button.setAttribute("aria-label", label);
                button.title = label;
            });
        }

        function metrics(presentation, quickFilter) {
            const fleet = presentation.fleet;
            const auth = presentation.auth;
            const values = {
                "metric-total-hosts": fleet.total,
                "metric-total-note": fleet.total === 0 ? "No servers loaded" : `${fleet.total} ${fleet.total === 1 ? "host" : "hosts"} monitored`,
                "metric-reachable-hosts": fleet.reachable,
                "metric-pending-approvals": fleet.pendingApproval,
                "metric-prechecks": fleet.active,
                "metric-active-runs": fleet.active,
                "metric-done-hosts": fleet.done,
                "metric-failed-hosts": fleet.failed,
                "metric-pending-packages": fleet.pendingPackages,
                "metric-security-updates": fleet.securityUpdates,
                "metric-stale-facts": fleet.staleFacts,
                "metric-high-risk-cve": fleet.highRiskCVE,
                "metric-auth-posture": auth.label,
                "metric-auth-note": `${auth.withServerKey} server key · ${auth.withGlobalKey} global SSH key · ${auth.withPassword} password · ${auth.missing} missing`
            };
            Object.entries(values).forEach(([id, value]) => text(id, value));
            metricFilterState(quickFilter);
        }

        function syncState({ degraded, live, lastSyncText }) {
            const polling = documentRef.getElementById("polling-state-label");
            const lastSync = documentRef.getElementById("last-sync-label");
            if (polling) {
                polling.textContent = degraded ? "Polling degraded" : (live ? "Live events" : "Live polling");
                polling.classList.toggle("warning", degraded);
                polling.classList.toggle("live", !degraded);
            }
            if (lastSync) {
                lastSync.textContent = lastSyncText;
                lastSync.classList.toggle("warning", degraded);
            }
        }

        return Object.freeze({ text, html, visible, metrics, metricFilterLabel, metricFilterState, syncState });
    }

    return Object.freeze({ create });
}));
