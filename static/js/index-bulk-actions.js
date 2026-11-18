// Bulk transport and controls execute Status Page Interaction command plans.
(function (root) {
    function create(options) {
        const lifecycle = root.BrowserEffects.create();
        const fetch = lifecycle.fetch;
        const {
            getStatusView,
            statusInteraction,
            pluralize,
            updateRefreshAllFactsState,
            scheduleSelectPageStateUpdate,
            dispatchStatusInteraction,
            escapeHtml,
            renderHTML,
            reviewKeptBackApproval,
        } = options;
        function bulkDashboardActionKey(actionPath) {
            if (actionPath === "update") return "update";
            if (actionPath === "approve") return "approve_all";
            if (actionPath === "approve-security") return "approve_security";
            if (actionPath === "approve-security-kept-back")
                return "approve_security_kept_back";
            if (actionPath === "cancel") return "cancel";
            if (actionPath === "autoremove") return "autoremove";
            if (actionPath === "facts-refresh") return "refresh_facts";
            return "";
        }
        function setBulkButtonState(
            id,
            label,
            count,
            enabled,
            enabledTitle,
            disabledTitle,
        ) {
            const button = document.getElementById(id);
            if (!button) return;
            button.disabled = !enabled;
            button.textContent = `${label} (${count})`;
            button.title = enabled ? enabledTitle : disabledTitle;
            button.setAttribute("aria-describedby", "bulk-action-hint");
        }
        function updateBulkActionState() {
            const hint = document.getElementById("bulk-action-hint");
            const view = getStatusView();
            const previewPlan = (actionKey) =>
                statusInteraction.planBulkAction(actionKey, { preview: true });
            const updatePlan = previewPlan("update");
            const approvePlan = previewPlan("approve_all");
            const approveSecurityPlan = previewPlan("approve_security");
            const approveKeptSecurityPlan = previewPlan(
                "approve_security_kept_back",
            );
            const cancelPlan = previewPlan("cancel");
            const autoremovePlan = previewPlan("autoremove");
            const selectedCount = view.selectedNames.length;
            const visibleCount = view.visibleSelectedNames.length;
            const hiddenCount = view.hiddenSelectedNames.length;
            const updateCount = updatePlan.eligibleNames.length;
            const approveCount = approvePlan.eligibleNames.length;
            const approveSecurityCount =
                approveSecurityPlan.eligibleNames.length;
            const approveKeptSecurityCount =
                approveKeptSecurityPlan.eligibleNames.length;
            const removalReviews = approveKeptSecurityPlan.removalReviews;
            const cancelCount = cancelPlan.eligibleNames.length;
            const autoremoveCount = autoremovePlan.eligibleNames.length;
            const bulk = view.actions.bulk;
            const lastBulkResult = view.actions.lastBulkResult;
            document
                .getElementById("bulk-action-controls")
                ?.classList.toggle("is-empty", selectedCount === 0 && !bulk);

            if (hint) {
                if (bulk) {
                    hint.textContent = `${bulk.selectedCount} selected · ${bulk.eligibleCount} executing · ${bulk.skippedCount} skipped`;
                    hint.classList.toggle("warning", bulk.skippedCount > 0);
                } else if (lastBulkResult) {
                    const parts = [
                        `${lastBulkResult.selectedCount} selected`,
                        `${lastBulkResult.executedCount} executed`,
                        `${lastBulkResult.skippedCount} skipped`,
                    ];
                    if (lastBulkResult.failedCount > 0)
                        parts.push(`${lastBulkResult.failedCount} failed`);
                    hint.textContent = parts.join(" · ");
                    hint.classList.toggle(
                        "warning",
                        lastBulkResult.skippedCount > 0 ||
                            lastBulkResult.failedCount > 0,
                    );
                } else if (selectedCount === 0) {
                    hint.textContent = "No hosts selected";
                    hint.classList.remove("warning");
                } else if (visibleCount === 0) {
                    hint.textContent = `${pluralize(selectedCount, "host")} selected · 0 visible in current filter`;
                    hint.classList.add("warning");
                } else {
                    const parts = [
                        `${selectedCount} selected`,
                        `${visibleCount} visible`,
                    ];
                    if (hiddenCount > 0)
                        parts.push(`${hiddenCount} outside current view`);
                    hint.textContent = parts.join(" · ");
                    hint.classList.toggle("warning", hiddenCount > 0);
                }
            }

            const bulkDisabledTitle = bulk
                ? `Bulk ${bulk.actionLabel} is already running`
                : null;
            const displayCount = (actionKey, count) =>
                bulk?.actionKey === actionKey ? bulk.eligibleCount : count;
            setBulkButtonState(
                "bulk-update",
                "Update",
                displayCount("update", updateCount),
                !bulk && updateCount > 0,
                `Update ${pluralize(updateCount, "visible selected host")}`,
                bulkDisabledTitle ||
                    (selectedCount === 0
                        ? "Select visible hosts first"
                        : "No selected host can run update checks"),
            );
            setBulkButtonState(
                "bulk-approve",
                "Approve standard",
                displayCount("approve_all", approveCount),
                !bulk && approveCount > 0,
                `Approve standard updates on ${pluralize(approveCount, "visible selected host")}`,
                bulkDisabledTitle ||
                    (selectedCount === 0
                        ? "Select visible hosts first"
                        : "No selected host has standard updates eligible for approval"),
            );
            setBulkButtonState(
                "bulk-approve-security",
                "Approve standard security",
                displayCount("approve_security", approveSecurityCount),
                !bulk && approveSecurityCount > 0,
                `Approve standard security updates on ${pluralize(approveSecurityCount, "visible selected host")}`,
                bulkDisabledTitle ||
                    (selectedCount === 0
                        ? "Select visible hosts first"
                        : "No selected host has standard security updates eligible for approval"),
            );
            setBulkButtonState(
                "bulk-approve-kept-security",
                "Approve kept-back security",
                displayCount(
                    "approve_security_kept_back",
                    approveKeptSecurityCount,
                ),
                !bulk && approveKeptSecurityCount > 0,
                `Approve kept-back security updates on ${pluralize(approveKeptSecurityCount, "visible selected host")}`,
                bulkDisabledTitle ||
                    (selectedCount === 0
                        ? "Select visible hosts first"
                        : removalReviews.length > 0
                          ? "Review package removals for each affected host"
                          : "No selected host has kept-back security updates eligible for approval"),
            );
            const review = document.getElementById("bulk-removal-review");
            review.hidden = removalReviews.length === 0;
            renderHTML(review, removalReviews.length ? `
                <p class="bulk-hint warning" id="bulk-removal-review-hint">Package removals require individual confirmation.</p>
                <ul>${removalReviews.map(({ name, serverIdentity }) => `
                    <li data-render-key="${escapeHtml(serverIdentity || name)}">
                        <button type="button" class="inline-btn btn-ghost" data-action="review-kept-back-removals"
                            data-name="${escapeHtml(name)}" data-server-identity="${escapeHtml(serverIdentity)}"
                            aria-describedby="bulk-removal-review-hint">Review ${escapeHtml(name)}</button>
                    </li>`).join("")}</ul>` : "");
            setBulkButtonState(
                "bulk-cancel",
                "Cancel",
                displayCount("cancel", cancelCount),
                !bulk && cancelCount > 0,
                `Cancel approval for ${pluralize(cancelCount, "visible selected host")}`,
                bulkDisabledTitle ||
                    (selectedCount === 0
                        ? "Select visible hosts first"
                        : "No selected host is waiting for approval"),
            );
            setBulkButtonState(
                "bulk-autoremove",
                "Autoremove",
                displayCount("autoremove", autoremoveCount),
                !bulk && autoremoveCount > 0,
                `Run autoremove on ${pluralize(autoremoveCount, "visible selected host")}`,
                bulkDisabledTitle ||
                    (selectedCount === 0
                        ? "Select visible hosts first"
                        : "No visible selected host can run autoremove"),
            );
            updateRefreshAllFactsState();
            scheduleSelectPageStateUpdate();
        }
        function buildBulkActionPlan(actionPath, actionLabel) {
            const actionKey = bulkDashboardActionKey(actionPath);
            const plan = statusInteraction.planBulkAction(actionKey, {
                actionLabel,
            });
            return {
                ...plan,
                actionPath,
            };
        }
        const batchLabels = {
            update: "Update checks",
            approve_all: "Approve standard updates",
            approve_security: "Approve security updates",
            approve_security_kept_back: "Approve kept-back security updates",
            cancel: "Cancel approval",
            autoremove: "Autoremove",
            refresh_facts: "Refresh host facts",
        };
        let batch = null;
        let batchID = "";
        let batchTimer = null;
        let batchRead = 0;
        let pendingPlan = null;
        let submitting = false;

        function newBatchKey() {
            return Array.from(window.crypto.getRandomValues(new Uint8Array(16)),
                byte => byte.toString(16).padStart(2, "0")).join("");
        }

        function batchTarget(name, facts) {
            return { name, server_identity: facts.serverIdentity, approval: facts.approvalIdentity };
        }

        function renderBatch() {
            const panel = document.getElementById("status-batch-panel");
            if (!panel) return;
            panel.hidden = !batch;
            if (!batch) return;
            document.getElementById("status-batch-label").textContent = batchLabels[batch.action];
            document.getElementById("status-batch-link").href = `?maintenance_batch=${encodeURIComponent(batch.id)}`;
            const labels = {
                queued: "Queued", admitting: "Checking eligibility", accepted: "Accepted",
                running: "In progress", waiting_approval: "Waiting for approval",
                completed: "Completed", refused: "Refused", interrupted: "Interrupted", uncertain: "Needs verification",
                succeeded: "Succeeded", failed: "Failed", cancelled: "Cancelled",
            };
            const statusClass = {
                queued: "idle", admitting: "updating", accepted: "updating", running: "upgrading",
                waiting_approval: "pending_approval", completed: "done", refused: "error",
                interrupted: "error", uncertain: "error", succeeded: "done", failed: "error", cancelled: "idle",
            };
            const badge = state => `<span class="status-pill status-${statusClass[state] || "idle"}">${escapeHtml(labels[state] || state)}</span>`;
            renderHTML(document.getElementById("status-batch-rows"), batch.hosts.map(host => {
                const admitted = host.job_id || ["accepted", "running", "waiting_approval", "completed"].includes(host.state);
                const admission = admitted ? "accepted" : host.state;
                const outcome = host.job_status || (admitted && host.state !== admission ? host.state : "");
                return `
                <li data-render-key="${escapeHtml(host.name)}">
                    <strong>${escapeHtml(host.name)}</strong>
                    ${badge(admission)}
                    ${outcome ? badge(outcome) : ""}
                    ${host.message ? `<p>${escapeHtml(host.message)}</p>` : ""}
                    ${host.job_id ? `<a href="/api/reports/jobs/${encodeURIComponent(host.job_id)}">Job report</a>` : ""}
                    ${host.retryable ? `<button type="button" class="inline-btn btn-ghost" data-action="retry-batch-host"
                        data-name="${escapeHtml(host.name)}">Retry ${escapeHtml(host.name)}</button>` : ""}
                </li>`;
            }).join(""));
        }

        function clearBatchRead() {
            if (batchTimer !== null) lifecycle.clearTimeout(batchTimer);
            batchTimer = null;
        }

        function scheduleBatchRead() {
            clearBatchRead();
            batchTimer = lifecycle.setTimeout(() => {
                batchTimer = null;
                readBatch();
            }, 1000);
        }

        function refreshWaitingBatch() {
            if (lifecycle.active && batchTimer === null && batch?.hosts.some(host => host.state === "waiting_approval")) {
                scheduleBatchRead();
            }
        }

        async function readBatch() {
            const lifetime = lifecycle.capture();
            const sequence = ++batchRead;
            const url = `/api/status/batches/${batchID ? encodeURIComponent(batchID) : "latest"}`;
            try {
                const response = await fetch(url);
                if (!lifecycle.isCurrent(lifetime) || sequence !== batchRead) {
                    lifecycle.discardResponse(response);
                    return;
                }
                if (!response.ok) {
                    lifecycle.discardResponse(response);
                    if ([401, 403, 404].includes(response.status)) {
                        const message = "This maintenance batch is unavailable.";
                        const plan = pendingPlan;
                        pendingPlan = null;
                        if (plan) await dispatchStatusInteraction({
                            type: "actionFailed", operationId: plan.id, refreshStreams: [], message,
                            bulkResult: { executedCount: 0 },
                        });
                        else if (batchID) window.notifyApp(message);
                    } else if (batchID || pendingPlan || batch) scheduleBatchRead();
                    return;
                }
                const data = await response.json();
                if (!lifecycle.isCurrent(lifetime) || sequence !== batchRead) return;
                batch = data.batch;
                renderBatch();
                if (!batch) return;
                const admissionsPending = batch.hosts.some(host => ["queued", "admitting"].includes(host.state));
                if (pendingPlan && !admissionsPending) {
                    const plan = pendingPlan;
                    pendingPlan = null;
                    const failures = batch.hosts.filter(host => ["refused", "uncertain", "interrupted"].includes(host.state));
                    await dispatchStatusInteraction({
                        type: failures.length ? "actionFailed" : "actionCompleted",
                        operationId: plan.id,
                        refreshStreams: plan.actionKey === "refresh_facts" ? ["servers", "dashboard"] : ["servers"],
                        message: failures.length ? `Bulk ${plan.actionLabel}: ${failures.map(host => `${host.name}: ${host.message || host.state}`).join(", ")}` : "",
                        bulkResult: {
                            selectedCount: plan.selectedNames.length,
                            executedCount: batch.hosts.length,
                            skippedCount: batch.skipped_count,
                            failedCount: failures.length,
                        },
                    });
                    if (!lifecycle.isCurrent(lifetime) || sequence !== batchRead) return;
                }
                if (admissionsPending || batch.hosts.some(host => ["accepted", "running"].includes(host.state))) scheduleBatchRead();
            } catch (error) {
                if (lifecycle.isCurrent(lifetime) && sequence === batchRead && error?.name !== "AbortError"
                    && (batchID || pendingPlan || batch?.hosts.some(host => ["queued", "admitting", "accepted", "running", "waiting_approval"].includes(host.state)))) {
                    scheduleBatchRead();
                }
            }
        }

        async function submitBatch(request, plan) {
            const lifetime = lifecycle.capture();
            submitting = true;
            try {
                const send = async () => {
                    const response = await fetch("/api/status/batches", {
                        method: "POST",
                        headers: { "Content-Type": "application/json" },
                        body: JSON.stringify(request),
                    });
                    if (!response.ok) {
                        const result = await response.json().catch(() => ({}));
                        return result?.error || "Could not submit maintenance batch";
                    }
                    const result = await response.json();
                    if (result.id !== request.id) throw new Error("The server did not confirm the submitted batch");
                    return "";
                };
                let errorMessage;
                try {
                    errorMessage = await send();
                } catch (error) {
                    if (!lifecycle.isCurrent(lifetime) || error?.name === "AbortError") return;
                    // Either successful response may have been lost while the
                    // command was already durable. Check its exact ID below.
                    await send().catch(() => {});
                }
                if (!lifecycle.isCurrent(lifetime)) return;
                if (errorMessage) {
                    if (plan) await dispatchStatusInteraction({
                        type: "actionFailed", operationId: plan.id, refreshStreams: [],
                        message: errorMessage, bulkResult: { executedCount: 0, failedCount: plan.eligibleNames.length },
                    });
                    else window.notifyApp(errorMessage);
                    return;
                }
                batchID = request.id;
                batch = null;
                pendingPlan = plan;
                clearBatchRead();
                renderBatch();
                document.getElementById("status-batch-panel").open = true;
                await readBatch();
            } finally {
                submitting = false;
            }
        }

        async function runBulkAction(actionPath, actionLabel) {
            if (submitting || getStatusView().actions.bulk) return;
            const lifetime = lifecycle.capture();
            const plan = buildBulkActionPlan(actionPath, actionLabel);
            if (!plan.eligibleNames.length) return;
            await dispatchStatusInteraction({ type: "actionStarted", plan });
            if (!lifecycle.isCurrent(lifetime) || !getStatusView().actions.inFlight.some(action => action.operationId === plan.id)) return;
            await submitBatch({
                id: newBatchKey(), action: plan.actionKey,
                targets: plan.eligibleNames.map(name => batchTarget(name, plan.payloadFacts[name])),
                skipped_count: plan.skippedHosts.length,
            }, plan);
        }

        async function retryBatchHost(name) {
            if (submitting || pendingPlan || !batch) return;
            const host = batch.hosts.find(item => item.name === name && item.retryable);
            if (!host) return;
            const facts = statusInteraction.planBatchRetry(batch.action, host.name, host.server_identity);
            if (!facts) {
                window.notifyApp("The host or plan is no longer eligible. Review the current Status row first.");
                return;
            }
            await submitBatch({ id: newBatchKey(), action: batch.action, retry_of: batch.id, targets: [batchTarget(name, facts)] }, null);
        }
        function init() {
            if (lifecycle.active) return;
            lifecycle.init();
            if (window.canUseScope("logs:read")) {
                batchID = new URLSearchParams(window.location.search).get("maintenance_batch") || "";
                document.getElementById("status-batch-panel").open = Boolean(batchID);
                void readBatch();
                lifecycle.listen(document.getElementById("status-batch-rows"), "click", (event) => {
                    const button = event.target.closest("button[data-action='retry-batch-host']");
                    if (button) retryBatchHost(button.dataset.name);
                });
            }
            lifecycle.listen(document.getElementById("bulk-removal-review"), "click", (event) => {
                const button = event.target.closest("button[data-action='review-kept-back-removals']");
                if (button) reviewKeptBackApproval(button.dataset.name, button.dataset.serverIdentity);
            });
            lifecycle.listen(
                document.getElementById("bulk-update"),
                "click",
                async () => {
                    const lifetime = lifecycle.capture();

                    await runBulkAction("update", "update");
                    if (!lifecycle.isCurrent(lifetime)) return;
                },
            );
            lifecycle.listen(
                document.getElementById("bulk-approve"),
                "click",
                async () => {
                    const lifetime = lifecycle.capture();

                    await runBulkAction("approve", "approve standard updates");
                    if (!lifecycle.isCurrent(lifetime)) return;
                },
            );
            lifecycle.listen(
                document.getElementById("bulk-approve-security"),
                "click",
                async () => {
                    const lifetime = lifecycle.capture();

                    await runBulkAction(
                        "approve-security",
                        "approve security updates",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                },
            );
            lifecycle.listen(
                document.getElementById("bulk-approve-kept-security"),
                "click",
                async () => {
                    const lifetime = lifecycle.capture();

                    await runBulkAction(
                        "approve-security-kept-back",
                        "approve kept-back security updates",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                },
            );
            lifecycle.listen(
                document.getElementById("bulk-cancel"),
                "click",
                async () => {
                    const lifetime = lifecycle.capture();

                    await runBulkAction("cancel", "cancel");
                    if (!lifecycle.isCurrent(lifetime)) return;
                },
            );
            lifecycle.listen(
                document.getElementById("bulk-autoremove"),
                "click",
                async () => {
                    const lifetime = lifecycle.capture();

                    await runBulkAction("autoremove", "apt autoremove");
                    if (!lifecycle.isCurrent(lifetime)) return;
                },
            );
            lifecycle.listen(
                document.getElementById("refresh-all-facts"),
                "click",
                async () => {
                    const lifetime = lifecycle.capture();

                    await runBulkAction("facts-refresh", "refresh facts");
                    if (!lifecycle.isCurrent(lifetime)) return;
                },
            );
        }
        return Object.freeze({
            init,
            destroy: lifecycle.destroy,
            refreshWaitingBatch,
            updateBulkActionState: (...args) =>
                lifecycle.active ? updateBulkActionState(...args) : undefined,
        });
    }
    root.StatusBulkActions = Object.freeze({ create });
})(globalThis);
