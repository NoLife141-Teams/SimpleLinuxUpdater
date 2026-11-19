(function initStatusActionAdapter(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.StatusActionAdapter = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function statusActionAdapterFactory() {
    "use strict";

    function approvalImpact(title, facts = {}) {
        const lines = [String(title || "Confirm operation")];
        const append = (label, values) => {
            if (Array.isArray(values) && values.length) lines.push(`${label}: ${values.join(", ")}`);
        };
        append("Packages", facts.packages);
        append("May install dependencies", facts.newPackages);
        append("May remove packages", facts.removedPackages);
        if (facts.note) lines.push(String(facts.note));
        return lines.join("\n\n");
    }

    function create(windowRef) {
        let confirmationPending = false;
        async function confirm(message, options) {
            // A late plan response must not replace the decision the operator is reading.
            if (confirmationPending) return false;
            confirmationPending = true;
            try {
                return await windowRef.confirmAction(message, options);
            } finally {
                confirmationPending = false;
            }
        }

        return Object.freeze({
            notify: (message, options) => windowRef.notifyApp(message, options),
            confirm,
            approvalImpact
        });
    }

    return Object.freeze({ create, approvalImpact });
}));
