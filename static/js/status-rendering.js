(function initStatusRendering(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.StatusRendering = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function statusRenderingFactory() {
    "use strict";

    // Reuse a defensive domain snapshot only within one synchronous DOM render.
    // Other callers retain fresh reads; nested renders share the accepted facts.
    function createSnapshotScope(readSnapshot) {
        let active = false;
        let snapshot;
        function read() {
            return active ? snapshot : readSnapshot();
        }
        function run(render) {
            if (active) return render();
            snapshot = readSnapshot();
            active = true;
            try {
                return render();
            } finally {
                active = false;
                snapshot = undefined;
            }
        }
        return Object.freeze({ read, run });
    }

    const syncStreamLabels = Object.freeze({
        servers: "Fleet inventory",
        dashboard: "Dashboard",
        commandHistory: "Command history",
        audit: "Audit trail",
        observability: "Observability",
        policies: "Policies",
        globalKey: "SSH key status"
    });

    function joinLabels(labels) {
        if (labels.length < 2) return labels[0] || "";
        if (labels.length === 2) return `${labels[0]} and ${labels[1]}`;
        return `${labels.slice(0, -1).join(", ")}, and ${labels.at(-1)}`;
    }

    function syncNotice(streams, formatTimestamp = value => String(value || "not yet successful")) {
        const affected = Object.entries(streams || {})
            .filter(([, stream]) => stream && stream.lastError)
            .map(([name, stream]) => ({
                label: syncStreamLabels[name] || name,
                lastSuccessfulAt: String(stream.lastSuccessfulAt || "")
            }));
        if (affected.length === 0) return { visible: false, title: "", summary: "", lastSuccessText: "" };
        const labels = affected.map(source => source.label);
        const refreshes = affected.map(source => (
            `${source.label} ${formatTimestamp(source.lastSuccessfulAt, "not yet successful")}`
        ));
        return {
            visible: true,
            title: "Data freshness warning",
            summary: `Affected data: ${joinLabels(labels)}. Visible maintenance data may be out of date.`,
            lastSuccessText: `Last successful refresh: ${refreshes.join(" · ")}`
        };
    }

    // Keys describe the action target, never its changing label or enabled state.
    function nodeKey(node) {
        if (node.nodeType !== 1) return "";
        const attributes = ["data-render-key", "id", "data-action", "data-pending-action", "data-select-host", "data-select-server"];
        for (const name of attributes) {
            if (node.hasAttribute(name)) {
                return JSON.stringify([node.localName, name, node.getAttribute(name),
                    node.getAttribute("data-name"), node.getAttribute("data-tab"), node.getAttribute("data-pending-value")]);
            }
        }
        return "";
    }

    function patchNode(current, next) {
        if (current.nodeType !== 1) {
            if (current.nodeValue !== next.nodeValue) current.nodeValue = next.nodeValue;
            return;
        }
        for (const attribute of Array.from(current.attributes)) {
            if (!next.hasAttribute(attribute.name)) current.removeAttribute(attribute.name);
        }
        for (const attribute of Array.from(next.attributes)) {
            if (current.getAttribute(attribute.name) !== attribute.value) current.setAttribute(attribute.name, attribute.value);
        }
        if (current.localName === "input") {
            if (current.value !== next.value) current.value = next.value;
            if (current.checked !== next.checked) current.checked = next.checked;
        }
        patchChildren(current, next);
    }

    function patchChildren(parent, nextParent) {
        const remaining = new Set(parent.childNodes);
        const pools = new Map();
        const identity = node => JSON.stringify([node.nodeType, node.nodeName, node.namespaceURI, nodeKey(node)]);
        for (const node of remaining) {
            const key = identity(node);
            if (!pools.has(key)) pools.set(key, { nodes: [], index: 0 });
            pools.get(key).nodes.push(node);
        }
        const pairs = Array.from(nextParent.childNodes, next => {
            const pool = pools.get(identity(next));
            const current = pool && pool.index < pool.nodes.length ? pool.nodes[pool.index++] : next;
            remaining.delete(current);
            return { current, next };
        });
        // Remove obsolete siblings first so an unchanged button need not move
        // just because an action before it disappeared.
        for (const node of remaining) node.remove();
        let cursor = parent.firstChild;
        for (const { current, next } of pairs) {
            if (current !== cursor) {
                if (current.parentNode === parent && parent.isConnected && parent.moveBefore) parent.moveBefore(current, cursor);
                else parent.insertBefore(current, cursor);
            }
            if (current !== next) patchNode(current, next);
            cursor = current.nextSibling;
        }
    }

    function create(documentRef) {
        function text(id, value) {
            const element = documentRef.getElementById(id);
            if (element) element.textContent = String(value ?? "");
        }

        function html(element, markup) {
            if (!element) return;
            const template = documentRef.createElement("template");
            template.innerHTML = String(markup ?? "");
            patchChildren(element, template.content);
        }

        function visible(element, show) {
            if (element) element.classList.toggle("hidden", !show);
        }

        function metricFilterLabel(key) {
            switch (key) {
            case "pending_approval": return "Show hosts waiting for approval";
            case "active": return "Show hosts with active maintenance phases";
            case "stale_facts": return "Show hosts whose facts need refresh";
            case "high_risk": return "Show hosts with CVEs linked to available updates";
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

        function syncState({ degraded, live, lastSyncText, notice = {} }) {
            const polling = documentRef.getElementById("polling-state-label");
            const lastSync = documentRef.getElementById("last-sync-label");
            const degradedNotice = documentRef.getElementById("sync-degraded-notice");
            if (polling) {
                polling.textContent = degraded ? "Polling degraded" : (live ? "Live events" : "Live polling");
                polling.classList.toggle("warning", degraded);
                polling.classList.toggle("live", !degraded);
            }
            if (lastSync) {
                lastSync.textContent = lastSyncText;
                lastSync.classList.toggle("warning", degraded);
            }
            if (degradedNotice) {
                degradedNotice.classList.toggle("hidden", !notice.visible);
                degradedNotice.setAttribute("aria-hidden", notice.visible ? "false" : "true");
            }
            text("sync-degraded-title", notice.title || "");
            text("sync-degraded-summary", notice.summary || "");
            text("sync-degraded-last-success", notice.lastSuccessText || "");
        }

        return Object.freeze({ text, html, patchChildren, visible, metrics, metricFilterLabel, metricFilterState, syncNotice, syncState });
    }

    return Object.freeze({ create, syncNotice, createSnapshotScope });
}));
