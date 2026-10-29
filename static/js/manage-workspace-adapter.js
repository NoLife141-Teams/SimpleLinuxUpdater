// Browser effects for ManageWorkspaceAdapter; accepted state and command plans stay in the injected interaction owner.
(function (root) {
    "use strict";
    function create(options) {
        const lifecycle = root.BrowserEffects.create();
        const { managePageInteraction, renderAddAuthMethod } = options;
        const navigation = root.SectionNavigationAdapter.create({
            lifecycle, page: "manage", interaction: managePageInteraction,
            renderEffects: runManageWorkspaceEffects,
        });

        function renderManageWorkspace() {
            const view = managePageInteraction.getView();
            const workspace = view.workspace;
            workspace.sections.forEach((section) => {
                const root = document.querySelector(
                    `[data-manage-section="${section.id}"]`,
                );
                const link = document.querySelector(
                    `[data-manage-section-link="${section.id}"]`,
                );
                const navSummary = document.querySelector(
                    `[data-manage-section-nav-summary="${section.id}"]`,
                );
                const summary = document.querySelector(
                    `[data-manage-section-summary="${section.id}"]`,
                );
                const toggle = document.querySelector(
                    `[data-manage-section-toggle="${section.id}"]`,
                );
                const content = document.querySelector(
                    `[data-manage-section-content="${section.id}"]`,
                );
                root?.classList.toggle("is-collapsed", section.collapsed);
                if (link) globalThis.SectionNavigationAdapter.renderLink(link, workspace, section.id);
                if (navSummary) navSummary.textContent = section.summary;
                if (summary) {
                    summary.textContent = section.summary;
                    summary.hidden = !section.collapsed;
                }
                if (toggle) {
                    toggle.setAttribute(
                        "aria-expanded",
                        String(!section.collapsed),
                    );
                    toggle.setAttribute(
                        "aria-label",
                        `${section.collapsed ? "Expand" : "Collapse"} ${section.label}`,
                    );
                    const label = toggle.querySelector(
                        "[data-manage-section-toggle-label]",
                    );
                    if (label)
                        label.textContent = section.collapsed
                            ? "Expand"
                            : "Collapse";
                }
                if (content) content.hidden = section.collapsed;
            });

            const fleet = view.inventory.summary;
            document.getElementById("manage-summary-total").textContent =
                String(fleet.total);
            document.getElementById("manage-summary-host-auth").textContent =
                String(fleet.password + fleet.serverKey);
            document.getElementById("manage-summary-global-auth").textContent =
                String(fleet.globalKey);
            document.getElementById("manage-summary-ambiguous").textContent =
                String(fleet.ambiguous);
            document.getElementById("manage-summary-missing-auth").textContent =
                String(fleet.missing);
            document.getElementById("manage-summary-unknown-auth").textContent =
                String(fleet.unknown);
            document.getElementById("manage-summary-host-trust").textContent =
                String(fleet.hostKeyAttention);

            renderAddAuthMethod();
        }

        function runManageWorkspaceEffects(effects) {
            for (const effect of effects || []) {
                if (effect.type === "render" && effect.area === "workspace")
                    renderManageWorkspace();
                if (effect.type === "clearSectionLocation")
                    globalThis.SectionNavigationAdapter.clearLocation("manage", effect.sectionID);
                if (effect.type === "focusSection") {
                    const section = document.querySelector(
                        `[data-manage-section="${effect.sectionID}"]`,
                    );
                    const heading = document.getElementById(
                        `manage-section-${effect.sectionID}-heading`,
                    );
                    section?.scrollIntoView({
                        behavior: "auto",
                        block: "start",
                    });
                    heading?.focus({ preventScroll: true });
                }
            }
        }

        function manageSectionIDFromHash() {
            const prefix = "#manage-section-";
            if (window.location.hash.startsWith(prefix))
                return window.location.hash.slice(prefix.length);
            if (window.location.hash === "#audit-trail") return "audit";
            if (window.location.hash === "#server-directory")
                return "directory";
            return "";
        }

        function navigateManageSection(sectionID, updateHistory = false) {
            const effects = managePageInteraction.dispatch({
                type: "sectionNavigationRequested",
                sectionID,
            });
            if (!effects.length) return;
            if (updateHistory)
                history.pushState(history.state, "", `#manage-section-${sectionID}`);
            runManageWorkspaceEffects(effects);
            navigation.schedule();
        }

        function initializeManageWorkspace() {
            lifecycle.listen(
                document.getElementById("add-server-action"),
                "click",
                () => {
                    navigateManageSection("add-server", true);
                },
            );
            lifecycle.listen(
                document.getElementById("manage-section-nav"),
                "click",
                (event) => {
                    const link = event.target.closest(
                        "[data-manage-section-link]",
                    );
                    if (!link) return;
                    event.preventDefault();
                    navigateManageSection(link.dataset.manageSectionLink, true);
                },
            );
            document
                .querySelectorAll("[data-manage-section-toggle]")
                .forEach((toggle) => {
                    lifecycle.listen(toggle, "click", () => {
                        runManageWorkspaceEffects(
                            managePageInteraction.dispatch({
                                type: "sectionCollapseToggled",
                                sectionID: toggle.dataset.manageSectionToggle,
                            }),
                        );
                        navigation.schedule();
                    });
                });
            lifecycle.listen(window, "hashchange", () => {
                const sectionID = manageSectionIDFromHash();
                if (sectionID) navigateManageSection(sectionID);
            });
            const deepLink = manageSectionIDFromHash();
            renderManageWorkspace();
            navigation.init(deepLink, navigateManageSection);
        }
        return Object.freeze({
            init: lifecycle.init,
            destroy: lifecycle.destroy,
            renderManageWorkspace: (...args) =>
                lifecycle.active ? renderManageWorkspace(...args) : undefined,
            initializeManageWorkspace: (...args) =>
                lifecycle.active
                    ? initializeManageWorkspace(...args)
                    : undefined,
        });
    }
    root.ManageWorkspaceAdapter = Object.freeze({ create });
})(globalThis);
