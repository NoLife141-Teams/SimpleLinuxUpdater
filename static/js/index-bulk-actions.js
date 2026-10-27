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
            pendingApprovalRequestOptions,
            dispatchStatusInteraction,
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
                        : "No selected host has kept-back security updates eligible for approval"),
            );
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
        function bulkActionRequestOptions(actionPath, facts) {
            if (!actionPath.startsWith("approve") && actionPath !== "cancel")
                return {};
            const confirmRemovals =
                actionPath === "approve-security-kept-back" &&
                facts.counts.keptBackSecurityRemovedPackages.length > 0;
            return pendingApprovalRequestOptions(
                facts.approvalIdentity,
                confirmRemovals,
            );
        }
        async function runBulkAction(actionPath, actionLabel) {
            const lifetime = lifecycle.capture();

            if (getStatusView().actions.bulk) return;
            const plan = buildBulkActionPlan(actionPath, actionLabel);
            if (plan.visibleNames.length === 0) {
                if (plan.selectedNames.length > 0) {
                    window.notifyApp(
                        `No visible selected hosts for bulk ${actionLabel}.`,
                    );
                }
                return;
            }
            if (plan.eligibleNames.length === 0) {
                window.notifyApp(
                    `No visible selected hosts can run bulk ${actionLabel}.`,
                );
                return;
            }
            await dispatchStatusInteraction({ type: "actionStarted", plan });
            if (!lifecycle.isCurrent(lifetime)) return;
            if (
                !getStatusView().actions.inFlight.some(
                    (action) => action.operationId === plan.id,
                )
            )
                return;
            try {
                const jobs = plan.eligibleNames.map(async (name) => {
                    const lifetime = lifecycle.capture();

                    const response = await fetch(
                        `/api/${actionPath}/${encodeURIComponent(name)}`,
                        {
                            method: "POST",
                            ...bulkActionRequestOptions(
                                actionPath,
                                plan.payloadFacts[name],
                            ),
                        },
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    if (!response.ok) {
                        const payload = await response.json().catch(() => ({}));
                        if (!lifecycle.isCurrent(lifetime)) return;
                        const detail =
                            typeof payload.error === "string" &&
                            payload.error.trim()
                                ? payload.error.trim()
                                : `${response.status} ${response.statusText}`.trim();
                        throw new Error(detail || "Request failed");
                    }
                });

                const results = await Promise.allSettled(jobs);
                if (!lifecycle.isCurrent(lifetime)) return;
                const failures = [];
                results.forEach((result, index) => {
                    if (result.status === "rejected") {
                        failures.push(
                            `${plan.eligibleNames[index]}: ${result.reason?.message || "Request failed"}`,
                        );
                    }
                });

                let message = "";
                if (failures.length > 0) {
                    console.error(`Bulk ${actionLabel} failures:`, failures);
                    message = `Bulk ${actionLabel} completed with ${failures.length} failure(s): ${failures.join(", ")}`;
                } else if (
                    plan.hiddenNames.length > 0 ||
                    plan.ineligible.length > 0
                ) {
                    const skipped = [];
                    if (plan.hiddenNames.length > 0)
                        skipped.push(
                            `${plan.hiddenNames.length} hidden selected host(s)`,
                        );
                    if (plan.ineligible.length > 0)
                        skipped.push(
                            `${plan.ineligible.length} ineligible visible host(s)`,
                        );
                    message = `Bulk ${actionLabel} completed; ${skipped.join(" and ")} were skipped.`;
                }
                await dispatchStatusInteraction({
                    type:
                        failures.length > 0
                            ? "actionFailed"
                            : "actionCompleted",
                    operationId: plan.id,
                    refreshStreams: ["servers"],
                    message,
                    bulkResult: {
                        selectedCount: plan.selectedNames.length,
                        executedCount: plan.eligibleNames.length,
                        skippedCount: plan.skippedHosts.length,
                        failedCount: failures.length,
                    },
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (error) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    error?.name === "AbortError"
                )
                    return;
                await dispatchStatusInteraction({
                    type: "actionFailed",
                    operationId: plan.id,
                    refreshStreams: ["servers"],
                    message: `Bulk ${actionLabel} failed: ${error?.message || "Request failed"}`,
                    bulkResult: {
                        selectedCount: plan.selectedNames.length,
                        executedCount: plan.eligibleNames.length,
                        skippedCount: plan.skippedHosts.length,
                        failedCount: plan.eligibleNames.length,
                    },
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            }
        }
        async function refreshSelectedHostFacts() {
            const lifetime = lifecycle.capture();

            if (getStatusView().actions.bulk) return;
            const plan = buildBulkActionPlan("facts-refresh", "refresh facts");
            if (plan.visibleNames.length === 0) {
                if (plan.selectedNames.length > 0) {
                    window.notifyApp(
                        "No visible selected hosts for facts refresh.",
                    );
                }
                return;
            }
            if (plan.eligibleNames.length === 0) {
                window.notifyApp(
                    "No visible selected hosts can refresh facts right now.",
                );
                return;
            }
            await dispatchStatusInteraction({ type: "actionStarted", plan });
            if (!lifecycle.isCurrent(lifetime)) return;
            if (
                !getStatusView().actions.inFlight.some(
                    (action) => action.operationId === plan.id,
                )
            )
                return;
            const failures = [];
            let cursor = 0;
            const workerCount = Math.min(4, plan.eligibleNames.length);
            const runWorker = async () => {
                const lifetime = lifecycle.capture();

                while (cursor < plan.eligibleNames.length) {
                    const name = plan.eligibleNames[cursor];
                    cursor += 1;
                    try {
                        const response = await fetch(
                            `/api/servers/${encodeURIComponent(name)}/facts/refresh`,
                            { method: "POST" },
                        );
                        if (!lifecycle.isCurrent(lifetime)) return;
                        if (!response.ok) {
                            const payload = await response
                                .json()
                                .catch(() => ({}));
                            if (!lifecycle.isCurrent(lifetime)) return;
                            failures.push(
                                `${name}: ${payload.error || response.statusText || response.status}`,
                            );
                        }
                    } catch (err) {
                        if (
                            !lifecycle.isCurrent(lifetime) ||
                            err?.name === "AbortError"
                        )
                            return;
                        failures.push(
                            `${name}: ${err?.message || "Failed to refresh host facts"}`,
                        );
                    }
                }
            };
            try {
                await Promise.all(
                    Array.from({ length: workerCount }, runWorker),
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                let message = "";
                if (failures.length > 0) {
                    message = `Facts refresh completed with ${failures.length} failure(s): ${failures.join(", ")}`;
                } else if (
                    plan.hiddenNames.length > 0 ||
                    plan.ineligible.length > 0
                ) {
                    const skipped = [];
                    if (plan.hiddenNames.length > 0)
                        skipped.push(
                            `${plan.hiddenNames.length} hidden selected host(s)`,
                        );
                    if (plan.ineligible.length > 0)
                        skipped.push(
                            `${plan.ineligible.length} active or unavailable visible host(s)`,
                        );
                    message = `Facts refresh completed; ${skipped.join(" and ")} were skipped.`;
                }
                await dispatchStatusInteraction({
                    type:
                        failures.length > 0
                            ? "actionFailed"
                            : "actionCompleted",
                    operationId: plan.id,
                    refreshStreams: ["servers", "dashboard"],
                    message,
                    bulkResult: {
                        selectedCount: plan.selectedNames.length,
                        executedCount: plan.eligibleNames.length,
                        skippedCount: plan.skippedHosts.length,
                        failedCount: failures.length,
                    },
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (error) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    error?.name === "AbortError"
                )
                    return;
                await dispatchStatusInteraction({
                    type: "actionFailed",
                    operationId: plan.id,
                    refreshStreams: ["servers", "dashboard"],
                    message: `Facts refresh failed: ${error?.message || "Request failed"}`,
                    bulkResult: {
                        selectedCount: plan.selectedNames.length,
                        executedCount: plan.eligibleNames.length,
                        skippedCount: plan.skippedHosts.length,
                        failedCount: plan.eligibleNames.length,
                    },
                });
                if (!lifecycle.isCurrent(lifetime)) return;
            }
        }
        function init() {
            if (lifecycle.active) return;
            lifecycle.init();
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

                    await refreshSelectedHostFacts();
                    if (!lifecycle.isCurrent(lifetime)) return;
                },
            );
        }
        return Object.freeze({
            init,
            destroy: lifecycle.destroy,
            updateBulkActionState: (...args) =>
                lifecycle.active ? updateBulkActionState(...args) : undefined,
        });
    }
    root.StatusBulkActions = Object.freeze({ create });
})(globalThis);
