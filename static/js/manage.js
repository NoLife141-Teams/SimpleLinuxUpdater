// Composition of browser adapters; interaction owners retain state and decisions.
(function (root) {
    let current = null;
    function mount() {
        current?.destroy();
        const lifecycle = root.BrowserEffects.create();
        lifecycle.init();
        const fetch = lifecycle.fetch;
        const IntersectionObserver = root.IntersectionObserver
            ? lifecycle.observer(root.IntersectionObserver)
            : undefined;
        const ResizeObserver = root.ResizeObserver
            ? lifecycle.observer(root.ResizeObserver)
            : undefined;
        const modalEffects = window.ManageModalEffects.create();
        const {
            activateModalFocus,
            releaseModalFocus,
            trapActiveModalFocus,
            resetFileInputLabel,
        } = modalEffects;
        lifecycle.own(modalEffects.destroy);
        const managePageInteraction =
            window.ManagePageInteraction.createStore();
        let hostKeyModalPromise = null;
        let hostKeyModalResolvers = [];
        let editKnownHostCheckPromise = null;

        let manageInventoryRefreshPromise = Promise.resolve();

        const manageWorkspace = window.ManageWorkspaceAdapter.create({
            managePageInteraction: managePageInteraction,
            renderAddAuthMethod: (...args) => renderAddAuthMethod(...args),
        });
        const { renderManageWorkspace, initializeManageWorkspace } =
            manageWorkspace;
        manageWorkspace.init();
        lifecycle.own(manageWorkspace.destroy);
        const manageAudit = window.ManageAuditAdapter.create({
            managePageInteraction: managePageInteraction,
            escapeHtml: (...args) => escapeHtml(...args),
            safeStatusClassToken: (...args) => safeStatusClassToken(...args),
            encodeURIComponent: (...args) => encodeURIComponent(...args),
            activateModalFocus: (...args) => activateModalFocus(...args),
            releaseModalFocus: (...args) => releaseModalFocus(...args),
            renderManageWorkspace: (...args) => renderManageWorkspace(...args),
            parseErrorResponse: (...args) => parseErrorResponse(...args),
        });
        const {
            hydrateAuditDateTimeInput,
            renderAuditFailureCauseFilter,
            auditEventByID,
            openAuditDetailDrawer,
            closeAuditDetailDrawer,
            copyAuditDetail,
            fetchAuditEvents,
        } = manageAudit;
        manageAudit.init();
        lifecycle.own(manageAudit.destroy);

        function activeEditorName() {
            const editor = managePageInteraction.getView().editor;
            return editor.open ? editor.originalName : "";
        }

        function effectiveAuthLabel(value) {
            return (
                {
                    "per-server-key": "per-server SSH key",
                    "global-key": "Global SSH Credential",
                    "per-server-key-and-password":
                        "per-server SSH key and password",
                    "global-key-and-password":
                        "Global SSH Credential and password",
                    password: "password",
                    missing: "no usable credential",
                    unknown: "authentication status unknown",
                }[value] || "unknown authentication"
            );
        }

        function renderCredentialOutcomes() {
            const outcome = document.getElementById("credential-outcome");
            if (!outcome) return;
            const editor = managePageInteraction.getView().editor;
            outcome.textContent = `Clear Password → ${effectiveAuthLabel(editor.credentialOutcomes?.clearPassword)}. Clear Key → ${effectiveAuthLabel(editor.credentialOutcomes?.clearKey)}.`;
        }

        function commandExecution(command, payload = {}) {
            if (!lifecycle.active) return null;
            return managePageInteraction
                .dispatch({ type: "commandRequested", command, payload })
                .find((effect) => effect.type === "executeCommand");
        }

        async function executeManageEffects(effects, options = {}) {
            const lifetime = lifecycle.capture();

            const announce = options.announce !== false;
            const refreshStreams = new Set();
            for (const effect of effects || []) {
                if (effect.type === "announce" && announce)
                    window.notifyApp(
                        effect.message || "Manage action completed.",
                    );
                if (effect.type === "refresh")
                    (effect.streams || []).forEach((stream) =>
                        refreshStreams.add(stream),
                    );
            }
            const refreshes = [];
            if (refreshStreams.has("inventory"))
                refreshes.push(fetchManageServers());
            if (refreshStreams.has("globalKey"))
                refreshes.push(fetchGlobalKeyStatus());
            if (refreshStreams.has("audit"))
                refreshes.push(fetchAuditEvents({ silent: true }));
            await Promise.all(refreshes);
            if (!lifecycle.isCurrent(lifetime)) return;
        }

        async function settleCommand(type, plan, message, options = {}) {
            const lifetime = lifecycle.capture();

            const effects = managePageInteraction.dispatch({
                type,
                plan,
                message,
            });
            await executeManageEffects(effects, options);
            if (!lifecycle.isCurrent(lifetime)) return;
        }

        const managePolicyOverrides =
            window.ManagePolicyOverrideAdapter.createAdapter({
                store: managePageInteraction,
                escapeHTML: escapeHtml,
                requestJSON: async (url, options, fallbackMessage) => {
                    const lifetime = lifecycle.capture();

                    const response = await fetch(url, options);
                    if (!lifecycle.isCurrent(lifetime)) return;
                    if (!response.ok)
                        throw new Error(
                            await parseErrorResponse(response, fallbackMessage),
                        );
                    return response.json().catch(() => ({}));
                },
            });

        function escapeHtml(value) {
            return String(value ?? "")
                .replace(/&/g, "&amp;")
                .replace(/</g, "&lt;")
                .replace(/>/g, "&gt;")
                .replace(/"/g, "&quot;")
                .replace(/'/g, "&#39;");
        }

        function normalizePort(value, fallback = 22) {
            const parsed = Number.parseInt(value, 10);
            if (!Number.isFinite(parsed) || parsed <= 0 || parsed > 65535)
                return fallback;
            return parsed;
        }

        async function scanHostKey(host, port) {
            const lifetime = lifecycle.capture();

            const res = await fetch("/api/hostkeys/scan", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ host, port }),
            });
            if (!lifecycle.isCurrent(lifetime)) return;
            if (!res.ok) {
                throw new Error(
                    await parseErrorResponse(res, "Failed to scan host key."),
                );
            }
            return res.json();
        }

        async function trustHostKey(host, port, fingerprint) {
            const lifetime = lifecycle.capture();

            const res = await fetch("/api/hostkeys/trust", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({
                    host,
                    port,
                    fingerprint_sha256: fingerprint,
                }),
            });
            if (!lifecycle.isCurrent(lifetime)) return;
            if (!res.ok) {
                throw new Error(
                    await parseErrorResponse(res, "Failed to trust host key."),
                );
            }
            return res.json();
        }

        async function clearKnownHost(host, port) {
            const lifetime = lifecycle.capture();

            const res = await fetch("/api/hostkeys/clear", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ host, port }),
            });
            if (!lifecycle.isCurrent(lifetime)) return;
            if (!res.ok) {
                throw new Error(
                    await parseErrorResponse(
                        res,
                        "Failed to clear known host entry.",
                    ),
                );
            }
            return res.json();
        }

        function hostKeyPromptText(scanned) {
            const replacing =
                !!scanned?.host_entry_exists && !scanned?.already_trusted;
            return (
                `Host: ${scanned.host}\n` +
                `Port: ${scanned.port}\n` +
                `Algorithm: ${scanned.algorithm}\n` +
                `Fingerprint: ${scanned.fingerprint_sha256}\n\n` +
                `${replacing ? "Replace the saved key" : "Add this key"} in known_hosts?`
            );
        }

        function closeHostKeyModal(confirmed) {
            const modal = document.getElementById("hostkey-modal");
            if (modal) {
                modal.classList.remove("active");
                releaseModalFocus(modal);
            }
            const resolvers = hostKeyModalResolvers;
            hostKeyModalResolvers = [];
            hostKeyModalPromise = null;
            for (const resolver of resolvers) {
                resolver(!!confirmed);
            }
        }

        function confirmHostKeyWithModal(scanned) {
            const modal = document.getElementById("hostkey-modal");
            const details = document.getElementById("hostkey-modal-details");
            const replacing =
                !!scanned?.host_entry_exists && !scanned?.already_trusted;
            if (!modal || !details) {
                return window.confirmAction(
                    `Verify SSH host key before trusting:\n\n${hostKeyPromptText(scanned)}`,
                    { confirmLabel: replacing ? "Replace key" : "Trust key" },
                );
            }
            if (hostKeyModalPromise) {
                return Promise.resolve(false);
            }
            details.textContent = hostKeyPromptText(scanned);
            document.getElementById("hostkey-title").textContent = replacing
                ? "Replace SSH Host Key"
                : "Trust SSH Host Key";
            document.getElementById("hostkey-modal-trust").textContent =
                replacing ? "Replace Key" : "Trust Key";
            modal.classList.add("active");
            activateModalFocus(
                modal,
                document.getElementById("hostkey-modal-cancel"),
            );
            hostKeyModalPromise = new Promise((resolve) => {
                hostKeyModalResolvers = [resolve];
            });
            return hostKeyModalPromise;
        }

        async function trustHostKeyFlow(host, port, hooks = {}) {
            const lifetime = lifecycle.capture();

            if (typeof hooks.onScanning === "function") {
                hooks.onScanning();
            }
            const scanned = await scanHostKey(host, port);
            if (!lifecycle.isCurrent(lifetime)) return;
            if (typeof hooks.onScanned === "function") {
                hooks.onScanned(scanned);
            }
            if (scanned && scanned.already_trusted) {
                if (typeof hooks.onAlreadyTrusted === "function") {
                    hooks.onAlreadyTrusted(scanned);
                }
                return { alreadyTrusted: true, scanned };
            }
            const confirmed = await confirmHostKeyWithModal(scanned);
            if (!lifecycle.isCurrent(lifetime)) return;
            if (!confirmed) {
                throw new Error("Host key trust cancelled.");
            }
            if (typeof hooks.onTrusting === "function") {
                hooks.onTrusting(scanned);
            }
            const replacing =
                !!scanned?.host_entry_exists && !scanned?.already_trusted;
            const trusted = await trustHostKey(
                scanned.host,
                scanned.port,
                scanned.fingerprint_sha256,
            );
            if (!lifecycle.isCurrent(lifetime)) return;
            return {
                alreadyTrusted: !!trusted?.already_trusted,
                replaced: replacing,
                scanned,
                trusted,
            };
        }

        function saveWindowScroll() {
            return { x: window.scrollX, y: window.scrollY };
        }

        function restoreWindowScroll(pos) {
            if (!pos) return;
            window.scrollTo(pos.x, pos.y);
        }

        async function performInventoryRequest(request, pageScroll) {
            const lifetime = lifecycle.capture();

            try {
                const response = await fetch("/api/servers");
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!response.ok) {
                    throw new Error(
                        await parseErrorResponse(
                            response,
                            "Failed to load servers.",
                        ),
                    );
                }
                const servers = await response.json();
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!Array.isArray(servers)) {
                    throw new Error("Invalid server list response.");
                }
                const effects = managePageInteraction.dispatch({
                    type: "inventorySnapshotReceived",
                    requestID: request.requestID,
                    items: servers,
                });
                const tbody = document.querySelector(
                    "#manage-servers-table tbody",
                );
                tbody.innerHTML = "";
                renderTable();
                lifecycle.requestAnimationFrame(() =>
                    restoreWindowScroll(pageScroll),
                );
                const followup = effects.find(
                    (effect) =>
                        effect.type === "fetchSnapshot" &&
                        effect.stream === "inventory",
                );
                if (followup)
                    await performInventoryRequest(followup, saveWindowScroll());
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (error) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    error?.name === "AbortError"
                )
                    return;
                const effects = managePageInteraction.dispatch({
                    type: "snapshotFailed",
                    stream: "inventory",
                    requestID: request.requestID,
                    error: error?.message,
                });
                window.notifyApp(error?.message || "Failed to load servers.");
                const followup = effects.find(
                    (effect) =>
                        effect.type === "fetchSnapshot" &&
                        effect.stream === "inventory",
                );
                if (followup)
                    await performInventoryRequest(followup, saveWindowScroll());
                if (!lifecycle.isCurrent(lifetime)) return;
            }
        }

        async function fetchManageServers() {
            const lifetime = lifecycle.capture();

            const request = managePageInteraction
                .dispatch({ type: "snapshotRequested", stream: "inventory" })
                .find((effect) => effect.type === "fetchSnapshot");
            if (request)
                manageInventoryRefreshPromise = performInventoryRequest(
                    request,
                    saveWindowScroll(),
                );
            await manageInventoryRefreshPromise;
            if (!lifecycle.isCurrent(lifetime)) return;
        }

        function renderTable() {
            const tbody = document.querySelector("#manage-servers-table tbody");
            tbody.innerHTML = "";
            const projection = managePageInteraction.getView().inventory;
            document.getElementById("page-info").textContent =
                `Page ${projection.page} of ${projection.totalPages} (${projection.total} hosts)`;
            document.getElementById("server-filter-result").textContent =
                projection.activeFilterCount
                    ? `${projection.total} matching ${projection.total === 1 ? "server" : "servers"} · ${projection.activeFilterCount} active ${projection.activeFilterCount === 1 ? "filter" : "filters"}`
                    : `${projection.total} ${projection.total === 1 ? "server" : "servers"} visible`;
            document.getElementById("clear-server-filters").disabled =
                projection.activeFilterCount === 0;
            const groups = projection.groups;
            groups.forEach((group) => {
                if (group.key) {
                    const groupRow = document.createElement("tr");
                    groupRow.className = "group-row";
                    groupRow.innerHTML = `<td colspan="6">${escapeHtml(group.key)}</td>`;
                    tbody.appendChild(groupRow);
                }
                group.items.forEach((server) => {
                    const row = document.createElement("tr");
                    row.dataset.name = server.name;
                    const safeName = escapeHtml(server.name);
                    const safeHost = escapeHtml(server.host);
                    const safeUser = escapeHtml(server.user);
                    const safeDataName = escapeHtml(server.name);
                    row.innerHTML = `
                        <td>${safeName}<small class="server-availability" title="${escapeHtml(server.availability.description)}">${server.availability.label}</small></td>
                        <td><span class="server-endpoint"><span>${safeHost}</span><small>SSH port ${escapeHtml(server.port || 22)}</small></span></td>
                        <td>${safeUser}</td>
                        <td>${renderTags(server.tags)}</td>
                        <td>${renderAccessPosture(server)}</td>
                        <td><div class="row-actions">
                            <button type="button" class="btn-ghost inline-btn" data-action="toggle-server" data-name="${safeDataName}" title="${escapeHtml(server.availability.description)}" ${server.availability.busy ? "disabled" : ""}>${server.availability.actionLabel}</button>
                            <button type="button" class="btn-ghost" data-action="edit-server" data-name="${safeDataName}">Edit</button>
                            <details class="row-actions-menu">
                                <summary class="btn-ghost inline-btn" aria-label="More actions for ${safeName}">More</summary>
                                <div><button type="button" class="btn-danger inline-btn" data-action="delete-server" data-name="${safeDataName}">Delete</button></div>
                            </details>
                        </div></td>
                    `;
                    tbody.appendChild(row);
                });
            });
            renderManageWorkspace();
        }

        lifecycle.listen(
            document.querySelector("#manage-servers-table tbody"),
            "click",
            (e) => {
                const button = e.target.closest("button[data-action]");
                if (!button) return;
                const name = button.dataset.name || "";
                if (!name) return;
                const action = button.dataset.action || "";
                if (action === "edit-server") {
                    editServer(name);
                    return;
                }
                if (action === "toggle-server") {
                    toggleServerAvailability(name);
                    return;
                }
                if (action === "delete-server") {
                    deleteServer(name);
                }
            },
        );

        const applyManageSortFromHeader = (th) => {
            if (!th) return;
            const key = th.dataset.sortKey;
            managePageInteraction.dispatch({ type: "sortChanged", key });
            renderTable();
        };

        document
            .querySelectorAll("#manage-servers-table th.sortable")
            .forEach((th) => {
                const trigger = th.querySelector(".sort-header-btn");
                if (trigger) {
                    lifecycle.listen(trigger, "click", () => {
                        applyManageSortFromHeader(th);
                    });
                    return;
                }
                lifecycle.listen(th, "click", () => {
                    applyManageSortFromHeader(th);
                });
            });

        function syncInventoryFilters() {
            managePageInteraction.dispatch({
                type: "filtersChanged",
                patch: {
                    search: document.getElementById("search").value,
                    tag: document.getElementById("tag-filter").value,
                    auth: document.getElementById("auth-filter").value,
                    group: document.getElementById("group-by").value,
                    pageSize: document.getElementById("page-size").value,
                },
            });
            renderTable();
        }
        lifecycle.listen(
            document.getElementById("search"),
            "input",
            syncInventoryFilters,
        );
        lifecycle.listen(
            document.getElementById("tag-filter"),
            "input",
            syncInventoryFilters,
        );
        lifecycle.listen(
            document.getElementById("auth-filter"),
            "change",
            syncInventoryFilters,
        );
        lifecycle.listen(
            document.getElementById("group-by"),
            "change",
            syncInventoryFilters,
        );
        lifecycle.listen(
            document.getElementById("page-size"),
            "change",
            syncInventoryFilters,
        );
        lifecycle.listen(
            document.getElementById("clear-server-filters"),
            "click",
            () => {
                document.getElementById("search").value = "";
                document.getElementById("tag-filter").value = "";
                document.getElementById("auth-filter").value = "";
                document.getElementById("group-by").value = "";
                document.getElementById("page-size").value = "20";
                managePageInteraction.dispatch({ type: "filtersReset" });
                renderTable();
            },
        );

        lifecycle.listen(document.getElementById("prev-page"), "click", () => {
            managePageInteraction.dispatch({
                type: "pageChanged",
                page: Math.max(
                    1,
                    managePageInteraction.getView().inventory.page - 1,
                ),
            });
            renderTable();
        });
        lifecycle.listen(document.getElementById("next-page"), "click", () => {
            managePageInteraction.dispatch({
                type: "pageChanged",
                page: managePageInteraction.getView().inventory.page + 1,
            });
            renderTable();
        });
        lifecycle.listen(
            document.getElementById("audit-prev-page"),
            "click",
            async () => {
                const lifetime = lifecycle.capture();

                managePageInteraction.dispatch({
                    type: "auditQueryChanged",
                    patch: {
                        page: Math.max(
                            1,
                            managePageInteraction.getView().audit.query.page -
                                1,
                        ),
                    },
                });
                await fetchAuditEvents();
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("audit-next-page"),
            "click",
            async () => {
                const lifetime = lifecycle.capture();

                managePageInteraction.dispatch({
                    type: "auditQueryChanged",
                    patch: {
                        page:
                            managePageInteraction.getView().audit.query.page +
                            1,
                    },
                });
                await fetchAuditEvents();
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("audit-target-filter"),
            "input",
            async () => {
                const lifetime = lifecycle.capture();

                managePageInteraction.dispatch({
                    type: "auditQueryChanged",
                    patch: { page: 1 },
                });
                await fetchAuditEvents();
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("audit-action-filter"),
            "input",
            async () => {
                const lifetime = lifecycle.capture();

                managePageInteraction.dispatch({
                    type: "auditQueryChanged",
                    patch: { page: 1 },
                });
                document.getElementById("audit-action-preset").value = "";
                await fetchAuditEvents();
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("audit-action-preset"),
            "change",
            async () => {
                const lifetime = lifecycle.capture();

                document.getElementById("audit-action-filter").value =
                    document.getElementById("audit-action-preset").value;
                managePageInteraction.dispatch({
                    type: "auditQueryChanged",
                    patch: { page: 1 },
                });
                await fetchAuditEvents();
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("audit-status-filter"),
            "change",
            async () => {
                const lifetime = lifecycle.capture();

                managePageInteraction.dispatch({
                    type: "auditQueryChanged",
                    patch: { page: 1 },
                });
                await fetchAuditEvents();
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("audit-from-filter"),
            "change",
            async () => {
                const lifetime = lifecycle.capture();

                managePageInteraction.dispatch({
                    type: "auditQueryChanged",
                    patch: { page: 1 },
                });
                await fetchAuditEvents();
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("audit-from-filter"),
            "input",
            async () => {
                const lifetime = lifecycle.capture();

                managePageInteraction.dispatch({
                    type: "auditQueryChanged",
                    patch: { page: 1 },
                });
                await fetchAuditEvents();
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("audit-to-filter"),
            "change",
            async () => {
                const lifetime = lifecycle.capture();

                managePageInteraction.dispatch({
                    type: "auditQueryChanged",
                    patch: { page: 1 },
                });
                await fetchAuditEvents();
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("audit-to-filter"),
            "input",
            async () => {
                const lifetime = lifecycle.capture();

                managePageInteraction.dispatch({
                    type: "auditQueryChanged",
                    patch: { page: 1 },
                });
                await fetchAuditEvents();
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("audit-refresh"),
            "click",
            fetchAuditEvents,
        );
        lifecycle.listen(
            document.getElementById("audit-clear-filters"),
            "click",
            async () => {
                const lifetime = lifecycle.capture();

                for (const id of [
                    "audit-target-filter",
                    "audit-action-filter",
                    "audit-action-preset",
                    "audit-status-filter",
                    "audit-from-filter",
                    "audit-to-filter",
                ]) {
                    document.getElementById(id).value = "";
                }
                managePageInteraction.dispatch({
                    type: "auditQueryChanged",
                    patch: {
                        targetName: "",
                        action: "",
                        status: "",
                        failureCause: "",
                        from: "",
                        to: "",
                        page: 1,
                    },
                });
                const url = new URL(window.location.href);
                for (const parameter of [
                    "audit_target",
                    "audit_action",
                    "audit_status",
                    "failure_cause",
                    "audit_from",
                    "audit_to",
                ]) {
                    url.searchParams.delete(parameter);
                }
                window.history.replaceState(
                    null,
                    "",
                    `${url.pathname}${url.search}${url.hash}`,
                );
                renderAuditFailureCauseFilter();
                await fetchAuditEvents();
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("audit-clear-failure-cause"),
            "click",
            async () => {
                const lifetime = lifecycle.capture();

                managePageInteraction.dispatch({
                    type: "auditQueryChanged",
                    patch: { failureCause: "", page: 1 },
                });
                const url = new URL(window.location.href);
                url.searchParams.delete("failure_cause");
                window.history.replaceState(
                    null,
                    "",
                    `${url.pathname}${url.search}${url.hash}`,
                );
                renderAuditFailureCauseFilter();
                await fetchAuditEvents();
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("audit-prune"),
            "click",
            async () => {
                const lifetime = lifecycle.capture();

                if (
                    !(await window.confirmTypedAction(
                        "Prune audit events older than the configured retention window?",
                        "PRUNE",
                    ))
                ) {
                    return;
                }
                if (!lifecycle.isCurrent(lifetime)) return;
                const command = managePageInteraction.dispatch({
                    type: "commandRequested",
                    command: "auditPrune",
                });
                const execution = command.find(
                    (effect) => effect.type === "executeCommand",
                );
                if (!execution) {
                    window.notifyApp(
                        command.find(
                            (effect) => effect.type === "commandRejected",
                        )?.reason || "Audit prune is unavailable.",
                    );
                    return;
                }
                try {
                    const res = await fetch("/api/audit-events/prune", {
                        method: "POST",
                    });
                    if (!lifecycle.isCurrent(lifetime)) return;
                    if (!res.ok)
                        throw new Error(
                            await parseErrorResponse(
                                res,
                                "Failed to prune audit events.",
                            ),
                        );
                    await settleCommand(
                        "commandCompleted",
                        execution.plan,
                        "Audit events pruned.",
                        { announce: false },
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                } catch (err) {
                    if (
                        !lifecycle.isCurrent(lifetime) ||
                        err?.name === "AbortError"
                    )
                        return;
                    await settleCommand(
                        "commandFailed",
                        execution.plan,
                        err.message || "Failed to prune audit events.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                }
            },
        );
        lifecycle.listen(
            document.querySelector("#audit-table tbody"),
            "click",
            (e) => {
                const button = e.target.closest("button[data-audit-detail]");
                if (!button) return;
                openAuditDetailDrawer(
                    auditEventByID(button.dataset.auditDetail),
                );
            },
        );

        function renderAccessPosture(server) {
            const authLabels = {
                password: ["Password", "pill-success"],
                "per-server-key": ["Per-server key", "pill-success"],
                "global-key": ["Global SSH Credential", "pill"],
                "per-server-key-and-password": [
                    "Per-server key + password",
                    "pill-warning",
                ],
                "global-key-and-password": [
                    "Global SSH Credential + password",
                    "pill-warning",
                ],
                missing: ["Missing credential", "pill-danger"],
                unknown: ["Authentication unknown", "pill-warning"],
            };
            const trustLabels = {
                trusted: "Host key trusted",
                missing: "Host key not trusted",
                changed: "Host key changed",
                unknown: "Trust not checked",
            };
            const [label, className] =
                authLabels[server.effectiveAuth] || authLabels.missing;
            const trust =
                trustLabels[server.host_key_status] || trustLabels.unknown;
            return `<span class="access-posture"><span class="pill ${className}">${label}</span><small>${escapeHtml(trust)}</small></span>`;
        }

        function renderTags(tags) {
            if (!tags || tags.length === 0) {
                return '<span class="pill pill-muted">None</span>';
            }
            return tags
                .map((tag) => `<span class="pill">${escapeHtml(tag)}</span>`)
                .join(" ");
        }

        function safeStatusClassToken(status) {
            const normalized = String(status || "unknown")
                .toLowerCase()
                .replace(/[^a-z0-9_-]/g, "-");
            switch (normalized) {
                case "success":
                case "failure":
                case "started":
                case "ignored":
                case "error":
                case "pending":
                case "unknown":
                    return normalized;
                default:
                    return "unknown";
            }
        }

        function renderAddAuthMethod() {
            const creation = managePageInteraction.getView().creation;
            document
                .querySelectorAll('input[name="add-auth-method"]')
                .forEach((input) => {
                    input.checked =
                        input.value === creation.authenticationMethod;
                    if (input.value === "global-key")
                        input.disabled = !creation.globalCredentialAvailable;
                });
            document.getElementById("add-password-field").hidden =
                !creation.passwordFieldVisible;
            document.getElementById("add-key-field").hidden =
                !creation.keyFieldVisible;
            document.getElementById("pass").required =
                creation.passwordFieldVisible;
            document.getElementById("global-auth-help").textContent =
                creation.globalCredentialAvailable
                    ? "Uses the configured Global SSH Credential."
                    : "Configure a Global SSH Credential to use this option.";
        }

        const addServerFieldIDs = {
            name: "name",
            host: "host",
            port: "port",
            user: "user",
            pass: "pass",
            key: "key_file",
            auth: "add-auth-methods",
        };

        function addServerField(field) {
            return document.getElementById(addServerFieldIDs[field]);
        }

        function setAddServerFieldInvalidState(field, invalid) {
            const input = addServerField(field);
            if (!input) return;
            input.classList.toggle("is-invalid", !!invalid);
            if (invalid) input.setAttribute("aria-invalid", "true");
            else input.removeAttribute("aria-invalid");
        }

        function clearAddServerValidation() {
            Object.keys(addServerFieldIDs).forEach((field) =>
                setAddServerFieldInvalidState(field, false),
            );
            document.getElementById("add-server-error").textContent = "";
        }

        function showAddServerValidation(rejection = {}) {
            clearAddServerValidation();
            const invalidFields = rejection.invalidFields || [];
            invalidFields.forEach((field) =>
                setAddServerFieldInvalidState(field, true),
            );
            document.getElementById("add-server-error").textContent =
                rejection.reason || "Server creation is unavailable.";
            const firstInvalid = addServerField(invalidFields[0]);
            if (firstInvalid?.matches("fieldset"))
                firstInvalid.querySelector("input:not(:disabled)")?.focus();
            else firstInvalid?.focus();
        }

        function clearEditedAddServerField(field) {
            setAddServerFieldInvalidState(field, false);
            const hasInvalidField = Object.keys(addServerFieldIDs).some((key) =>
                addServerField(key)?.hasAttribute("aria-invalid"),
            );
            if (!hasInvalidField)
                document.getElementById("add-server-error").textContent = "";
        }

        document
            .querySelectorAll('input[name="add-auth-method"]')
            .forEach((input) => {
                lifecycle.listen(input, "change", () => {
                    managePageInteraction.dispatch({
                        type: "creationAuthenticationChanged",
                        authenticationMethod: input.value,
                    });
                    for (const field of ["auth", "pass", "key"])
                        clearEditedAddServerField(field);
                    renderAddAuthMethod();
                });
            });
        for (const field of ["name", "host", "port", "user", "pass"]) {
            lifecycle.listen(addServerField(field), "input", () =>
                clearEditedAddServerField(field),
            );
        }
        lifecycle.listen(addServerField("key"), "change", () =>
            clearEditedAddServerField("key"),
        );

        const onboardingAdapter = window.ServerOnboarding.mount({
            store: managePageInteraction,
            clearValidation: clearAddServerValidation,
            showValidation: showAddServerValidation,
            settle: settleCommand,
            onSaved() {
                const keyFileInput = document.getElementById("key_file");
                keyFileInput.value = "";
                resetFileInputLabel(keyFileInput);
                document.getElementById(
                    "server-key-file-selection",
                ).textContent = "No file selected";
                managePageInteraction.dispatch({
                    type: "creationAuthenticationChanged",
                    authenticationMethod: "password",
                });
                renderAddAuthMethod();
            },
        });

        const importAdapter = window.InventoryImport.mount({
            store: managePageInteraction,
            workspaceChanged: renderManageWorkspace,
            refresh: executeManageEffects,
            editServer: async (name) => {
                const lifetime = lifecycle.capture();

                const before = managePageInteraction.getView();
                await fetchManageServers();
                if (!lifecycle.isCurrent(lifetime)) return;
                const current = managePageInteraction.getView();
                if (
                    current.editor.open ||
                    current.editor.sessionID !== before.editor.sessionID ||
                    current.inventoryImport.generation !==
                        before.inventoryImport.generation
                )
                    return;
                if (
                    !current.inventory.allItems.some(
                        (server) => server.name === name,
                    )
                ) {
                    window.notifyApp(
                        "This server is no longer in inventory. Refresh the directory.",
                    );
                    return;
                }
                await editServer(name);
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        });

        lifecycle.listen(document, "change", (e) => {
            if (e.target && e.target.classList.contains("file-input")) {
                resetFileInputLabel(e.target);
                const selectionIDs = {
                    "global-key-file": "global-key-file-selection",
                    key_file: "server-key-file-selection",
                    "edit-key": "edit-key-file-selection",
                };
                const selection = document.getElementById(
                    selectionIDs[e.target.id],
                );
                if (selection)
                    selection.textContent =
                        e.target.files?.[0]?.name || "No file selected";
                if (e.target.id === "edit-key") {
                    managePageInteraction.dispatch({
                        type: "editorCredentialIntentChanged",
                        keyReplacement: !!e.target.files?.length,
                    });
                    setEditSaveButtonState(false);
                }
            }
        });

        async function toggleServerAvailability(name) {
            const lifetime = lifecycle.capture();

            const execution = commandExecution("setServerDisabled", {
                serverName: name,
            });
            if (!execution) return;
            renderTable();
            try {
                const response = await fetch(
                    `/api/servers/${encodeURIComponent(name)}/availability`,
                    {
                        method: "PUT",
                        headers: { "Content-Type": "application/json" },
                        body: JSON.stringify({
                            disabled: execution.plan.payload.disabled,
                        }),
                    },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!response.ok)
                    throw new Error(
                        await parseErrorResponse(
                            response,
                            "Failed to change server availability.",
                        ),
                    );
                const message = execution.plan.payload.disabled
                    ? "Server disabled. Maintenance is paused."
                    : "Server enabled. Maintenance can resume.";
                await settleCommand(
                    "commandCompleted",
                    execution.plan,
                    message,
                );
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (error) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    error?.name === "AbortError"
                )
                    return;
                await settleCommand(
                    "commandFailed",
                    execution.plan,
                    error?.message || "Failed to change server availability.",
                );
                if (!lifecycle.isCurrent(lifetime)) return;
            } finally {
                if (!lifecycle.isCurrent(lifetime)) return;
                renderTable();
            }
        }

        async function deleteServer(name) {
            const lifetime = lifecycle.capture();

            if (
                await window.confirmTypedAction(
                    `Delete server "${name}"?`,
                    name,
                )
            ) {
                const execution = commandExecution("deleteServer", {
                    serverName: name,
                });
                if (!execution) {
                    window.notifyApp(
                        "This server action is already in progress.",
                    );
                    return;
                }
                try {
                    const response = await fetch(
                        `/api/servers/${encodeURIComponent(name)}`,
                        { method: "DELETE" },
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    if (!response.ok) {
                        throw new Error(
                            await parseErrorResponse(
                                response,
                                "Failed to delete server.",
                            ),
                        );
                    }
                    await settleCommand(
                        "commandCompleted",
                        execution.plan,
                        "Server deleted.",
                        { announce: false },
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                } catch (error) {
                    if (
                        !lifecycle.isCurrent(lifetime) ||
                        error?.name === "AbortError"
                    )
                        return;
                    await settleCommand(
                        "commandFailed",
                        execution.plan,
                        error?.message || "Failed to delete server.",
                        { announce: false },
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    window.notifyApp(
                        error?.message || "Failed to delete server.",
                    );
                }
            }
            if (!lifecycle.isCurrent(lifetime)) return;
        }

        const packageDeferralEditor = window.PackageDeferrals.mount(
            document.getElementById("server-package-deferrals"),
        );
        let applicationPostcheckPreviewController = null;
        async function loadApplicationPostcheckPreview(name) {
            const lifetime = lifecycle.capture();

            applicationPostcheckPreviewController?.abort();
            const controller = new AbortController();
            applicationPostcheckPreviewController = controller;
            const sessionID = managePageInteraction.getView().editor.sessionID;
            document.getElementById("edit-application-postchecks").textContent =
                "Loading saved expectations…";
            try {
                const response = await fetch(
                    `/api/servers/${encodeURIComponent(name)}/postchecks`,
                    { cache: "no-store", signal: controller.signal },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                const data = await response.json();
                if (!lifecycle.isCurrent(lifetime)) return;
                managePageInteraction.dispatch({
                    type: "applicationPostchecksReceived",
                    sessionID,
                    profile: response.ok ? data.profile : null,
                    error: response.ok
                        ? ""
                        : data.error || "Post-check profile unavailable.",
                });
            } catch (error) {
                if (!lifecycle.isCurrent(lifetime)) return;
                if (controller.signal.aborted) return;
                managePageInteraction.dispatch({
                    type: "applicationPostchecksReceived",
                    sessionID,
                    error: "Post-check profile unavailable.",
                });
            }
            const editor = managePageInteraction.getView().editor;
            if (!editor.open || editor.sessionID !== sessionID) return;
            const fact = editor.applicationPostchecks;
            document.getElementById("edit-application-postchecks").textContent =
                fact?.error ||
                (fact?.profile
                    ? `${fact.profile.name} · revision ${fact.profile.revision} · ${fact.profile.checks.map((c) => `${c.name} (${c.mode})`).join(", ")}`
                    : "No application profile assigned. Existing post-check settings apply.");
        }

        async function editServer(name) {
            const lifetime = lifecycle.capture();

            const current =
                managePageInteraction
                    .getView()
                    .inventory.allItems.find(
                        (server) => server.name === name,
                    ) || {};
            managePageInteraction.dispatch({
                type: "editorOpened",
                name,
                server: current,
            });
            void loadApplicationPostcheckPreview(name);
            void packageDeferralEditor.select({ server_name: name });
            document.getElementById("edit-name").value = current.name || name;
            document.getElementById("edit-host").value = current.host || "";
            document.getElementById("edit-port").value = current.port || "";
            document.getElementById("edit-user").value = current.user || "";
            document.getElementById("edit-tags").value = (
                current.tags || []
            ).join(", ");
            document.getElementById("edit-pass").value = "";
            renderCredentialOutcomes();
            const keyInput = document.getElementById("edit-key");
            if (keyInput) {
                keyInput.value = "";
                resetFileInputLabel(keyInput);
                document.getElementById("edit-key-file-selection").textContent =
                    "No file selected";
            }
            setEditHostKeyStatus("");
            clearEditValidationState();
            setEditSaveButtonState(false);
            renderEditKnownHostState("checking");
            const editModal = document.getElementById("edit-modal");
            editModal.classList.add("active");
            activateModalFocus(editModal, document.getElementById("edit-name"));
            checkEditKnownHostStatus();
            document.getElementById("edit-policy-overrides").innerHTML =
                '<div class="subtle">Loading matching policies...</div>';
            try {
                await managePolicyOverrides.fetchContext(name);
                if (!lifecycle.isCurrent(lifetime)) return;
                managePolicyOverrides.render(
                    document.getElementById("edit-policy-overrides"),
                );
                setEditSaveButtonState(false);
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                document.getElementById("edit-policy-overrides").innerHTML =
                    `<div class="subtle">${escapeHtml(err.message || "Failed to load scheduled policies.")}</div>`;
            }
        }

        function closeEditModal() {
            void packageDeferralEditor.clear();
            applicationPostcheckPreviewController?.abort();
            const editModal = document.getElementById("edit-modal");
            editModal.classList.remove("active");
            releaseModalFocus(editModal);
            setEditHostKeyStatus("");
            clearEditValidationState();
            setEditSaveButtonState(false);
            managePageInteraction.dispatch({ type: "editorClosed" });
            const overrides = document.getElementById("edit-policy-overrides");
            if (overrides) {
                overrides.innerHTML = "";
            }
        }

        async function requestCloseEditModal() {
            const lifetime = lifecycle.capture();

            const effects = managePageInteraction.dispatch({
                type: "editorCloseRequested",
            });
            const confirmation = effects.find(
                (effect) => effect.type === "confirmEditorDiscard",
            );
            if (confirmation) {
                const confirmed = await window.confirmAction(
                    "Discard unsaved server changes?",
                    { confirmLabel: "Discard changes" },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!confirmed) return;
            }
            closeEditModal();
        }

        function renderEditorFieldsFromView() {
            const draft = managePageInteraction.getView().editor.draft || {};
            document.getElementById("edit-name").value = draft.name || "";
            document.getElementById("edit-host").value = draft.host || "";
            document.getElementById("edit-port").value = draft.port || 22;
            document.getElementById("edit-user").value = draft.user || "";
            document.getElementById("edit-tags").value = (
                draft.tags || []
            ).join(", ");
            document.getElementById("edit-pass").value = "";
        }

        function setEditHostKeyStatus(message) {
            const el = document.getElementById("edit-hostkey-status");
            if (!el) return;
            el.textContent = String(message || "").trim();
        }

        function editKnownHostStateFromKey(hostKey) {
            if (hostKey?.alreadyTrusted) return "trusted";
            if (hostKey?.hostEntryExists) return "changed";
            if (hostKey?.checked || hostKey?.fingerprint) return "missing";
            return "stale";
        }

        function renderEditKnownHostState(state, hostKey = null) {
            const badge = document.getElementById("edit-known-host-state");
            const target = document.getElementById("edit-known-host-target");
            const fingerprint = document.getElementById(
                "edit-known-host-fingerprint",
            );
            const refreshButton = document.getElementById(
                "edit-check-known-host",
            );
            const trustButton = document.getElementById(
                "edit-trust-known-host",
            );
            const removeButton = document.getElementById(
                "edit-clear-known-host",
            );
            if (
                !badge ||
                !target ||
                !fingerprint ||
                !refreshButton ||
                !trustButton ||
                !removeButton
            )
                return;

            const labels = {
                checking: "Checking…",
                trusted: "Trusted",
                missing: "Not trusted",
                changed: "Key changed",
                stale: "Needs refresh",
                error: "Check failed",
            };
            const currentState = Object.hasOwn(labels, state) ? state : "error";
            const editor = managePageInteraction.getView().editor;
            const host = String(
                hostKey?.host || editor.draft?.host || "",
            ).trim();
            const port = normalizePort(hostKey?.port || editor.draft?.port, 22);
            const knownFingerprint = String(hostKey?.fingerprint || "").trim();

            badge.className = `known-host-state is-${currentState}`;
            badge.textContent = labels[currentState];
            target.textContent = host ? `${host}:${port}` : "Host required";
            fingerprint.textContent = knownFingerprint || "Not checked";

            const busy = currentState === "checking";
            refreshButton.disabled = busy;
            refreshButton.textContent = busy ? "Checking…" : "Refresh status";
            trustButton.hidden =
                currentState !== "missing" && currentState !== "changed";
            trustButton.disabled = busy;
            trustButton.textContent =
                currentState === "changed"
                    ? "Replace Host Key"
                    : "Trust Host Key";
            const canRemoveLocalEntry =
                !!host &&
                (currentState === "trusted" ||
                    currentState === "changed" ||
                    currentState === "error");
            removeButton.disabled = busy || !canRemoveLocalEntry;
            removeButton.textContent = "Remove Host Key";
        }

        function setEditValidationError(message) {
            const el = document.getElementById("edit-error");
            if (!el) return;
            el.textContent = String(message || "").trim();
        }

        function setEditFieldInvalidState(fieldId, isInvalid) {
            const input = document.getElementById(fieldId);
            if (!input) return;
            input.classList.toggle("is-invalid", !!isInvalid);
            if (isInvalid) {
                input.setAttribute("aria-invalid", "true");
            } else {
                input.removeAttribute("aria-invalid");
            }
        }

        function maybeClearEditValidationError() {
            const requiredFields = ["edit-name", "edit-host", "edit-user"];
            const hasInvalid = requiredFields.some((fieldId) => {
                const input = document.getElementById(fieldId);
                return !!input && input.classList.contains("is-invalid");
            });
            if (!hasInvalid) {
                setEditValidationError("");
            }
        }

        function clearEditValidationState() {
            setEditValidationError("");
            setEditFieldInvalidState("edit-name", false);
            setEditFieldInvalidState("edit-host", false);
            setEditFieldInvalidState("edit-port", false);
            setEditFieldInvalidState("edit-user", false);
        }

        function setEditSaveButtonState(isBusy, label) {
            const saveBtn = document.getElementById("edit-save");
            const cancelBtn = document.getElementById("edit-cancel");
            const closeBtn = document.getElementById("edit-close");
            const discardBtn = document.getElementById("edit-discard");
            const draftState = document.getElementById("edit-draft-state");
            if (!saveBtn) return;
            const editor = managePageInteraction.getView().editor;
            saveBtn.disabled = !!isBusy || !editor.canSave;
            saveBtn.textContent = isBusy ? label || "Saving..." : "Save Server";
            if (cancelBtn) {
                cancelBtn.disabled = !!isBusy;
            }
            if (closeBtn) closeBtn.disabled = !!isBusy;
            if (discardBtn) discardBtn.disabled = !!isBusy || !editor.dirty;
            if (draftState)
                draftState.textContent = editor.dirty
                    ? "Unsaved changes"
                    : "No unsaved changes";
        }

        async function checkEditKnownHostStatus() {
            const lifetime = lifecycle.capture();

            if (!activeEditorName()) return;
            const editor = managePageInteraction.getView().editor;
            const host = String(editor.draft?.host || "").trim();
            const port = normalizePort(editor.draft?.port, 22);
            if (!host) {
                setEditHostKeyStatus("Known host status: host is required.");
                return;
            }
            const request = managePageInteraction
                .dispatch({ type: "snapshotRequested", stream: "hostKey" })
                .find((effect) => effect.type === "fetchSnapshot");
            if (!request) return editKnownHostCheckPromise;
            const sessionID = managePageInteraction.getView().editor.sessionID;
            const currentCheck = (async () => {
                const lifetime = lifecycle.capture();

                renderEditKnownHostState("checking");
                setEditHostKeyStatus("Scanning the remote SSH host key…");
                try {
                    const scanned = await scanHostKey(host, port);
                    if (!lifecycle.isCurrent(lifetime)) return;
                    const currentEditor =
                        managePageInteraction.getView().editor;
                    if (
                        currentEditor.sessionID !== sessionID ||
                        String(currentEditor.draft?.host || "").trim() !==
                            host ||
                        normalizePort(currentEditor.draft?.port, 22) !== port
                    ) {
                        return;
                    }
                    const hostKey = {
                        host,
                        port,
                        checked: true,
                        fingerprint: scanned?.fingerprint_sha256 || "",
                        algorithm: scanned?.algorithm || "",
                        alreadyTrusted: !!scanned?.already_trusted,
                        hostEntryExists: !!scanned?.host_entry_exists,
                    };
                    managePageInteraction.dispatch({
                        type: "hostKeyReceived",
                        requestID: request.requestID,
                        sessionID,
                        host,
                        port,
                        hostKey,
                    });
                    const state = editKnownHostStateFromKey(hostKey);
                    renderEditKnownHostState(state, hostKey);
                    setEditHostKeyStatus(
                        state === "trusted"
                            ? "The scanned fingerprint matches the saved known_hosts entry."
                            : state === "changed"
                              ? "The remote fingerprint differs from the saved known_hosts entry. Verify it before replacing the key."
                              : "No known_hosts entry exists for this host and port.",
                    );
                } catch (err) {
                    if (
                        !lifecycle.isCurrent(lifetime) ||
                        err?.name === "AbortError"
                    )
                        return;
                    const currentEditor =
                        managePageInteraction.getView().editor;
                    if (
                        currentEditor.sessionID !== sessionID ||
                        String(currentEditor.draft?.host || "").trim() !==
                            host ||
                        normalizePort(currentEditor.draft?.port, 22) !== port
                    ) {
                        return;
                    }
                    managePageInteraction.dispatch({
                        type: "snapshotFailed",
                        stream: "hostKey",
                        requestID: request.requestID,
                        error: err?.message,
                    });
                    renderEditKnownHostState("error");
                    setEditHostKeyStatus(
                        `Unable to check the remote host key: ${err.message || "unknown error"}`,
                    );
                }
            })();
            editKnownHostCheckPromise = currentCheck;
            try {
                await currentCheck;
                if (!lifecycle.isCurrent(lifetime)) return;
            } finally {
                if (!lifecycle.isCurrent(lifetime)) return;
                if (editKnownHostCheckPromise === currentCheck) {
                    editKnownHostCheckPromise = null;
                }
            }
        }

        async function trustEditKnownHost() {
            const lifetime = lifecycle.capture();

            if (!activeEditorName()) return;
            const editor = managePageInteraction.getView().editor;
            const host = String(editor.draft?.host || "").trim();
            const port = normalizePort(editor.draft?.port, 22);
            if (!host) {
                window.notifyApp("Host is required.");
                return;
            }
            if (editKnownHostCheckPromise) await editKnownHostCheckPromise;
            if (!lifecycle.isCurrent(lifetime)) return;
            let currentKey = managePageInteraction.getView().editor.hostKey;
            if (
                !currentKey?.fingerprint ||
                currentKey.host !== host ||
                currentKey.port !== port
            ) {
                await checkEditKnownHostStatus();
                if (!lifecycle.isCurrent(lifetime)) return;
                currentKey = managePageInteraction.getView().editor.hostKey;
            }
            if (!currentKey?.fingerprint || currentKey.alreadyTrusted) {
                renderEditKnownHostState(
                    editKnownHostStateFromKey(currentKey),
                    currentKey,
                );
                return;
            }

            const scanned = {
                host,
                port,
                algorithm: currentKey.algorithm || "unknown",
                fingerprint_sha256: currentKey.fingerprint,
                already_trusted: !!currentKey.alreadyTrusted,
                host_entry_exists: !!currentKey.hostEntryExists,
            };
            if (!(await confirmHostKeyWithModal(scanned))) return;
            if (!lifecycle.isCurrent(lifetime)) return;

            const execution = commandExecution("trustHostKey", {
                serverName: activeEditorName(),
                host,
                port,
            });
            if (!execution) {
                window.notifyApp("Known host action is already in progress.");
                return;
            }
            renderEditKnownHostState("checking", currentKey);
            const replacing = !!currentKey.hostEntryExists;
            setEditHostKeyStatus(
                replacing
                    ? "Replacing the saved known_hosts entry…"
                    : "Saving the host key to known_hosts…",
            );
            try {
                const trusted = await trustHostKey(
                    host,
                    port,
                    currentKey.fingerprint,
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                const trustedKey = {
                    ...currentKey,
                    fingerprint:
                        trusted?.fingerprint_sha256 || currentKey.fingerprint,
                    alreadyTrusted: true,
                    hostEntryExists: true,
                };
                managePageInteraction.dispatch({
                    type: "hostKeyReceived",
                    sessionID: editor.sessionID,
                    host,
                    port,
                    hostKey: trustedKey,
                });
                await settleCommand(
                    "commandCompleted",
                    execution.plan,
                    replacing
                        ? "Known host key replaced."
                        : "Known host key trusted.",
                    { announce: false },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                renderEditKnownHostState("trusted", trustedKey);
                setEditHostKeyStatus(
                    "The scanned fingerprint matches the saved known_hosts entry.",
                );
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                await settleCommand(
                    "commandFailed",
                    execution.plan,
                    err.message || "Failed to trust host key.",
                    { announce: false },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                renderEditKnownHostState("error", currentKey);
                setEditHostKeyStatus(
                    `Unable to save the host key: ${err.message || "unknown error"}`,
                );
                window.notifyApp(err.message || "Failed to trust host key.");
            }
        }

        async function clearEditKnownHost() {
            const lifetime = lifecycle.capture();

            if (!activeEditorName()) return;
            const editor = managePageInteraction.getView().editor;
            const host = String(editor.draft?.host || "").trim();
            const port = normalizePort(editor.draft?.port, 22);
            if (!host) {
                window.notifyApp("Host is required.");
                return;
            }
            if (
                !(await window.confirmTypedAction(
                    `Remove known_hosts entry for ${host}:${port}?`,
                    `${host}:${port}`,
                ))
            ) {
                return;
            }
            if (!lifecycle.isCurrent(lifetime)) return;
            const execution = commandExecution("clearHostKey", {
                serverName: activeEditorName(),
                host,
                port,
            });
            if (!execution) {
                window.notifyApp("Known host action is already in progress.");
                return;
            }
            const currentKey = managePageInteraction.getView().editor.hostKey;
            renderEditKnownHostState("checking", currentKey);
            setEditHostKeyStatus("Removing the known_hosts entry…");
            try {
                const result = await clearKnownHost(host, port);
                if (!lifecycle.isCurrent(lifetime)) return;
                const clearedKey = {
                    ...(currentKey || {}),
                    host,
                    port,
                    checked: true,
                    alreadyTrusted: false,
                    hostEntryExists: false,
                };
                managePageInteraction.dispatch({
                    type: "hostKeyReceived",
                    sessionID: managePageInteraction.getView().editor.sessionID,
                    host,
                    port,
                    hostKey: clearedKey,
                });
                await settleCommand(
                    "commandCompleted",
                    execution.plan,
                    "Known host entry cleared.",
                    { announce: false },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                renderEditKnownHostState("missing", clearedKey);
                if (Number(result?.removed_entries || 0) > 0) {
                    setEditHostKeyStatus("The known_hosts entry was removed.");
                } else {
                    setEditHostKeyStatus(
                        "No known_hosts entry existed for this host and port.",
                    );
                }
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                await settleCommand(
                    "commandFailed",
                    execution.plan,
                    err.message || "Failed to clear known host entry.",
                    { announce: false },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                renderEditKnownHostState("error", currentKey);
                setEditHostKeyStatus(
                    `Unable to remove the known_hosts entry: ${err.message || "unknown error"}`,
                );
                window.notifyApp(
                    err.message || "Failed to clear known host entry.",
                );
            }
        }

        async function saveServerEdit() {
            const lifetime = lifecycle.capture();

            const originalName = activeEditorName();
            if (!originalName) return;
            const newPass = document.getElementById("edit-pass").value;
            clearEditValidationState();
            const command = managePageInteraction.dispatch({
                type: "commandRequested",
                command: "saveEditor",
            });
            const execution = command.find(
                (effect) => effect.type === "executeCommand",
            );
            if (!execution) {
                const rejected =
                    command.find(
                        (effect) => effect.type === "commandRejected",
                    ) || {};
                const fieldIDs = {
                    name: "edit-name",
                    host: "edit-host",
                    port: "edit-port",
                    user: "edit-user",
                };
                for (const field of rejected.invalidFields || []) {
                    setEditFieldInvalidState(fieldIDs[field], true);
                }
                setEditValidationError(
                    rejected.reason ||
                        "This server action is already in progress.",
                );
                const firstInvalid = document.getElementById(
                    fieldIDs[(rejected.invalidFields || [])[0]],
                );
                if (firstInvalid) firstInvalid.focus();
                return;
            }
            setEditSaveButtonState(true, "Saving...");
            try {
                const accepted = execution.plan.payload;
                const res = await fetch(
                    `/api/servers/${encodeURIComponent(originalName)}`,
                    {
                        method: "PUT",
                        headers: { "Content-Type": "application/json" },
                        body: JSON.stringify({
                            name: accepted.name,
                            host: accepted.host,
                            port: accepted.port,
                            user: accepted.user,
                            pass: newPass,
                            tags: accepted.tags,
                        }),
                    },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok)
                    throw new Error(
                        await parseErrorResponse(res, "Failed to save server."),
                    );
                managePageInteraction.dispatch({
                    type: "editorServerAccepted",
                    sessionID: accepted.sessionID,
                    server: accepted,
                });
                document.getElementById("edit-pass").value = "";
                const replacementKey = document.getElementById("edit-key");
                if (accepted.keyReplacement && replacementKey?.files?.length) {
                    let keyUploadError = "";
                    try {
                        const keyForm = new FormData();
                        keyForm.append("key", replacementKey.files[0]);
                        const keyResponse = await fetch(
                            `/api/servers/${encodeURIComponent(accepted.name)}/key`,
                            {
                                method: "POST",
                                body: keyForm,
                            },
                        );
                        if (!lifecycle.isCurrent(lifetime)) return;
                        if (!keyResponse.ok) {
                            keyUploadError = await parseErrorResponse(
                                keyResponse,
                                "replacement key rejected",
                            );
                            if (!lifecycle.isCurrent(lifetime)) return;
                        }
                    } catch (err) {
                        if (
                            !lifecycle.isCurrent(lifetime) ||
                            err?.name === "AbortError"
                        )
                            return;
                        keyUploadError = err?.message || "unknown error";
                    }
                    if (keyUploadError) {
                        const message = `Server saved, but the replacement SSH key failed to upload: ${keyUploadError}`;
                        await settleCommand(
                            "commandPartiallyCompleted",
                            execution.plan,
                            message,
                            { announce: false },
                        );
                        if (!lifecycle.isCurrent(lifetime)) return;
                        window.notifyApp(message);
                        return;
                    }
                    replacementKey.value = "";
                    resetFileInputLabel(replacementKey);
                    document.getElementById(
                        "edit-key-file-selection",
                    ).textContent = "No file selected";
                    managePageInteraction.dispatch({
                        type: "editorCredentialIntentChanged",
                        keyReplacement: false,
                    });
                }
                let overrideSaveError = null;
                try {
                    const outcome = await managePolicyOverrides.save(
                        accepted.name,
                        accepted.policyOverrides,
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    if (
                        outcome.status === "partial" ||
                        outcome.status === "failure"
                    ) {
                        const failures = outcome.failures
                            .map(
                                (failure) =>
                                    `${failure.policyID}: ${failure.error}`,
                            )
                            .join("; ");
                        throw new Error(
                            `Failed to save scheduled update overrides for policy IDs ${failures}`,
                        );
                    }
                } catch (err) {
                    if (
                        !lifecycle.isCurrent(lifetime) ||
                        err?.name === "AbortError"
                    )
                        return;
                    overrideSaveError = err;
                }
                if (overrideSaveError) {
                    const message = `Server saved, but scheduled update overrides were not fully saved: ${overrideSaveError?.message || "unknown error"}`;
                    await settleCommand(
                        "commandPartiallyCompleted",
                        execution.plan,
                        message,
                        { announce: false },
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    window.notifyApp(message);
                    return;
                }
                await settleCommand(
                    "commandCompleted",
                    execution.plan,
                    "Server saved.",
                    { announce: false },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                closeEditModal();
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                await settleCommand(
                    "commandFailed",
                    execution.plan,
                    err?.message || "Failed to save server.",
                    { announce: false },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                window.notifyApp(err?.message || "Failed to save server.");
                setEditHostKeyStatus("");
            } finally {
                if (!lifecycle.isCurrent(lifetime)) return;
                setEditSaveButtonState(false);
            }
        }

        async function uploadServerKey(name) {
            const lifetime = lifecycle.capture();

            const input = document.getElementById("edit-key");
            if (!input || !input.files || input.files.length === 0) {
                window.notifyApp("Select a private key file to upload.");
                return;
            }
            const execution = commandExecution("uploadServerKey", {
                serverName: name,
            });
            if (!execution) {
                window.notifyApp("This server action is already in progress.");
                return;
            }
            const form = new FormData();
            form.append("key", input.files[0]);
            try {
                const res = await fetch(
                    `/api/servers/${encodeURIComponent(name)}/key`,
                    { method: "POST", body: form },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok)
                    throw new Error(
                        (await res.json().catch(() => ({}))).error ||
                            "Failed to upload key.",
                    );
                await settleCommand(
                    "commandCompleted",
                    execution.plan,
                    "Server key uploaded.",
                    { announce: false },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                input.value = "";
                resetFileInputLabel(input);
                document.getElementById("edit-key-file-selection").textContent =
                    "No file selected";
                managePageInteraction.dispatch({
                    type: "editorCredentialIntentChanged",
                    keyReplacement: false,
                });
                setEditSaveButtonState(false);
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                await settleCommand(
                    "commandFailed",
                    execution.plan,
                    err?.message || "Failed to upload key.",
                    { announce: false },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                window.notifyApp(err?.message || "Failed to upload key.");
            }
        }

        async function clearServerKey(name) {
            const lifetime = lifecycle.capture();

            const outcome = effectiveAuthLabel(
                managePageInteraction.getView().editor.credentialOutcomes
                    ?.clearKey,
            );
            if (
                !(await window.confirmAction(
                    `Clear this server SSH key? Effective authentication will become ${outcome}.`,
                    { confirmLabel: "Clear key" },
                ))
            )
                return;
            if (!lifecycle.isCurrent(lifetime)) return;
            const execution = commandExecution("clearServerKey", {
                serverName: name,
            });
            if (!execution) {
                window.notifyApp("This server action is already in progress.");
                return;
            }
            try {
                const res = await fetch(
                    `/api/servers/${encodeURIComponent(name)}/key`,
                    { method: "DELETE" },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok)
                    throw new Error(
                        (await res.json().catch(() => ({}))).error ||
                            "Failed to clear key.",
                    );
                await settleCommand(
                    "commandCompleted",
                    execution.plan,
                    "Server key cleared.",
                    { announce: false },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                await settleCommand(
                    "commandFailed",
                    execution.plan,
                    err?.message || "Failed to clear key.",
                    { announce: false },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                window.notifyApp(err?.message || "Failed to clear key.");
            }
        }

        async function clearServerPassword(name) {
            const lifetime = lifecycle.capture();

            const outcome = effectiveAuthLabel(
                managePageInteraction.getView().editor.credentialOutcomes
                    ?.clearPassword,
            );
            if (
                !(await window.confirmAction(
                    `Clear this server password? Effective authentication will become ${outcome}.`,
                    { confirmLabel: "Clear password" },
                ))
            )
                return;
            if (!lifecycle.isCurrent(lifetime)) return;
            const execution = commandExecution("clearServerPassword", {
                serverName: name,
            });
            if (!execution) {
                window.notifyApp("This server action is already in progress.");
                return;
            }
            try {
                const res = await fetch(
                    `/api/servers/${encodeURIComponent(name)}/password`,
                    { method: "DELETE" },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok)
                    throw new Error(
                        (await res.json().catch(() => ({}))).error ||
                            "Failed to clear password.",
                    );
                await settleCommand(
                    "commandCompleted",
                    execution.plan,
                    "Server password cleared.",
                    { announce: false },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                await settleCommand(
                    "commandFailed",
                    execution.plan,
                    err?.message || "Failed to clear password.",
                    { announce: false },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                window.notifyApp(err?.message || "Failed to clear password.");
            }
        }

        lifecycle.listen(
            document.getElementById("edit-cancel"),
            "click",
            requestCloseEditModal,
        );
        lifecycle.listen(
            document.getElementById("edit-close"),
            "click",
            requestCloseEditModal,
        );
        lifecycle.listen(
            document.getElementById("edit-discard"),
            "click",
            () => {
                managePageInteraction.dispatch({ type: "editorDiscarded" });
                renderEditorFieldsFromView();
                const keyInput = document.getElementById("edit-key");
                keyInput.value = "";
                resetFileInputLabel(keyInput);
                document.getElementById("edit-key-file-selection").textContent =
                    "No file selected";
                clearEditValidationState();
                renderEditKnownHostState("stale");
                setEditHostKeyStatus(
                    "Draft restored. Refresh host trust if needed.",
                );
                managePolicyOverrides.render(
                    document.getElementById("edit-policy-overrides"),
                );
                renderCredentialOutcomes();
                setEditSaveButtonState(false);
            },
        );
        lifecycle.listen(
            document.getElementById("edit-save"),
            "click",
            saveServerEdit,
        );
        lifecycle.listen(
            document.getElementById("edit-upload-key"),
            "click",
            () => {
                if (activeEditorName()) {
                    uploadServerKey(activeEditorName());
                }
            },
        );
        lifecycle.listen(
            document.getElementById("edit-clear-key"),
            "click",
            () => {
                if (activeEditorName()) {
                    clearServerKey(activeEditorName());
                }
            },
        );
        lifecycle.listen(
            document.getElementById("edit-clear-password"),
            "click",
            () => {
                if (activeEditorName()) {
                    clearServerPassword(activeEditorName());
                }
            },
        );
        lifecycle.listen(document.getElementById("edit-name"), "input", () => {
            managePageInteraction.dispatch({
                type: "editorChanged",
                patch: { name: document.getElementById("edit-name").value },
            });
            setEditFieldInvalidState("edit-name", false);
            maybeClearEditValidationError();
            setEditSaveButtonState(false);
        });
        lifecycle.listen(document.getElementById("edit-host"), "input", () => {
            managePageInteraction.dispatch({
                type: "editorChanged",
                patch: { host: document.getElementById("edit-host").value },
            });
            setEditFieldInvalidState("edit-host", false);
            maybeClearEditValidationError();
            if (activeEditorName()) {
                editKnownHostCheckPromise = null;
                renderEditKnownHostState("stale");
                setEditHostKeyStatus(
                    "Host or port changed. Refresh the status before trusting this host key.",
                );
            }
            setEditSaveButtonState(false);
        });
        lifecycle.listen(document.getElementById("edit-port"), "input", () => {
            managePageInteraction.dispatch({
                type: "editorChanged",
                patch: { port: document.getElementById("edit-port").value },
            });
            setEditFieldInvalidState("edit-port", false);
            maybeClearEditValidationError();
            if (activeEditorName()) {
                editKnownHostCheckPromise = null;
                renderEditKnownHostState("stale");
                setEditHostKeyStatus(
                    "Host or port changed. Refresh the status before trusting this host key.",
                );
            }
            setEditSaveButtonState(false);
        });
        lifecycle.listen(document.getElementById("edit-tags"), "input", () => {
            managePageInteraction.dispatch({
                type: "editorChanged",
                patch: { tags: document.getElementById("edit-tags").value },
            });
            if (activeEditorName()) {
                managePolicyOverrides.render(
                    document.getElementById("edit-policy-overrides"),
                );
            }
            setEditSaveButtonState(false);
        });
        lifecycle.listen(
            document.getElementById("edit-policy-overrides"),
            "change",
            (event) => {
                const checkbox = event.target.closest("input[data-policy-id]");
                if (checkbox) {
                    managePolicyOverrides.change(
                        checkbox.dataset.policyId,
                        checkbox.checked,
                    );
                    setEditSaveButtonState(false);
                }
            },
        );
        lifecycle.listen(document.getElementById("edit-user"), "input", () => {
            managePageInteraction.dispatch({
                type: "editorChanged",
                patch: { user: document.getElementById("edit-user").value },
            });
            setEditFieldInvalidState("edit-user", false);
            maybeClearEditValidationError();
            setEditSaveButtonState(false);
        });
        lifecycle.listen(document.getElementById("edit-pass"), "input", () => {
            managePageInteraction.dispatch({
                type: "editorChanged",
                patch: {
                    passwordReplacement:
                        !!document.getElementById("edit-pass").value,
                },
            });
            setEditSaveButtonState(false);
        });
        lifecycle.listen(
            document.getElementById("edit-check-known-host"),
            "click",
            () => {
                if (activeEditorName()) {
                    checkEditKnownHostStatus();
                }
            },
        );
        lifecycle.listen(
            document.getElementById("edit-trust-known-host"),
            "click",
            () => {
                if (activeEditorName()) {
                    trustEditKnownHost();
                }
            },
        );
        lifecycle.listen(
            document.getElementById("edit-clear-known-host"),
            "click",
            () => {
                if (activeEditorName()) {
                    clearEditKnownHost();
                }
            },
        );
        lifecycle.listen(
            document.getElementById("hostkey-modal-cancel"),
            "click",
            () => closeHostKeyModal(false),
        );
        lifecycle.listen(
            document.getElementById("hostkey-modal-trust"),
            "click",
            () => closeHostKeyModal(true),
        );
        lifecycle.listen(
            document.getElementById("audit-detail-copy"),
            "click",
            copyAuditDetail,
        );
        lifecycle.listen(
            document.getElementById("audit-detail-close"),
            "click",
            closeAuditDetailDrawer,
        );
        lifecycle.listen(
            document.getElementById("audit-detail-modal"),
            "click",
            (e) => {
                if (e.target && e.target.id === "audit-detail-modal") {
                    closeAuditDetailDrawer();
                }
            },
        );
        lifecycle.listen(
            document.getElementById("hostkey-modal"),
            "click",
            (e) => {
                if (e.target && e.target.id === "hostkey-modal") {
                    closeHostKeyModal(false);
                }
            },
        );
        lifecycle.listen(document, "keydown", (e) => {
            if (e.key === "Tab" && trapActiveModalFocus(e)) {
                return;
            }
            if (e.key === "Escape") {
                const hostKeyModal = document.getElementById("hostkey-modal");
                if (hostKeyModal && hostKeyModal.classList.contains("active")) {
                    closeHostKeyModal(false);
                    return;
                }
                const auditDetailModal =
                    document.getElementById("audit-detail-modal");
                if (
                    auditDetailModal &&
                    auditDetailModal.classList.contains("active")
                ) {
                    closeAuditDetailDrawer();
                    return;
                }
                const editModal = document.getElementById("edit-modal");
                if (editModal && editModal.classList.contains("active")) {
                    if (
                        managePageInteraction
                            .getView()
                            .commands.inFlight.some((key) =>
                                key.startsWith("saveEditor:"),
                            )
                    ) {
                        return;
                    }
                    void requestCloseEditModal();
                }
            }
        });

        async function uploadGlobalKey() {
            const lifetime = lifecycle.capture();

            const input = document.getElementById("global-key-file");
            const feedback = document.getElementById("global-key-feedback");
            const uploadButton = document.getElementById(
                "upload-global-key-btn",
            );
            if (!input || !input.files || input.files.length === 0) {
                if (feedback)
                    feedback.textContent =
                        "Select a private key file to upload.";
                return;
            }
            const command = managePageInteraction.dispatch({
                type: "commandRequested",
                command: "globalKeyUpload",
            });
            const execution = command.find(
                (effect) => effect.type === "executeCommand",
            );
            if (!execution) {
                window.notifyApp(
                    "Global SSH Credential action is already in progress.",
                );
                return;
            }
            const form = new FormData();
            form.append("key", input.files[0]);
            const previousLabel =
                uploadButton?.textContent || "Add Global SSH Credential";
            if (feedback)
                feedback.textContent = "Uploading Global SSH Credential…";
            if (uploadButton) {
                uploadButton.disabled = true;
                uploadButton.textContent = "Uploading…";
            }
            try {
                const res = await fetch("/api/keys/global", {
                    method: "POST",
                    body: form,
                });
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok)
                    throw new Error(
                        await parseErrorResponse(
                            res,
                            "Failed to upload Global SSH Credential.",
                        ),
                    );
                await settleCommand(
                    "commandCompleted",
                    execution.plan,
                    "Global SSH Credential saved.",
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (feedback)
                    feedback.textContent = "Global SSH Credential saved.";
                input.value = "";
                resetFileInputLabel(input);
                document.getElementById(
                    "global-key-file-selection",
                ).textContent = "No file selected";
            } catch (error) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    error?.name === "AbortError"
                )
                    return;
                const message =
                    error?.message || "Failed to upload Global SSH Credential.";
                await settleCommand("commandFailed", execution.plan, message);
                if (!lifecycle.isCurrent(lifetime)) return;
                if (feedback) feedback.textContent = message;
            } finally {
                if (!lifecycle.isCurrent(lifetime)) return;
                if (uploadButton && uploadButton.textContent === "Uploading…") {
                    uploadButton.textContent = previousLabel;
                    uploadButton.disabled = false;
                }
            }
        }

        async function clearGlobalKey() {
            const lifetime = lifecycle.capture();

            if (
                !(await window.confirmTypedAction(
                    "Clear the Global SSH Credential?",
                    "CLEAR GLOBAL SSH CREDENTIAL",
                ))
            ) {
                return;
            }
            if (!lifecycle.isCurrent(lifetime)) return;
            const command = managePageInteraction.dispatch({
                type: "commandRequested",
                command: "globalKeyClear",
            });
            const execution = command.find(
                (effect) => effect.type === "executeCommand",
            );
            if (!execution) {
                window.notifyApp(
                    "Global SSH Credential action is already in progress.",
                );
                return;
            }
            const res = await fetch("/api/keys/global", { method: "DELETE" });
            if (!lifecycle.isCurrent(lifetime)) return;
            if (!res.ok) {
                const data = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                await settleCommand(
                    "commandFailed",
                    execution.plan,
                    data.error || "Failed to clear Global SSH Credential.",
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                return;
            }
            await settleCommand(
                "commandCompleted",
                execution.plan,
                "Global SSH Credential cleared.",
            );
            if (!lifecycle.isCurrent(lifetime)) return;
        }

        function renderGlobalKeyState(state, detail = "") {
            const status = document.getElementById("global-key-status");
            const uploadButton = document.getElementById(
                "upload-global-key-btn",
            );
            const clearButton = document.getElementById("clear-global-key-btn");
            if (!status || !uploadButton || !clearButton) return;

            const states = {
                checking: {
                    label: "Checking…",
                    className: "is-checking",
                    hasKey: null,
                },
                configured: {
                    label: "Configured",
                    className: "is-configured",
                    hasKey: true,
                },
                missing: {
                    label: "Not configured",
                    className: "is-missing",
                    hasKey: false,
                },
                error: { label: "Error", className: "is-error", hasKey: null },
            };
            const current = states[state] || states.error;
            status.className = `global-key-state ${current.className}`;
            status.textContent = current.label;
            status.title = detail || current.label;

            uploadButton.textContent = current.hasKey
                ? "Replace Global SSH Credential"
                : "Add Global SSH Credential";
            uploadButton.disabled = state === "checking";
            clearButton.disabled = current.hasKey !== true;
            clearButton.title = current.hasKey
                ? "Remove the configured Global SSH Credential"
                : "No Global SSH Credential is configured";
            renderManageWorkspace();
        }

        async function performGlobalKeyRequest(request) {
            const lifetime = lifecycle.capture();

            try {
                const res = await fetch("/api/keys/global");
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok)
                    throw new Error(await parseErrorResponse(res, "unknown"));
                const data = await res.json();
                if (!lifecycle.isCurrent(lifetime)) return;
                const effects = managePageInteraction.dispatch({
                    type: "globalKeySnapshotReceived",
                    requestID: request.requestID,
                    hasKey: !!data.has_key,
                });
                renderGlobalKeyState(
                    managePageInteraction.getView().globalKeyAvailable
                        ? "configured"
                        : "missing",
                );
                if (
                    managePageInteraction.getView().inventory.allItems.length >
                    0
                )
                    renderTable();
                const followup = effects.find(
                    (effect) =>
                        effect.type === "fetchSnapshot" &&
                        effect.stream === "globalKey",
                );
                if (followup) await performGlobalKeyRequest(followup);
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                const effects = managePageInteraction.dispatch({
                    type: "snapshotFailed",
                    stream: "globalKey",
                    requestID: request.requestID,
                    error: err.message || "unknown",
                });
                renderGlobalKeyState(
                    "error",
                    err.message ||
                        "Unable to check the Global SSH Credential status",
                );
                const followup = effects.find(
                    (effect) =>
                        effect.type === "fetchSnapshot" &&
                        effect.stream === "globalKey",
                );
                if (followup) await performGlobalKeyRequest(followup);
                if (!lifecycle.isCurrent(lifetime)) return;
            }
        }

        async function fetchGlobalKeyStatus() {
            const lifetime = lifecycle.capture();

            const request = managePageInteraction
                .dispatch({ type: "snapshotRequested", stream: "globalKey" })
                .find((effect) => effect.type === "fetchSnapshot");
            if (!request) return;
            renderGlobalKeyState("checking");
            await performGlobalKeyRequest(request);
            if (!lifecycle.isCurrent(lifetime)) return;
        }

        lifecycle.listen(document.getElementById("logout-btn"), "click", () =>
            window.logout(),
        );
        lifecycle.listen(
            document.getElementById("upload-global-key-btn"),
            "click",
            uploadGlobalKey,
        );
        lifecycle.listen(
            document.getElementById("clear-global-key-btn"),
            "click",
            clearGlobalKey,
        );
        const manageDeepLink = new URLSearchParams(window.location.search);
        const selectedServer = manageDeepLink.get("server") || "";
        document.getElementById("search").value = selectedServer;
        managePageInteraction.dispatch({
            type: "filtersChanged",
            patch: { search: selectedServer },
        });
        document.getElementById("audit-target-filter").value =
            manageDeepLink.get("audit_target") || "";
        document.getElementById("audit-action-filter").value =
            manageDeepLink.get("audit_action") || "";
        document.getElementById("audit-status-filter").value =
            manageDeepLink.get("audit_status") || "";
        hydrateAuditDateTimeInput(
            document.getElementById("audit-from-filter"),
            manageDeepLink.get("audit_from"),
        );
        hydrateAuditDateTimeInput(
            document.getElementById("audit-to-filter"),
            manageDeepLink.get("audit_to"),
        );
        managePageInteraction.dispatch({
            type: "auditQueryChanged",
            patch: { failureCause: manageDeepLink.get("failure_cause") || "" },
        });
        renderAuditFailureCauseFilter();
        initializeManageWorkspace();
        renderAddAuthMethod();
        fetchManageServers();
        fetchGlobalKeyStatus();
        fetchAuditEvents();
        lifecycle.setInterval(fetchAuditEvents, 15000);

        lifecycle.own(packageDeferralEditor.destroy);
        lifecycle.own(onboardingAdapter.destroy);
        lifecycle.own(importAdapter.destroy);
        lifecycle.own(() => {
            closeHostKeyModal(false);
            closeEditModal();
            closeAuditDetailDrawer();
        });
        // Settle outstanding confirmation promises before replacing this page.
        lifecycle.own(() => {
            for (const name of ["action-confirm", "typed-confirm"]) {
                if (
                    document
                        .getElementById(`${name}-modal`)
                        ?.classList.contains("active")
                )
                    document.getElementById(`${name}-cancel`)?.click();
            }
        });
        const destroy = () => lifecycle.destroy();
        lifecycle.listen(root, "pagehide", (event) => {
            // A cached document keeps its one mounted interaction owner.
            if (!event.persisted) destroy();
        });
        current = Object.freeze({
            destroy,
            getView: () => managePageInteraction.getView(),
        });
        return current;
    }
    root.ManagePage = Object.freeze({
        mount,
        destroy: () => current?.destroy(),
        getView: () => current?.getView(),
    });
    mount();
})(globalThis);
