// Dashboard bulk actions. Loaded before index.js.
        function bulkDashboardActionKey(actionPath) {
            if (actionPath === "update") return "update";
            if (actionPath === "approve") return "approve_all";
            if (actionPath === "approve-security") return "approve_security";
            if (actionPath === "approve-security-kept-back") return "approve_security_kept_back";
            if (actionPath === "cancel") return "cancel";
            if (actionPath === "autoremove") return "autoremove";
            if (actionPath === "facts-refresh") return "refresh_facts";
            return "";
        }

        function setBulkButtonState(id, label, count, enabled, enabledTitle, disabledTitle) {
            const button = document.getElementById(id);
            if (!button) return;
            button.disabled = !enabled;
            button.textContent = `${label} (${count})`;
            button.title = enabled ? enabledTitle : disabledTitle;
            button.setAttribute('aria-describedby', 'bulk-action-hint');
        }

	        function updateBulkActionState() {
	            const hint = document.getElementById('bulk-action-hint');
	            const view = getStatusView();
	            const previewPlan = actionKey => statusInteraction.planBulkAction(actionKey, { preview: true });
	            const updatePlan = previewPlan("update");
	            const approvePlan = previewPlan("approve_all");
	            const approveSecurityPlan = previewPlan("approve_security");
	            const approveKeptSecurityPlan = previewPlan("approve_security_kept_back");
	            const cancelPlan = previewPlan("cancel");
	            const autoremovePlan = previewPlan("autoremove");
	            const selectedCount = view.selectedNames.length;
	            const visibleCount = view.visibleSelectedNames.length;
	            const hiddenCount = view.hiddenSelectedNames.length;
	            const updateCount = updatePlan.eligibleNames.length;
	            const approveCount = approvePlan.eligibleNames.length;
	            const approveSecurityCount = approveSecurityPlan.eligibleNames.length;
	            const approveKeptSecurityCount = approveKeptSecurityPlan.eligibleNames.length;
	            const cancelCount = cancelPlan.eligibleNames.length;
	            const autoremoveCount = autoremovePlan.eligibleNames.length;
	            const bulk = view.actions.bulk;
	            const lastBulkResult = view.actions.lastBulkResult;

	            if (hint) {
	                if (bulk) {
	                    hint.textContent = `${bulk.selectedCount} selected · ${bulk.eligibleCount} executing · ${bulk.skippedCount} skipped`;
	                    hint.classList.toggle("warning", bulk.skippedCount > 0);
	                } else if (lastBulkResult) {
	                    const parts = [
	                        `${lastBulkResult.selectedCount} selected`,
	                        `${lastBulkResult.executedCount} executed`,
	                        `${lastBulkResult.skippedCount} skipped`
	                    ];
	                    if (lastBulkResult.failedCount > 0) parts.push(`${lastBulkResult.failedCount} failed`);
	                    hint.textContent = parts.join(" · ");
	                    hint.classList.toggle("warning", lastBulkResult.skippedCount > 0 || lastBulkResult.failedCount > 0);
	                } else if (selectedCount === 0) {
		                    hint.textContent = "No hosts selected";
		                    hint.classList.remove("warning");
		                } else if (visibleCount === 0) {
		                    hint.textContent = `${pluralize(selectedCount, "host")} selected · 0 visible in current filter`;
	                    hint.classList.add("warning");
	                } else {
	                    const parts = [`${selectedCount} selected`, `${visibleCount} visible`];
	                    if (hiddenCount > 0) parts.push(`${hiddenCount} outside current view`);
	                    hint.textContent = parts.join(" · ");
	                    hint.classList.toggle("warning", hiddenCount > 0);
	                }
	            }

		            const bulkDisabledTitle = bulk
		                ? `Bulk ${bulk.actionLabel} is already running`
		                : null;
	            const displayCount = (actionKey, count) => bulk?.actionKey === actionKey ? bulk.eligibleCount : count;
	            setBulkButtonState("bulk-update", "Update", displayCount("update", updateCount), !bulk && updateCount > 0, `Update ${pluralize(updateCount, "visible selected host")}`, bulkDisabledTitle || (selectedCount === 0 ? "Select visible hosts first" : "No selected host can run update checks"));
	            setBulkButtonState("bulk-approve", "Approve standard", displayCount("approve_all", approveCount), !bulk && approveCount > 0, `Approve standard updates on ${pluralize(approveCount, "visible selected host")}`, bulkDisabledTitle || (selectedCount === 0 ? "Select visible hosts first" : "No selected host has standard updates eligible for approval"));
	            setBulkButtonState("bulk-approve-security", "Approve standard security", displayCount("approve_security", approveSecurityCount), !bulk && approveSecurityCount > 0, `Approve standard security updates on ${pluralize(approveSecurityCount, "visible selected host")}`, bulkDisabledTitle || (selectedCount === 0 ? "Select visible hosts first" : "No selected host has standard security updates eligible for approval"));
	            setBulkButtonState("bulk-approve-kept-security", "Approve kept-back security", displayCount("approve_security_kept_back", approveKeptSecurityCount), !bulk && approveKeptSecurityCount > 0, `Approve kept-back security updates on ${pluralize(approveKeptSecurityCount, "visible selected host")}`, bulkDisabledTitle || (selectedCount === 0 ? "Select visible hosts first" : "No selected host has kept-back security updates eligible for approval"));
	            setBulkButtonState("bulk-cancel", "Cancel", displayCount("cancel", cancelCount), !bulk && cancelCount > 0, `Cancel approval for ${pluralize(cancelCount, "visible selected host")}`, bulkDisabledTitle || (selectedCount === 0 ? "Select visible hosts first" : "No selected host is waiting for approval"));
	            setBulkButtonState("bulk-autoremove", "Autoremove", displayCount("autoremove", autoremoveCount), !bulk && autoremoveCount > 0, `Run autoremove on ${pluralize(autoremoveCount, "visible selected host")}`, bulkDisabledTitle || (selectedCount === 0 ? "Select visible hosts first" : "No visible selected host can run autoremove"));
	            updateRefreshAllFactsState();
	            scheduleSelectPageStateUpdate();
	        }

        function buildBulkActionPlan(actionPath, actionLabel) {
            const actionKey = bulkDashboardActionKey(actionPath);
            const plan = statusInteraction.planBulkAction(actionKey, { actionLabel });
            return {
                ...plan,
                actionPath
            };
        }

        function bulkActionRequestOptions(actionPath, name) {
            if (actionPath !== "approve-security-kept-back") {
                return {};
            }
            const counts = getPendingApprovalCounts(getServerByName(name));
            const body = counts.keptBackSecurityRemovedPackages.length > 0 ? { confirm_removals: true } : {};
            return {
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify(body)
            };
        }

	        async function runBulkAction(actionPath, actionLabel) {
            if (getStatusView().actions.bulk) return;
            const plan = buildBulkActionPlan(actionPath, actionLabel);
            if (plan.visibleNames.length === 0) {
                if (plan.selectedNames.length > 0) {
                    window.notifyApp(`No visible selected hosts for bulk ${actionLabel}.`);
                }
                return;
            }
            if (plan.eligibleNames.length === 0) {
                window.notifyApp(`No visible selected hosts can run bulk ${actionLabel}.`);
                return;
            }
	            await dispatchStatusInteraction({ type: "actionStarted", plan });
	            if (!getStatusView().actions.inFlight.some(action => action.operationId === plan.id)) return;
	            try {
	                const jobs = plan.eligibleNames.map(async (name) => {
	                    const response = await fetch(`/api/${actionPath}/${encodeURIComponent(name)}`, { method: 'POST', ...bulkActionRequestOptions(actionPath, name) });
	                    if (!response.ok) {
	                        const payload = await response.json().catch(() => ({}));
	                        const detail = typeof payload.error === 'string' && payload.error.trim()
	                            ? payload.error.trim()
	                            : `${response.status} ${response.statusText}`.trim();
	                        throw new Error(detail || 'Request failed');
	                    }
	                });

	                const results = await Promise.allSettled(jobs);
	                const failures = [];
	                results.forEach((result, index) => {
	                    if (result.status === 'rejected') {
	                        failures.push(`${plan.eligibleNames[index]}: ${result.reason?.message || 'Request failed'}`);
	                    }
	                });

	                let message = "";
	                if (failures.length > 0) {
	                    console.error(`Bulk ${actionLabel} failures:`, failures);
	                    message = `Bulk ${actionLabel} completed with ${failures.length} failure(s): ${failures.join(', ')}`;
	                } else if (plan.hiddenNames.length > 0 || plan.ineligible.length > 0) {
	                    const skipped = [];
	                    if (plan.hiddenNames.length > 0) skipped.push(`${plan.hiddenNames.length} hidden selected host(s)`);
	                    if (plan.ineligible.length > 0) skipped.push(`${plan.ineligible.length} ineligible visible host(s)`);
	                    message = `Bulk ${actionLabel} completed; ${skipped.join(" and ")} were skipped.`;
	                }
	                await dispatchStatusInteraction({
	                    type: failures.length > 0 ? "actionFailed" : "actionCompleted",
	                    operationId: plan.id,
	                    refreshStreams: ["servers"],
	                    message,
	                    bulkResult: {
	                        selectedCount: plan.selectedNames.length,
	                        executedCount: plan.eligibleNames.length,
	                        skippedCount: plan.skippedHosts.length,
	                        failedCount: failures.length
	                    }
	                });
	            } catch (error) {
	                await dispatchStatusInteraction({
	                    type: "actionFailed",
	                    operationId: plan.id,
	                    refreshStreams: ["servers"],
	                    message: `Bulk ${actionLabel} failed: ${error?.message || "Request failed"}`,
	                    bulkResult: {
	                        selectedCount: plan.selectedNames.length,
	                        executedCount: plan.eligibleNames.length,
	                        skippedCount: plan.skippedHosts.length,
	                        failedCount: plan.eligibleNames.length
	                    }
	                });
	            }
	        }

        document.getElementById('bulk-update').addEventListener('click', async () => {
            await runBulkAction('update', 'update');
        });
	        document.getElementById('bulk-approve').addEventListener('click', async () => {
	            await runBulkAction('approve', 'approve standard updates');
	        });
        document.getElementById('bulk-approve-security').addEventListener('click', async () => {
            await runBulkAction('approve-security', 'approve security updates');
        });
        document.getElementById('bulk-approve-kept-security').addEventListener('click', async () => {
            await runBulkAction('approve-security-kept-back', 'approve kept-back security updates');
        });
        document.getElementById('bulk-cancel').addEventListener('click', async () => {
            await runBulkAction('cancel', 'cancel');
        });
        document.getElementById('bulk-autoremove').addEventListener('click', async () => {
            await runBulkAction('autoremove', 'apt autoremove');
        });
        document.getElementById('refresh-all-facts').addEventListener('click', async () => {
            await refreshSelectedHostFacts();
        });

        async function refreshSelectedHostFacts() {
            if (getStatusView().actions.bulk) return;
            const plan = buildBulkActionPlan("facts-refresh", "refresh facts");
	            if (plan.visibleNames.length === 0) {
	                if (plan.selectedNames.length > 0) {
	                    window.notifyApp("No visible selected hosts for facts refresh.");
	                }
	                return;
	            }
	            if (plan.eligibleNames.length === 0) {
	                window.notifyApp("No visible selected hosts can refresh facts right now.");
	                return;
	            }
	            await dispatchStatusInteraction({ type: "actionStarted", plan });
	            if (!getStatusView().actions.inFlight.some(action => action.operationId === plan.id)) return;
	            const failures = [];
	            let cursor = 0;
	            const workerCount = Math.min(4, plan.eligibleNames.length);
	            const runWorker = async () => {
	                while (cursor < plan.eligibleNames.length) {
	                    const name = plan.eligibleNames[cursor];
	                    cursor += 1;
	                    try {
	                        const response = await fetch(`/api/servers/${encodeURIComponent(name)}/facts/refresh`, { method: 'POST' });
	                        if (!response.ok) {
	                            const payload = await response.json().catch(() => ({}));
	                            failures.push(`${name}: ${payload.error || response.statusText || response.status}`);
	                        }
                    } catch (err) {
                        failures.push(`${name}: ${err?.message || "Failed to refresh host facts"}`);
                    }
                }
            };
	            try {
	                await Promise.all(Array.from({ length: workerCount }, runWorker));
	                let message = "";
	                if (failures.length > 0) {
	                    message = `Facts refresh completed with ${failures.length} failure(s): ${failures.join(", ")}`;
	                } else if (plan.hiddenNames.length > 0 || plan.ineligible.length > 0) {
	                    const skipped = [];
	                    if (plan.hiddenNames.length > 0) skipped.push(`${plan.hiddenNames.length} hidden selected host(s)`);
	                    if (plan.ineligible.length > 0) skipped.push(`${plan.ineligible.length} active or unavailable visible host(s)`);
	                    message = `Facts refresh completed; ${skipped.join(" and ")} were skipped.`;
	                }
	                await dispatchStatusInteraction({
	                    type: failures.length > 0 ? "actionFailed" : "actionCompleted",
	                    operationId: plan.id,
	                    refreshStreams: ["servers", "dashboard"],
	                    message,
	                    bulkResult: {
	                        selectedCount: plan.selectedNames.length,
	                        executedCount: plan.eligibleNames.length,
	                        skippedCount: plan.skippedHosts.length,
	                        failedCount: failures.length
	                    }
	                });
	            } catch (error) {
	                await dispatchStatusInteraction({
	                    type: "actionFailed",
	                    operationId: plan.id,
	                    refreshStreams: ["servers", "dashboard"],
	                    message: `Facts refresh failed: ${error?.message || "Request failed"}`,
	                    bulkResult: {
	                        selectedCount: plan.selectedNames.length,
	                        executedCount: plan.eligibleNames.length,
	                        skippedCount: plan.skippedHosts.length,
	                        failedCount: plan.eligibleNames.length
	                    }
	                });
	            }
	        }
