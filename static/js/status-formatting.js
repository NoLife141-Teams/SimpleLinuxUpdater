(function initStatusFormatting(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.StatusFormatting = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function statusFormattingFactory() {
    "use strict";

    function duration(ms) {
        const value = Number(ms || 0);
        if (!Number.isFinite(value) || value <= 0) return "--";
        if (value < 1000) return `${Math.round(value)}ms`;
        const seconds = value / 1000;
        if (seconds < 60) return `${seconds.toFixed(seconds < 10 ? 1 : 0)}s`;
        const minutes = Math.floor(seconds / 60);
        const remainder = Math.round(seconds % 60);
        return remainder > 0 ? `${minutes}m ${remainder}s` : `${minutes}m`;
    }

    function diskFree(kb) {
        if (kb === null || kb === undefined || kb === "") return "--";
        const value = Number(kb);
        if (!Number.isFinite(value) || value < 0) return "--";
        const gib = value / 1024 / 1024;
        if (gib >= 1) return `${gib.toFixed(gib >= 10 ? 0 : 1)} GiB`;
        return `${Math.round(value / 1024)} MiB`;
    }

    function diskCapacity(freeKB, totalKB) {
        const free = diskFree(freeKB);
        const total = diskFree(totalKB);
        if (free === "--" && total === "--") return "--";
        if (total === "--") return `${free} free`;
        if (free === "--") return `${total} total`;
        return `${free} free of ${total} total`;
    }

    function uptime(seconds) {
        const value = Number(seconds || 0);
        if (!Number.isFinite(value) || value <= 0) return "--";
        const days = Math.floor(value / 86400);
        if (days > 0) return `${days}d`;
        const hours = Math.floor(value / 3600);
        if (hours > 0) return `${hours}h`;
        return `${Math.floor(value / 60)}m`;
    }

    function statusLabel(value) {
        return String(value || "unknown").replace(/_/g, " ");
    }

    function logLines(value) {
        const lines = [];
        let current = "";
        let pendingCR = false;
        const text = String(value || "");
        for (let index = 0; index < text.length; index += 1) {
            const char = text[index];
            if (pendingCR) {
                pendingCR = false;
                if (char === "\n") {
                    lines.push(current);
                    current = "";
                    continue;
                }
                current = "";
            }
            if (char === "\r") {
                pendingCR = true;
            } else if (char === "\n") {
                lines.push(current);
                current = "";
            } else {
                current += char;
            }
        }
        if (current || pendingCR || lines.length === 0) lines.push(current);
        return lines;
    }

    return Object.freeze({ duration, diskFree, diskCapacity, uptime, statusLabel, logLines });
}));
