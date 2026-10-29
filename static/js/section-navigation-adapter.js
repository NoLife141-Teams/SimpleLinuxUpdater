// Geometry, listeners and rendering effects only; the injected page interaction owns navigation.
(function (root) {
    "use strict";
    function create({ lifecycle, page, interaction, renderEffects }) {
        let frame = null;
        const attribute = `data-${page}-section`;
        const sections = Array.from(document.querySelectorAll(`[${attribute}]`));
        const nav = document.getElementById(`${page}-section-nav`);
        function publish(event) {
            const before = interaction.getView().workspace.activeSection;
            renderEffects(interaction.dispatch(event));
            const after = interaction.getView().workspace.activeSection;
            if (event.type === "sectionGeometryReceived" && before !== after) history.replaceState(history.state, "", `#${page}-section-${after}`);
        }
        function sync() {
            frame = null;
            publish({ type: "sectionGeometryReceived", geometry: {
                sections: sections.map(section => {
                    const rect = section.getBoundingClientRect();
                    return { id: section.getAttribute(attribute), top: rect.top, bottom: rect.bottom };
                }),
                anchorY: (nav?.getBoundingClientRect().bottom || 0) + 8,
                scrollY: window.scrollY,
                viewportHeight: window.innerHeight,
                documentHeight: document.documentElement.scrollHeight,
            } });
        }
        function schedule() {
            if (frame === null) frame = lifecycle.requestAnimationFrame(sync);
        }
        function interact(event) {
            const section = event.target.closest?.(`[${attribute}]`);
            if (section) {
                publish({ type: "sectionInteracted", sectionID: section.getAttribute(attribute) });
                schedule();
            }
        }
        function scrollIntent(event) {
            if (event.type === "wheel" && event.deltaX && !event.deltaY) return;
            if (event.type === "keydown" && (!["ArrowDown", "ArrowUp", "PageDown", "PageUp", "Home", "End", " "].includes(event.key)
                || event.target.closest?.("input, textarea, select, button, [contenteditable=true]"))) return;
            interaction.dispatch({ type: "sectionScrollIntent" });
            schedule();
        }
        function init() {
            lifecycle.listen(window, "scroll", schedule, { passive: true });
            lifecycle.listen(window, "resize", schedule);
            lifecycle.listen(window, "wheel", scrollIntent, { passive: true });
            lifecycle.listen(window, "touchmove", scrollIntent, { passive: true });
            lifecycle.listen(document, "keydown", scrollIntent);
            lifecycle.listen(document, "focusin", interact);
            lifecycle.listen(document, "pointerdown", interact);
            if (root.ResizeObserver) {
                const Observer = lifecycle.observer(root.ResizeObserver);
                const observer = new Observer(schedule);
                sections.forEach(section => observer.observe(section));
                if (nav) observer.observe(nav);
                observer.observe(document.body);
            }
            schedule();
        }
        return Object.freeze({ init, schedule });
    }
    function renderLink(link, workspace, sectionID) {
        const inArea = workspace.areaSections.includes(sectionID);
        link.classList.toggle("is-in-area", inArea);
        if (workspace.activeSection === sectionID) link.setAttribute("aria-current", "location");
        else link.removeAttribute("aria-current");
        let badge = link.querySelector(".section-area-badge");
        if (!badge) {
            badge = document.createElement("em");
            badge.className = "section-area-badge";
            badge.textContent = "In this area";
            link.append(badge);
        }
        badge.hidden = !inArea;
        if (workspace.activeSection === sectionID) {
            const track = link.parentElement;
            const bounds = link.getBoundingClientRect();
            const viewport = track.getBoundingClientRect();
            if (bounds.left < viewport.left) track.scrollLeft -= viewport.left - bounds.left;
            else if (bounds.right > viewport.right) track.scrollLeft += bounds.right - viewport.right;
        }
    }
    function clearLocation(page, sectionID) {
        const aliases = page === "manage" ? { audit: "#audit-trail", directory: "#server-directory" } : {};
        if (location.hash === `#${page}-section-${sectionID}` || location.hash === aliases[sectionID])
            history.replaceState(history.state, "", location.pathname + location.search);
    }
    root.SectionNavigationAdapter = Object.freeze({ create, renderLink, clearLocation });
})(globalThis);
