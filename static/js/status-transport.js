(function initStatusTransport(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.StatusTransport = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function statusTransportFactory() {
    "use strict";

    function createController(options = {}) {
        const timers = options.timers || globalThis;
        const EventSourceType = options.EventSourceType;
        let source = null;
        let reconnectTimer = null;
        let reconnectDelay = 1000;
        let serverPoll = null;
        let extrasPoll = null;

        function configurePolling(serverMs, extrasMs) {
            if (serverPoll !== null) timers.clearInterval(serverPoll);
            if (extrasPoll !== null) timers.clearInterval(extrasPoll);
            serverPoll = timers.setInterval(() => options.onServersPoll?.(), serverMs);
            extrasPoll = timers.setInterval(() => options.onExtrasPoll?.(), extrasMs);
        }

        function scheduleReconnect() {
            if (reconnectTimer !== null) return;
            const delay = reconnectDelay;
            reconnectDelay = Math.min(reconnectDelay * 2, 30000);
            reconnectTimer = timers.setTimeout(() => {
                reconnectTimer = null;
                connect();
            }, delay);
        }

        function dashboardEventPayload(event) {
            try {
                const payload = JSON.parse(String(event?.data || ""));
                return payload && typeof payload === "object"
                    ? { ...payload, reason: String(payload.reason || "changed") }
                    : { reason: "changed" };
            } catch (_) {
                return { reason: "changed" };
            }
        }

        function connect() {
            if (!EventSourceType) {
                configurePolling(5000, 30000);
                options.onConnectionChanged?.(false);
                return;
            }
            if (source) source.close();
            const candidate = new EventSourceType("/api/dashboard/events");
            source = candidate;
            candidate.addEventListener("open", () => {
                reconnectDelay = 1000;
                configurePolling(10000, 60000);
                options.onConnectionChanged?.(true);
                options.onReconnect?.();
            });
            candidate.addEventListener("dashboard", event => {
                const payload = dashboardEventPayload(event);
                if (payload.reason === "job.log") {
                    options.onJobLogEvent?.(payload);
                    return;
                }
                options.onDashboardEvent?.(payload.reason, payload);
            });
            candidate.addEventListener("error", () => {
                if (source === candidate) source = null;
                candidate.close();
                configurePolling(5000, 30000);
                options.onConnectionChanged?.(false);
                scheduleReconnect();
            });
        }

        function start() {
            configurePolling(5000, 30000);
            connect();
        }

        function isLive() {
            return source !== null;
        }

        return Object.freeze({ start, isLive });
    }

    return Object.freeze({ createController });
}));
