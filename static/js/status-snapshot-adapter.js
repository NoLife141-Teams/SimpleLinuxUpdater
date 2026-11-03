// Browser effects for StatusSnapshotAdapter; accepted state and command plans stay in the injected interaction owner.
(function (root) {
    "use strict";
    function create(options) {
        const lifecycle = root.BrowserEffects.create();
        const fetch = lifecycle.fetch;
        const IntersectionObserver = root.IntersectionObserver
            ? lifecycle.observer(root.IntersectionObserver)
            : undefined;
        const ResizeObserver = root.ResizeObserver
            ? lifecycle.observer(root.ResizeObserver)
            : undefined;
        const { dispatchStatusInteraction, encodeURIComponent } = options;
        async function executeRecentActivityFetch(effect) {
            const lifetime = lifecycle.capture();

            if (!window.canUseScope("audit:read")) {
                return dispatchStatusInteraction({
                    type: "secondarySnapshotReceived",
                    stream: "audit",
                    requestId: effect.requestId,
                    snapshot: [],
                    receivedAt: new Date().toISOString(),
                });
            }
            try {
                const response = await fetch(
                    "/api/audit-events?page=1&page_size=8",
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!response.ok) {
                    lifecycle.discardResponse(response);
                    throw new Error(`HTTP ${response.status}`);
                }
                const data = await response.json();
                if (!lifecycle.isCurrent(lifetime)) return;
                await dispatchStatusInteraction({
                    type: "secondarySnapshotReceived",
                    stream: "audit",
                    requestId: effect.requestId,
                    snapshot: Array.isArray(data?.items) ? data.items : [],
                    receivedAt: new Date().toISOString(),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                await dispatchStatusInteraction({
                    type: "snapshotFailed",
                    stream: "audit",
                    requestId: effect.requestId,
                    error: err?.message || String(err),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            }
        }

        async function executeObservabilitySummaryFetch(effect) {
            const lifetime = lifecycle.capture();

            try {
                const response = await fetch(
                    "/api/observability/summary?window=7d",
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!response.ok) {
                    lifecycle.discardResponse(response);
                    throw new Error(`HTTP ${response.status}`);
                }
                await dispatchStatusInteraction({
                    type: "secondarySnapshotReceived",
                    stream: "observability",
                    requestId: effect.requestId,
                    snapshot: await response.json(),
                    receivedAt: new Date().toISOString(),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                await dispatchStatusInteraction({
                    type: "snapshotFailed",
                    stream: "observability",
                    requestId: effect.requestId,
                    error: err?.message || String(err),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            }
        }

        async function executePolicySummaryFetch(effect) {
            const lifetime = lifecycle.capture();

            if (!window.canUseScope("policies:write")) {
                return dispatchStatusInteraction({
                    type: "secondarySnapshotReceived",
                    stream: "policies",
                    requestId: effect.requestId,
                    snapshot: [],
                    receivedAt: new Date().toISOString(),
                });
            }
            try {
                const response = await fetch("/api/update-policies");
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!response.ok) {
                    lifecycle.discardResponse(response);
                    throw new Error(`HTTP ${response.status}`);
                }
                const data = await response.json();
                if (!lifecycle.isCurrent(lifetime)) return;
                await dispatchStatusInteraction({
                    type: "secondarySnapshotReceived",
                    stream: "policies",
                    requestId: effect.requestId,
                    snapshot: Array.isArray(data)
                        ? data
                        : Array.isArray(data?.items)
                          ? data.items
                          : [],
                    receivedAt: new Date().toISOString(),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                await dispatchStatusInteraction({
                    type: "snapshotFailed",
                    stream: "policies",
                    requestId: effect.requestId,
                    error: err?.message || String(err),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            }
        }

        function executeStatusSnapshotFetch(effect) {
            if (effect.stream === "servers")
                return executeServersSnapshotFetch(effect);
            if (effect.stream === "dashboard")
                return executeDashboardSnapshotFetch(effect);
            if (effect.stream === "commandHistory")
                return executeCommandHistoryFetch(effect);
            if (effect.stream === "audit")
                return executeRecentActivityFetch(effect);
            if (effect.stream === "observability")
                return executeObservabilitySummaryFetch(effect);
            if (effect.stream === "policies")
                return executePolicySummaryFetch(effect);
            if (effect.stream === "globalKey")
                return executeGlobalKeyStatusFetch(effect);
            return Promise.resolve();
        }

        async function executeDashboardSnapshotFetch(effect) {
            const lifetime = lifecycle.capture();

            try {
                const response = await fetch(
                    "/api/dashboard/summary?window=7d&include_command_history=false&include_vulnerability_findings=false",
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!response.ok) {
                    lifecycle.discardResponse(response);
                    throw new Error(`HTTP ${response.status}`);
                }
                const nextDashboardSummary = await response.json();
                if (!lifecycle.isCurrent(lifetime)) return;
                await dispatchStatusInteraction({
                    type: "dashboardSnapshotReceived",
                    requestId: effect.requestId,
                    snapshot: nextDashboardSummary,
                    receivedAt: new Date().toISOString(),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                await dispatchStatusInteraction({
                    type: "snapshotFailed",
                    stream: "dashboard",
                    requestId: effect.requestId,
                    error: err?.message || String(err),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            }
        }

        async function executeCommandHistoryFetch(effect) {
            const lifetime = lifecycle.capture();

            if (!window.canUseScope("audit:read"))
                return dispatchStatusInteraction({
                    type: "commandHistoryReceived",
                    requestId: effect.requestId,
                    serverName: effect.serverName,
                    history: [],
                    receivedAt: new Date().toISOString(),
                });
            const serverName = String(effect.serverName || "");
            try {
                const query = new URLSearchParams({
                    page: "1",
                    page_size: "8",
                    order: "created_at",
                    target_type: "server",
                    target_name: serverName,
                });
                if (effect.from) query.set("from", String(effect.from));
                if (effect.to) query.set("to", String(effect.to));
                const response = await fetch(
                    `/api/audit-events?${query.toString()}`,
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!response.ok) {
                    lifecycle.discardResponse(response);
                    throw new Error(`HTTP ${response.status}`);
                }
                const data = await response.json();
                if (!lifecycle.isCurrent(lifetime)) return;
                await dispatchStatusInteraction({
                    type: "commandHistoryReceived",
                    requestId: effect.requestId,
                    serverName,
                    history: Array.isArray(data?.items) ? data.items : [],
                    receivedAt: new Date().toISOString(),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                await dispatchStatusInteraction({
                    type: "snapshotFailed",
                    stream: "commandHistory",
                    requestId: effect.requestId,
                    error: err?.message || String(err),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            }
        }

        async function executeJobLogFetch(effect) {
            const lifetime = lifecycle.capture();

            if (!window.canUseScope("logs:read")) return;
            const jobId = String(effect.jobId || "");
            const serverName = String(effect.serverName || "");
            const afterSequence = Math.max(
                0,
                Number(effect.afterSequence || 0),
            );
            try {
                const response = await fetch(
                    `/api/jobs/${encodeURIComponent(jobId)}/logs?after_seq=${encodeURIComponent(afterSequence)}&limit=200`,
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!response.ok) {
                    lifecycle.discardResponse(response);
                    throw new Error(`HTTP ${response.status}`);
                }
                const page = await response.json();
                if (!lifecycle.isCurrent(lifetime)) return;
                await dispatchStatusInteraction({
                    type: "jobLogRecoveryReceived",
                    serverName,
                    jobId,
                    afterSequence,
                    page,
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                console.error(
                    `Unable to recover job logs for ${serverName}:`,
                    err,
                );
                await dispatchStatusInteraction({
                    type: "jobLogRecoveryFailed",
                    serverName,
                    jobId,
                    error: err?.message || String(err),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            }
        }

        async function executeGlobalKeyStatusFetch(effect) {
            const lifetime = lifecycle.capture();

            if (!window.canUseScope("inventory:write")) {
                return dispatchStatusInteraction({
                    type: "secondarySnapshotReceived",
                    stream: "globalKey",
                    requestId: effect.requestId,
                    snapshot: false,
                    receivedAt: new Date().toISOString(),
                });
            }
            try {
                const response = await fetch("/api/keys/global");
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!response.ok) {
                    lifecycle.discardResponse(response);
                    throw new Error(`HTTP ${response.status}`);
                }
                const data = await response.json();
                if (!lifecycle.isCurrent(lifetime)) return;
                await dispatchStatusInteraction({
                    type: "secondarySnapshotReceived",
                    stream: "globalKey",
                    requestId: effect.requestId,
                    snapshot: !!data?.has_key,
                    receivedAt: new Date().toISOString(),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                await dispatchStatusInteraction({
                    type: "snapshotFailed",
                    stream: "globalKey",
                    requestId: effect.requestId,
                    error: err?.message || String(err),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            }
        }

        async function executeServersSnapshotFetch(effect) {
            const lifetime = lifecycle.capture();

            try {
                const response = await fetch("/api/servers");
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!response.ok) {
                    lifecycle.discardResponse(response);
                    throw new Error(
                        `Failed to fetch servers: HTTP ${response.status}`,
                    );
                }
                const parsedServers = await response.json();
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!Array.isArray(parsedServers)) {
                    throw new Error(
                        "Invalid servers payload: expected an array",
                    );
                }
                await dispatchStatusInteraction({
                    type: "serversSnapshotReceived",
                    requestId: effect.requestId,
                    servers: parsedServers,
                    receivedAt: new Date().toISOString(),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                console.error("Unable to refresh servers list:", err);
                await dispatchStatusInteraction({
                    type: "snapshotFailed",
                    stream: "servers",
                    requestId: effect.requestId,
                    error: err?.message || String(err),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            }
        }
        return Object.freeze({
            init: lifecycle.init,
            destroy: lifecycle.destroy,
            executeStatusSnapshotFetch: (...args) =>
                lifecycle.active
                    ? executeStatusSnapshotFetch(...args)
                    : undefined,
            executeJobLogFetch: (...args) =>
                lifecycle.active ? executeJobLogFetch(...args) : undefined,
        });
    }
    root.StatusSnapshotAdapter = Object.freeze({ create });
})(globalThis);
