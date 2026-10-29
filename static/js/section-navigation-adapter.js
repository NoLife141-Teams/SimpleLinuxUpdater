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
                || event.defaultPrevented
                || event.target.closest?.("input, textarea, select, [contenteditable=true]")
                || (event.key === " " && event.target.closest?.("button")))) return;
            interaction.dispatch({ type: "sectionScrollIntent" });
            schedule();
        }
        function observe() {
            lifecycle.listen(window, "scroll", schedule, { passive: true });
            lifecycle.listen(window, "resize", schedule);
            lifecycle.listen(window, "wheel", scrollIntent, { passive: true });
            lifecycle.listen(window, "touchmove", scrollIntent, { passive: true });
            lifecycle.listen(document, "keydown", scrollIntent);
            if (root.ResizeObserver) {
                const Observer = lifecycle.observer(root.ResizeObserver);
                const observer = new Observer(schedule);
                sections.forEach(section => observer.observe(section));
                if (nav) observer.observe(nav);
                observer.observe(document.body);
            }
            schedule();
        }
        function init(initialSectionID = "", navigate = () => {}) {
            // Accept a card choice immediately, including while native fragment
            // focus is pending; geometry must retain that choice within a row.
            lifecycle.listen(document, "focusin", interact);
            lifecycle.listen(document, "pointerdown", interact);
            if (!initialSectionID) {
                observe();
                return;
            }
            // Native fragment navigation finishes at load. Its deferred focus
            // must not replay over navigation or input already chosen by a user.
            const initialLocation = location.href;
            let interrupted = false;
            let tracking = false;
            const track = () => {
                if (tracking) return;
                tracking = true;
                observe();
            };
            const interrupt = event => {
                interrupted = true;
                track();
                if (["wheel", "touchmove", "keydown"].includes(event.type)) scrollIntent(event);
            };
            const intents = ["pointerdown", "keydown", "wheel", "touchmove"];
            intents.forEach(type => lifecycle.listen(window, type, interrupt, { passive: true }));
            const start = () => {
                intents.forEach(type => lifecycle.unlisten(window, type, interrupt));
                if (!interrupted && location.href === initialLocation) navigate(initialSectionID);
                track();
            };
            if (document.readyState === "complete") lifecycle.requestAnimationFrame(start);
            else lifecycle.listen(window, "load", () => lifecycle.requestAnimationFrame(start));
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
