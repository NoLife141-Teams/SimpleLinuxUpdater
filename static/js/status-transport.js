(function initStatusTransport(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.StatusTransport = api;
})(
    typeof globalThis !== "undefined" ? globalThis : this,
    function statusTransportFactory() {
        "use strict";

        const allRefreshStreams = ["servers", "dashboard", "globalKey", "audit", "observability", "policies"];
        const refreshStreamsByReason = new Map([
            ["job.create", ["servers", "dashboard"]],
            ["job.upsert", ["servers", "dashboard"]],
            ["job.update", ["servers", "dashboard"]],
            ["host-facts-refreshed", ["servers", "dashboard"]],
            ["server-created", ["servers", "dashboard"]],
            ["servers-imported", ["servers", "dashboard"]],
            ["server-availability-changed", ["servers", "dashboard"]],
            ["servers-bulk-updated", ["servers", "dashboard"]],
            ["package_deferral.changed", ["servers", "dashboard"]],
            ["update_policy.create", ["dashboard", "policies", "audit"]],
            ["update_policy.update", ["dashboard", "policies", "audit"]],
            ["update_policy.delete", ["dashboard", "policies", "audit"]],
            ["update_policy.override", ["dashboard", "audit"]],
            ["update_policy.settings", ["dashboard", "audit"]],
        ]);
        const dashboardBurstWindowMs = 125;

        function createController(options = {}) {
            const timers = options.timers || globalThis;
            const EventSourceType = options.EventSourceType;
            let source = null;
            let reconnectTimer = null;
            let reconnectDelay = 1000;
            let serverPoll = null;
            let extrasPoll = null;
            let started = false;
            let generation = 0;
            let disconnected = false;
            let dashboardRefreshTimer = null;
            const pendingStreams = new Set();

            function queueDashboardRefresh(streams) {
                streams.forEach(stream => pendingStreams.add(stream));
                if (dashboardRefreshTimer !== null) return;
                const ticket = generation;
                dashboardRefreshTimer = timers.setTimeout(() => {
                    if (!started || ticket !== generation) return;
                    dashboardRefreshTimer = null;
                    const streams = [...pendingStreams];
                    pendingStreams.clear();
                    options.onDashboardRefresh?.(streams);
                }, dashboardBurstWindowMs);
            }

            function configurePolling(serverMs, extrasMs) {
                const ticket = generation;
                if (serverPoll !== null) timers.clearInterval(serverPoll);
                if (extrasPoll !== null) timers.clearInterval(extrasPoll);
                serverPoll = timers.setInterval(() => {
                    if (started && ticket === generation)
                        options.onServersPoll?.();
                }, serverMs);
                extrasPoll = timers.setInterval(() => {
                    if (started && ticket === generation)
                        options.onExtrasPoll?.();
                }, extrasMs);
            }

            function scheduleReconnect() {
                if (reconnectTimer !== null) return;
                const delay = reconnectDelay;
                const ticket = generation;
                reconnectDelay = Math.min(reconnectDelay * 2, 30000);
                reconnectTimer = timers.setTimeout(() => {
                    if (!started || ticket !== generation) return;
                    reconnectTimer = null;
                    connect();
                }, delay);
            }

            function dashboardEventPayload(event) {
                try {
                    const payload = JSON.parse(String(event?.data || ""));
                    return payload && typeof payload === "object"
                        ? {
                              ...payload,
                              reason: String(payload.reason || "changed"),
                          }
                        : { reason: "changed" };
                } catch (_) {
                    return { reason: "changed" };
                }
            }

            function connect() {
                if (!started) return;
                if (!EventSourceType) {
                    configurePolling(5000, 30000);
                    options.onConnectionChanged?.(false);
                    return;
                }
                if (source) source.close();
                const candidate = new EventSourceType("/api/dashboard/events");
                const ticket = generation;
                const current = () =>
                    started && ticket === generation && source === candidate;
                source = candidate;
                candidate.addEventListener("open", () => {
                    if (!current()) return;
                    reconnectDelay = 1000;
                    configurePolling(10000, 60000);
                    options.onConnectionChanged?.(true);
                    options.onReconnect?.();
                    if (disconnected) {
                        disconnected = false;
                        queueDashboardRefresh(allRefreshStreams);
                    }
                });
                candidate.addEventListener("dashboard", (event) => {
                    if (!current()) return;
                    const payload = dashboardEventPayload(event);
                    // Opening the stream is already handled by the open listener.
                    if (payload.reason === "connected") return;
                    if (payload.reason === "job.log") {
                        options.onJobLogEvent?.(payload);
                        return;
                    }
                    queueDashboardRefresh(refreshStreamsByReason.get(payload.reason) ?? allRefreshStreams);
                });
                candidate.addEventListener("error", () => {
                    if (!current()) return;
                    if (source === candidate) source = null;
                    candidate.close();
                    disconnected = true;
                    configurePolling(5000, 30000);
                    options.onConnectionChanged?.(false);
                    scheduleReconnect();
                });
            }

            function start() {
                if (started) return;
                started = true;
                generation++;
                configurePolling(5000, 30000);
                connect();
            }

            function isLive() {
                return source !== null;
            }

            function destroy() {
                started = false;
                generation++;
                source?.close();
                source = null;
                if (serverPoll !== null) timers.clearInterval(serverPoll);
                if (extrasPoll !== null) timers.clearInterval(extrasPoll);
                if (reconnectTimer !== null)
                    timers.clearTimeout(reconnectTimer);
                if (dashboardRefreshTimer !== null) timers.clearTimeout(dashboardRefreshTimer);
                serverPoll = extrasPoll = reconnectTimer = dashboardRefreshTimer = null;
                pendingStreams.clear();
                disconnected = false;
                reconnectDelay = 1000;
            }

            return Object.freeze({ start, init: start, destroy, isLive });
        }

        return Object.freeze({ createController });
    },
);
