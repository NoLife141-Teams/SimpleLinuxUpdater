// Browser effects for ManageAuditAdapter; accepted state and command plans stay in the injected interaction owner.
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
        const {
            managePageInteraction,
            escapeHtml,
            safeStatusClassToken,
            encodeURIComponent,
            activateModalFocus,
            releaseModalFocus,
            renderManageWorkspace,
            parseErrorResponse,
        } = options;
        let auditFetchHadError = false;

        function auditDateTimeToRFC3339(value) {
            const raw = String(value || "").trim();
            if (!raw) return "";
            const parsed = new Date(raw);
            if (Number.isNaN(parsed.getTime())) return "";
            return parsed.toISOString();
        }

        function auditRFC3339ToDateTimeInput(value) {
            const parsed = new Date(String(value || "").trim());
            if (Number.isNaN(parsed.getTime())) return "";
            const localTime = new Date(
                parsed.getTime() - parsed.getTimezoneOffset() * 60_000,
            );
            return localTime.toISOString().slice(0, 19);
        }

        function hydrateAuditDateTimeInput(input, value) {
            const raw = String(value || "").trim();
            input.value = auditRFC3339ToDateTimeInput(raw);
            input.dataset.initialRFC3339 = raw;
            input.dataset.initialValue = input.value;
        }

        function auditDateTimeInputToRFC3339(input) {
            if (
                input.value === input.dataset.initialValue &&
                input.dataset.initialRFC3339
            ) {
                return input.dataset.initialRFC3339;
            }
            return auditDateTimeToRFC3339(input.value);
        }

        function renderAuditFailureCauseFilter() {
            const container = document.getElementById(
                "audit-failure-cause-active",
            );
            const value = document.getElementById("audit-failure-cause-value");
            if (!container || !value) return;
            const failureCause =
                managePageInteraction.getView().audit.query.failureCause;
            container.hidden = !failureCause;
            value.textContent = failureCause;
        }

        function prettyAuditMetadata(raw) {
            const text = String(raw || "").trim();
            if (!text) return "{}";
            try {
                return JSON.stringify(JSON.parse(text), null, 2);
            } catch (_) {
                if (!lifecycle.active || _?.name === "AbortError") return;
                return text;
            }
        }

        function auditEventByID(id) {
            return managePageInteraction
                .getView()
                .audit.items.find((evt) => String(evt.id) === String(id));
        }

        function openAuditDetailDrawer(evt) {
            if (!evt) return;
            managePageInteraction.dispatch({
                type: "auditDetailSelected",
                id: evt.id,
            });
            const modal = document.getElementById("audit-detail-modal");
            const status = escapeHtml(evt.status || "unknown");
            const statusClass = `status-${safeStatusClassToken(evt.status)}`;
            const createdAt = window.formatAppTimestamp
                ? window.formatAppTimestamp(evt.created_at, {
                      titleUTC: true,
                      preformattedPrimary: evt.created_at_display,
                  })
                : {
                      primary: evt.created_at || "",
                      title: evt.created_at || "",
                  };
            document.getElementById("audit-detail-title").textContent =
                `Audit #${evt.id}`;
            document.getElementById("audit-detail-actor").textContent =
                evt.actor || "-";
            document.getElementById("audit-detail-status").innerHTML =
                `<span class="status-badge ${statusClass}">${status}</span>`;
            document.getElementById("audit-detail-action").textContent =
                evt.action || "-";
            document.getElementById("audit-detail-target").textContent =
                `${evt.target_type || "-"}: ${evt.target_name || "-"}`;
            document.getElementById("audit-detail-time").textContent =
                createdAt.primary || evt.created_at || "-";
            document.getElementById("audit-detail-client-ip").textContent =
                evt.client_ip || "-";
            document.getElementById("audit-detail-request-id").textContent =
                evt.request_id || "-";
            document.getElementById("audit-detail-message").textContent =
                evt.message || "-";
            document.getElementById("audit-detail-meta").textContent =
                prettyAuditMetadata(evt.meta_json);
            const report = document.getElementById("audit-detail-report");
            report.href = `/api/reports/audit/${encodeURIComponent(evt.id)}`;
            modal.classList.add("active");
            activateModalFocus(
                modal,
                document.getElementById("audit-detail-close"),
            );
        }

        function closeAuditDetailDrawer() {
            const modal = document.getElementById("audit-detail-modal");
            modal.classList.remove("active");
            releaseModalFocus(modal);
            managePageInteraction.dispatch({
                type: "auditDetailSelected",
                id: "",
            });
        }

        async function copyAuditDetail() {
            const lifetime = lifecycle.capture();

            const fieldText = (id) =>
                document.getElementById(id).textContent.trim();
            const text = [
                fieldText("audit-detail-title"),
                `Actor: ${fieldText("audit-detail-actor")}`,
                `Status: ${fieldText("audit-detail-status")}`,
                `Action: ${fieldText("audit-detail-action")}`,
                `Target: ${fieldText("audit-detail-target")}`,
                `Time: ${fieldText("audit-detail-time")}`,
                `Client IP: ${fieldText("audit-detail-client-ip")}`,
                `Request ID: ${fieldText("audit-detail-request-id")}`,
                `Message: ${fieldText("audit-detail-message")}`,
                "Metadata:",
                fieldText("audit-detail-meta"),
            ].join("\n");
            try {
                await navigator.clipboard.writeText(text);
                if (!lifecycle.isCurrent(lifetime)) return;
                window.notifyApp("Audit details copied.");
            } catch (_) {
                if (!lifecycle.isCurrent(lifetime) || _?.name === "AbortError")
                    return;
                window.notifyApp(
                    "Failed to copy audit details. Copy them manually from the dialog.",
                );
            }
        }

        function renderAuditTable() {
            const tbody = document.querySelector("#audit-table tbody");
            if (!tbody) return;
            const projection = managePageInteraction.getView().audit;
            renderAuditFailureCauseFilter();
            tbody.innerHTML = "";
            if (!projection.items.length) {
                const row = document.createElement("tr");
                row.innerHTML =
                    '<td colspan="7" class="subtle">No activity yet.</td>';
                tbody.appendChild(row);
            } else {
                projection.items.forEach((evt) => {
                    const row = document.createElement("tr");
                    const status = escapeHtml(evt.status || "unknown");
                    const statusClass = `status-${safeStatusClassToken(evt.status)}`;
                    const createdAt = window.formatAppTimestamp
                        ? window.formatAppTimestamp(evt.created_at, {
                              titleUTC: true,
                              preformattedPrimary: evt.created_at_display,
                          })
                        : {
                              primary: evt.created_at || "",
                              title: evt.created_at || "",
                          };
                    row.innerHTML = `
                        <td title="${escapeHtml(createdAt.title || "")}">${escapeHtml(createdAt.primary || "")}</td>
                        <td>${escapeHtml(evt.actor || "")}</td>
                        <td>${escapeHtml(evt.action || "")}</td>
                        <td>${escapeHtml(evt.target_type || "")}: ${escapeHtml(evt.target_name || "")}</td>
                        <td><span class="status-badge ${statusClass}">${status}</span></td>
                        <td>${escapeHtml(evt.message || "")}</td>
                        <td><div class="audit-event-actions"><button class="inline-btn btn-ghost" type="button" data-audit-detail="${escapeHtml(String(evt.id))}">Details</button><a class="inline-btn btn-ghost" href="/api/reports/audit/${encodeURIComponent(evt.id)}">Report</a></div></td>
                    `;
                    tbody.appendChild(row);
                });
            }
            const totalPages = Math.max(
                1,
                Math.ceil(projection.total / projection.query.pageSize),
            );
            const currentPage = Math.min(projection.query.page, totalPages);
            document.getElementById("audit-page-info").textContent =
                `Page ${currentPage} of ${totalPages} (${projection.total} events)`;
            renderManageWorkspace();
        }

        async function performAuditRequest(request, silent) {
            const lifetime = lifecycle.capture();

            const query =
                request.query || managePageInteraction.getView().audit.query;
            try {
                if (window.ensureAppTimezoneLoaded) {
                    await window.ensureAppTimezoneLoaded();
                    if (!lifecycle.isCurrent(lifetime)) return;
                }
                const params = new URLSearchParams({
                    page: String(query.page),
                    page_size: String(query.pageSize),
                });
                if (query.targetName)
                    params.set("target_name", query.targetName);
                if (query.action) params.set("action", query.action);
                if (query.status) params.set("status", query.status);
                if (query.failureCause)
                    params.set("failure_cause", query.failureCause);
                if (query.from) params.set("from", query.from);
                if (query.to) params.set("to", query.to);
                const res = await fetch(
                    `/api/audit-events?${params.toString()}`,
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    throw new Error(
                        await parseErrorResponse(
                            res,
                            "Failed to load audit events.",
                        ),
                    );
                }
                const data = await res.json();
                if (!lifecycle.isCurrent(lifetime)) return;
                const effects = managePageInteraction.dispatch({
                    type: "auditSnapshotReceived",
                    requestID: request.requestID,
                    data,
                });
                auditFetchHadError = false;
                renderAuditTable();
                const followup = effects.find(
                    (effect) =>
                        effect.type === "fetchSnapshot" &&
                        effect.stream === "audit",
                );
                if (followup) await performAuditRequest(followup, silent);
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                managePageInteraction.dispatch({
                    type: "snapshotFailed",
                    stream: "audit",
                    requestID: request.requestID,
                    error: err?.message,
                });
                const message =
                    err && err.message
                        ? err.message
                        : "Failed to load audit events.";
                const pageInfo = document.getElementById("audit-page-info");
                if (pageInfo) {
                    pageInfo.textContent = `Audit events unavailable: ${message}`;
                }
                if (!silent && !auditFetchHadError) {
                    console.warn("Failed to load audit events:", err);
                }
                auditFetchHadError = true;
            }
        }

        async function fetchAuditEvents(options = {}) {
            const lifetime = lifecycle.capture();

            const current = managePageInteraction.getView().audit.query;
            const patch = {
                targetName: document
                    .getElementById("audit-target-filter")
                    .value.trim(),
                action: document
                    .getElementById("audit-action-filter")
                    .value.trim(),
                status: document.getElementById("audit-status-filter").value,
                failureCause:
                    managePageInteraction.getView().audit.query.failureCause,
                from: auditDateTimeInputToRFC3339(
                    document.getElementById("audit-from-filter"),
                ),
                to: auditDateTimeInputToRFC3339(
                    document.getElementById("audit-to-filter"),
                ),
                page: current.page,
                pageSize: current.pageSize,
            };
            managePageInteraction.dispatch({
                type: "auditQueryChanged",
                patch,
            });
            const query = managePageInteraction.getView().audit.query;
            const request = managePageInteraction
                .dispatch({
                    type: "snapshotRequested",
                    stream: "audit",
                    payload: { query },
                })
                .find((effect) => effect.type === "fetchSnapshot");
            if (!request) return;
            await performAuditRequest(request, !!options.silent);
            if (!lifecycle.isCurrent(lifetime)) return;
        }
        return Object.freeze({
            init: lifecycle.init,
            destroy: lifecycle.destroy,
            hydrateAuditDateTimeInput: (...args) =>
                lifecycle.active
                    ? hydrateAuditDateTimeInput(...args)
                    : undefined,
            renderAuditFailureCauseFilter: (...args) =>
                lifecycle.active
                    ? renderAuditFailureCauseFilter(...args)
                    : undefined,
            auditEventByID: (...args) =>
                lifecycle.active ? auditEventByID(...args) : undefined,
            openAuditDetailDrawer: (...args) =>
                lifecycle.active ? openAuditDetailDrawer(...args) : undefined,
            closeAuditDetailDrawer: (...args) =>
                lifecycle.active ? closeAuditDetailDrawer(...args) : undefined,
            copyAuditDetail: (...args) =>
                lifecycle.active ? copyAuditDetail(...args) : undefined,
            fetchAuditEvents: (...args) =>
                lifecycle.active ? fetchAuditEvents(...args) : undefined,
        });
    }
    root.ManageAuditAdapter = Object.freeze({ create });
})(globalThis);
