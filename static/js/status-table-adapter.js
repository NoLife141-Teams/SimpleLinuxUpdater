(function initStatusTableAdapter(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.StatusTableAdapter = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function statusTableAdapterFactory() {
    "use strict";

    function create(storage, key) {
        function load() {
            try {
                const parsed = JSON.parse(storage.getItem(key) || "{}");
                return parsed && typeof parsed === "object" && !Array.isArray(parsed) ? parsed : {};
            } catch (_) {
                return {};
            }
        }

        function save(widths) {
            try {
                storage.setItem(key, JSON.stringify(widths || {}));
            } catch (_) {
                // Storage is an optional enhancement.
            }
        }

        function boundedWidth(value, minimum, maximum, fallback) {
            const parsed = Number(value);
            const safe = Number.isFinite(parsed) ? parsed : fallback;
            return Math.round(Math.min(maximum, Math.max(minimum, safe)));
        }

        return Object.freeze({ load, save, boundedWidth });
    }

    return Object.freeze({ create });
}));
