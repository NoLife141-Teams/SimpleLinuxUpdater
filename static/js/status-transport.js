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
        const initialStreamWaitMs = 125;
        const fleetStreams = ["servers", "dashboard"];
        const maxChangedHosts = 16;

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
            let initialRefreshTimer = null;
            let initialRefreshStarted = false;
            let dashboardRefreshTimer = null;
            const pendingStreams = new Set();
            const pendingHosts = new Set();
            let pendingRevision = 0;
            let pendingLogRecovery = false;

            function startInitialRefresh() {
                if (initialRefreshStarted) return false;
                initialRefreshStarted = true;
                if (initialRefreshTimer !== null) {
                    timers.clearTimeout(initialRefreshTimer);
                    initialRefreshTimer = null;
                }
                options.onDashboardRefresh?.(allRefreshStreams);
                return true;
            }

            function queueDashboardRefresh(streams, { host, revision, recoverLogs = false } = {}) {
                streams.forEach(stream => pendingStreams.add(stream));
                pendingLogRecovery ||= recoverLogs;
                if (host) {
                    pendingHosts.add(host);
                    pendingRevision = revision;
                }
                if (dashboardRefreshTimer !== null) return;
                const ticket = generation;
                dashboardRefreshTimer = timers.setTimeout(() => {
                    if (!started || ticket !== generation) return;
                    dashboardRefreshTimer = null;
                    const recoverLogs = pendingLogRecovery;
                    pendingLogRecovery = false;
                    const names = [...pendingHosts];
                    const revision = pendingRevision;
                    const needsFullFleet = fleetStreams.some(stream => pendingStreams.has(stream));
                    if (names.length && (needsFullFleet || names.length > maxChangedHosts || !options.onHostsRefresh)) {
                        fleetStreams.forEach(stream => pendingStreams.add(stream));
                    } else if (names.length) {
                        options.onHostsRefresh(names, revision);
                    }
                    const requestedStreams = [...pendingStreams];
                    pendingStreams.clear();
                    pendingHosts.clear();
                    pendingRevision = 0;
                    if (recoverLogs) options.onReconnect?.();
                    if (requestedStreams.length) options.onDashboardRefresh?.(requestedStreams);
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
                    startInitialRefresh();
                    return;
                }
                if (source) source.close();
                const candidate = new EventSourceType("/api/dashboard/events");
                let receivedRevision = 0;
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
                    // The server subscribes before sending the first stream bytes.
                    // Reconcile when we already fell back to an earlier snapshot.
                    if (!startInitialRefresh()) queueDashboardRefresh(allRefreshStreams);
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
                    const revision = payload.revision;
                    if (Number.isSafeInteger(revision) && revision > 0) {
                        const gap = receivedRevision && revision !== receivedRevision + 1;
                        receivedRevision = revision;
                        if (gap) {
                            queueDashboardRefresh(allRefreshStreams, { recoverLogs: true });
                            return;
                        }
                        if (payload.reason === "job.update" && typeof payload.server_name === "string" && payload.server_name) {
                            queueDashboardRefresh([], { host: payload.server_name, revision });
                            return;
                        }
                    }
                    const streams = refreshStreamsByReason.get(payload.reason);
                    queueDashboardRefresh(streams ?? allRefreshStreams, { recoverLogs: !streams && payload.audit !== true });
                });
                candidate.addEventListener("error", () => {
                    if (!current()) return;
                    if (source === candidate) source = null;
                    candidate.close();
                    configurePolling(5000, 30000);
                    options.onConnectionChanged?.(false);
                    startInitialRefresh();
                    scheduleReconnect();
                });
            }

            function start() {
                if (started) return;
                started = true;
                generation++;
                configurePolling(5000, 30000);
                if (EventSourceType) {
                    const ticket = generation;
                    initialRefreshTimer = timers.setTimeout(() => {
                        if (started && ticket === generation) startInitialRefresh();
                    }, initialStreamWaitMs);
                }
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
                if (initialRefreshTimer !== null) timers.clearTimeout(initialRefreshTimer);
                if (dashboardRefreshTimer !== null) timers.clearTimeout(dashboardRefreshTimer);
                serverPoll = extrasPoll = reconnectTimer = initialRefreshTimer = dashboardRefreshTimer = null;
                pendingStreams.clear();
                pendingHosts.clear();
                pendingRevision = 0;
                pendingLogRecovery = false;
                initialRefreshStarted = false;
                reconnectDelay = 1000;
            }

            return Object.freeze({ start, init: start, destroy, isLive });
        }

        return Object.freeze({ createController });
    },
);
