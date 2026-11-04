// Shared shell disclosures; page interaction owners retain all operational state.
(function () {
    "use strict";
    if (window.__operatorWorkspaceReady) return;
    window.__operatorWorkspaceReady = true;
    document.documentElement.classList.add("workspace-enhanced");

    function closeMenu(returnFocus = false) {
        const header = document.querySelector(".app-header");
        const toggle = header?.querySelector(".app-nav-toggle");
        header?.classList.remove("is-menu-open");
        toggle?.setAttribute("aria-expanded", "false");
        if (returnFocus) toggle?.focus();
    }
    document.addEventListener("click", event => {
        const toggle = event.target.closest(".app-nav-toggle");
        if (toggle) {
            const header = toggle.closest(".app-header");
            const open = header.classList.toggle("is-menu-open");
            toggle.setAttribute("aria-expanded", String(open));
        } else if (event.target.closest(".app-nav a")) closeMenu();
        const sectionLink = event.target.closest("[data-admin-section-link]");
        if (sectionLink && window.matchMedia("(max-width: 1100px)").matches)
            sectionLink.closest("details")?.removeAttribute("open");
    });
    document.addEventListener("keydown", event => {
        if (event.key === "Escape" && document.querySelector(".app-header.is-menu-open")) {
            closeMenu(true);
            event.preventDefault();
        }
    });
    const mobileMenu = window.matchMedia("(max-width: 760px)");
    mobileMenu.addEventListener("change", () => closeMenu());
    document.querySelectorAll("[data-responsive-disclosure]").forEach(disclosure => {
        const width = disclosure.classList.contains("admin-section-picker") ? 1100 : 980;
        const media = window.matchMedia(`(max-width: ${width}px)`);
        const update = () => {
            if (media.matches && disclosure.contains(document.activeElement))
                disclosure.querySelector("summary")?.focus();
            disclosure.open = !media.matches;
        };
        update();
        media.addEventListener("change", update);
    });
})();
