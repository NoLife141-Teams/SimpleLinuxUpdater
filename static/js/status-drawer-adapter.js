(function initStatusDrawerAdapter(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.StatusDrawerAdapter = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function statusDrawerAdapterFactory() {
    "use strict";

    function create(documentRef, windowRef) {
        let previousFocus = null;

        function focusable(drawer) {
            return Array.from(drawer.querySelectorAll(
                'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
            )).filter(element => !element.classList.contains("hidden") && element.getAttribute("aria-hidden") !== "true");
        }

        function open(drawer, backdrop) {
            if (!drawer || !backdrop) return;
            previousFocus = documentRef.activeElement;
            documentRef.body.classList.add("drawer-open");
            drawer.classList.add("open");
            backdrop.classList.add("open");
            drawer.setAttribute("aria-hidden", "false");
            windowRef.setTimeout(() => {
                const target = focusable(drawer)[0] || drawer;
                target?.focus?.({ preventScroll: true });
            }, 0);
        }

        function close(drawer, backdrop) {
            if (!drawer || !backdrop) return;
            drawer.classList.remove("open");
            backdrop.classList.remove("open");
            drawer.setAttribute("aria-hidden", "true");
            documentRef.body.classList.remove("drawer-open");
            const target = previousFocus;
            previousFocus = null;
            if (target && documentRef.contains(target) && typeof target.focus === "function") {
                windowRef.setTimeout(() => target.focus({ preventScroll: true }), 0);
            }
        }

        return Object.freeze({ open, close, focusable });
    }

    return Object.freeze({ create });
}));
