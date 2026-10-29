// Shared, browser-independent section decisions. Page interaction owners retain this state.
(function (root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.SectionNavigation = api;
})(globalThis, function () {
    "use strict";
    const tolerance = 8;
    function create(activeSection) {
        return { activeSection, areaSections: [], preferredSection: activeSection, explicitSection: "" };
    }
    function select(state, sectionID) {
        return { ...state, activeSection: sectionID, preferredSection: sectionID, explicitSection: sectionID };
    }
    function reconcile(state, geometry) {
        const sections = (geometry.sections || []).filter(section =>
            Number.isFinite(section.top) && Number.isFinite(section.bottom) && section.bottom > section.top);
        const rows = [];
        for (const section of sections) {
            const row = rows.find(candidate => Math.abs(candidate.top - section.top) <= tolerance);
            if (row) row.ids.push(section.id);
            else rows.push({ top: section.top, ids: [section.id] });
        }
        rows.sort((a, b) => a.top - b.top);
        if (!rows.length) return state;
        const scrollable = geometry.documentHeight > geometry.viewportHeight + 1;
        let row;
        const explicit = sections.find(section => section.id === state.explicitSection
            && section.bottom > geometry.anchorY && section.top < geometry.viewportHeight);
        if (explicit) {
            row = rows.find(candidate => candidate.ids.includes(explicit.id));
        } else if (!scrollable) {
            row = rows.find(candidate => candidate.ids.includes(state.activeSection)) || rows[0];
        } else if (geometry.scrollY + geometry.viewportHeight >= geometry.documentHeight - 1) {
            row = rows[rows.length - 1];
        } else {
            const anchor = geometry.anchorY;
            row = rows.filter(candidate => candidate.top <= anchor).at(-1) || rows[0];
            // A small dead band avoids subpixel and late-font/layout jitter at row boundaries.
            const previous = rows.find(candidate => candidate.ids.some(id => state.areaSections.includes(id)));
            if (previous && previous !== row) {
                const boundary = Math.max(previous.top, row.top);
                if (Math.abs(boundary - anchor) <= tolerance) row = previous;
            }
        }
        const activeSection = row.ids.includes(state.preferredSection) ? state.preferredSection
            : row.ids.includes(state.activeSection) ? state.activeSection : row.ids[0];
        return { ...state, activeSection, areaSections: row.ids, explicitSection: row.ids.includes(state.explicitSection) ? state.explicitSection : "" };
    }
    return Object.freeze({ create, select, reconcile });
});
