// Dashboard bulk action and review modal helpers. Loaded before index.js.
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

        function getBulkDashboardAction(actionPath, server, options = {}) {
            const key = bulkDashboardActionKey(actionPath);
            if (!key || typeof getServerActionContract !== "function") return null;
            return getServerActionContract(server, key, options);
        }

	        function canRunBulkUpdate(server) {
	            return canRunUpdateAction(server);
	        }

	        function canRunBulkAutoremove(server) {
	            return canRunAutoremoveAction(server);
	        }

	        function canRefreshBulkFacts(server) {
	            return canRefreshFactsAction(server);
	        }

	        function canRunBulkApprove(server) {
	            return !!getServerApprovalTriage(server).can_approve_all;
	        }

	        function canRunBulkApproveSecurity(server) {
	            return !!getServerApprovalTriage(server).can_approve_security;
	        }

	        function canRunBulkApproveKeptBackSecurity(server) {
	            return !!getServerApprovalTriage(server).can_approve_kept_back_security;
	        }

	        function canRunBulkCancel(server) {
	            return !!getServerApprovalTriage(server).can_cancel;
	        }

        function canRunBulkAction(actionPath, server) {
            const action = getBulkDashboardAction(actionPath, server);
            if (action) return !!action.enabled;
            if (actionPath === "update") return canRunBulkUpdate(server);
            if (actionPath === "approve") return canRunBulkApprove(server);
            if (actionPath === "approve-security") return canRunBulkApproveSecurity(server);
            if (actionPath === "approve-security-kept-back") return canRunBulkApproveKeptBackSecurity(server);
            if (actionPath === "cancel") return canRunBulkCancel(server);
            if (actionPath === "autoremove") return canRunBulkAutoremove(server);
            if (actionPath === "facts-refresh") return canRefreshBulkFacts(server);
            return true;
        }

        function setBulkButtonState(id, enabled, enabledTitle, disabledTitle) {
            const button = document.getElementById(id);
            if (!button) return;
            button.disabled = !enabled;
            button.title = enabled ? enabledTitle : disabledTitle;
            button.setAttribute('aria-describedby', 'bulk-action-hint');
        }

	        function updateBulkActionState() {
	            const hint = document.getElementById('bulk-action-hint');
	            const selectedNames = Array.from(selectedServers);
	            const visibleSelectedServers = getVisibleSelectedServers();
	            const visibleCount = visibleSelectedServers.length;
	            const selectedCount = selectedNames.length;
	            const hiddenCount = Math.max(0, selectedCount - visibleCount);
	            const approveCount = visibleSelectedServers.filter(canRunBulkApprove).length;
	            const approveSecurityCount = visibleSelectedServers.filter(canRunBulkApproveSecurity).length;
	            const approveKeptSecurityCount = visibleSelectedServers.filter(canRunBulkApproveKeptBackSecurity).length;
	            const cancelCount = visibleSelectedServers.filter(canRunBulkCancel).length;
	            const updateCount = visibleSelectedServers.filter(canRunBulkUpdate).length;
	            const autoremoveCount = visibleSelectedServers.filter(canRunBulkAutoremove).length;
	            const refreshFactsCount = visibleSelectedServers.filter(canRefreshBulkFacts).length;

		            if (hint) {
		                if (bulkActionInFlightLabel) {
		                    hint.textContent = `Bulk ${bulkActionInFlightLabel} running for visible selected hosts`;
		                    hint.classList.remove("warning");
		                } else if (selectedCount === 0) {
		                    hint.textContent = "No hosts selected";
		                    hint.classList.remove("warning");
		                } else if (visibleCount === 0) {
		                    hint.textContent = `${pluralize(selectedCount, "host")} selected · 0 visible in current filter`;
	                    hint.classList.add("warning");
	                } else {
	                    const parts = [`${visibleCount} visible ${visibleCount === 1 ? "host" : "hosts"} selected`];
		                    if (updateCount > 0) parts.push(`${updateCount} can update`);
		                    if (approveCount > 0) parts.push(`${approveCount} can approve standard`);
		                    if (approveSecurityCount > 0) parts.push(`${approveSecurityCount} can approve security`);
		                    if (approveKeptSecurityCount > 0) parts.push(`${approveKeptSecurityCount} can approve kept security`);
		                    if (refreshFactsCount > 0) parts.push(`${refreshFactsCount} can refresh facts`);
		                    if (autoremoveCount > 0) parts.push(`${autoremoveCount} can autoremove`);
		                    if (hiddenCount > 0) parts.push(`${hiddenCount} skipped by current filter`);
	                    hint.textContent = parts.join(" · ");
	                    hint.classList.toggle("warning", hiddenCount > 0);
	                }
	            }

		            const bulkDisabledTitle = bulkActionInFlightLabel
		                ? `Bulk ${bulkActionInFlightLabel} is already running`
		                : null;
		            setBulkButtonState("bulk-update", !bulkActionInFlightLabel && updateCount > 0, `Update ${pluralize(updateCount, "visible selected host")}`, bulkDisabledTitle || (selectedCount === 0 ? "Select visible hosts first" : "No selected host can run update checks"));
		            setBulkButtonState("bulk-approve", !bulkActionInFlightLabel && approveCount > 0, `Approve standard updates on ${pluralize(approveCount, "visible selected host")}`, bulkDisabledTitle || (selectedCount === 0 ? "Select visible hosts first" : "No selected host has standard updates eligible for approval"));
		            setBulkButtonState("bulk-approve-security", !bulkActionInFlightLabel && approveSecurityCount > 0, `Approve standard security updates on ${pluralize(approveSecurityCount, "visible selected host")}`, bulkDisabledTitle || (selectedCount === 0 ? "Select visible hosts first" : "No selected host has standard security updates eligible for approval"));
		            setBulkButtonState("bulk-approve-kept-security", !bulkActionInFlightLabel && approveKeptSecurityCount > 0, `Approve kept-back security updates on ${pluralize(approveKeptSecurityCount, "visible selected host")}`, bulkDisabledTitle || (selectedCount === 0 ? "Select visible hosts first" : "No selected host has kept-back security updates eligible for approval"));
		            setBulkButtonState("bulk-cancel", !bulkActionInFlightLabel && cancelCount > 0, `Cancel approval for ${pluralize(cancelCount, "visible selected host")}`, bulkDisabledTitle || (selectedCount === 0 ? "Select visible hosts first" : "No selected host is waiting for approval"));
		            setBulkButtonState("bulk-autoremove", !bulkActionInFlightLabel && autoremoveCount > 0, `Run autoremove on ${pluralize(autoremoveCount, "visible selected host")}`, bulkDisabledTitle || (selectedCount === 0 ? "Select visible hosts first" : "No visible selected host can run autoremove"));
	            updateRefreshAllFactsState();
	            scheduleSelectPageStateUpdate();
	        }

        function bulkActionWarning(actionPath) {
            if (actionPath === "approve") {
                return "Kept-back and full-upgrade-only packages are not included.";
            }
            if (actionPath === "approve-security") {
                return "Only standard security updates are included.";
            }
            if (actionPath === "approve-security-kept-back") {
                return "Kept-back security approvals use targeted apt install; package removals are confirmed from the previewed plan.";
            }
            if (actionPath === "cancel") {
                return "This cancels approval for each eligible host.";
            }
            return "";
        }

        function bulkActionConfirmationText(actionPath) {
            if (actionPath === "update") return "BULK UPDATE";
            if (actionPath === "approve") return "BULK APPROVE";
            if (actionPath === "approve-security") return "BULK APPROVE SECURITY";
            if (actionPath === "approve-security-kept-back") return "BULK APPROVE KEPT SECURITY";
            return "";
        }

	        function bulkReadyReason(actionPath, server) {
            const action = getBulkDashboardAction(actionPath, server);
            if (action && action.reason) return action.reason;
	            const counts = getPendingApprovalCounts(server);
	            if (actionPath === "update") return "Ready to start update checks.";
	            if (actionPath === "approve") return `${pluralize(counts.standard, "standard update")} ready for approval.`;
            if (actionPath === "approve-security") return `${pluralize(counts.security || 0, "standard security update")} ready for approval.`;
            if (actionPath === "approve-security-kept-back") {
                const removalNote = counts.keptBackSecurityRemovedPackages.length > 0
                    ? " Package removals will be confirmed from the previewed plan."
                    : "";
                return `${pluralize(counts.keptBackSecurity || 0, "kept-back security update")} ready for targeted approval.${removalNote}`;
            }
            if (actionPath === "cancel") return "Ready to cancel pending approval.";
            if (actionPath === "autoremove") return "Ready to run apt autoremove.";
            if (actionPath === "facts-refresh") return "Ready to refresh host facts.";
            return "Ready.";
        }

        function bulkIneligibleReason(actionPath, server) {
            if (!server) return "Host is no longer loaded";
            const action = getBulkDashboardAction(actionPath, server);
            if (action && action.reason) return action.reason;
            if (isSingleHostActionInFlight(server.name)) return "Another action is already running for this host";
            const counts = getPendingApprovalCounts(server);
            if (actionPath === "update") {
                if (statusBlocksTransientAction(server.status)) return `Current status ${statusLabel(server.status)} blocks update checks`;
                return "Cannot start update checks now";
            }
            if (actionPath === "approve") {
                if (server.status !== "pending_approval") return "Not waiting for approval";
                if (counts.standard <= 0) return "No standard updates eligible";
                return "No standard updates eligible";
            }
            if (actionPath === "approve-security") {
                if (server.status !== "pending_approval") return "Not waiting for approval";
                if ((counts.security || 0) <= 0) return "No standard security updates eligible";
                return "No standard security updates eligible";
            }
            if (actionPath === "approve-security-kept-back") {
                if (server.status !== "pending_approval") return "Not waiting for approval";
                return counts.keptBackSecurityPlanAvailable ? "No kept-back security updates eligible" : "Needs a fresh package scan";
            }
            if (actionPath === "cancel") {
                if (server.status !== "pending_approval") return "Not waiting for approval";
                return "Cannot cancel approval now";
            }
            if (actionPath === "autoremove") {
                if (statusBlocksTransientAction(server.status)) return `Current status ${statusLabel(server.status)} blocks autoremove`;
                return "Cannot run autoremove now";
            }
            if (actionPath === "facts-refresh") {
                if (statusBlocksTransientAction(server.status)) return `Current status ${statusLabel(server.status)} blocks facts refresh`;
                return "Cannot refresh facts now";
            }
            return "Not eligible";
        }

        function buildBulkActionPlan(actionPath, actionLabel) {
            const visibleSelected = new Set(
                Array.from(document.querySelectorAll('#servers-table tbody tr[data-name] .row-select:checked'))
                    .map(cb => cb.dataset.name)
                    .filter(Boolean)
            );
            const selectedNames = Array.from(selectedServers);
            const visibleNames = selectedNames.filter(name => visibleSelected.has(name));
            const hiddenNames = selectedNames.filter(name => !visibleSelected.has(name));
            const eligibleNames = [];
            const eligibleHosts = [];
            const ineligible = [];
            visibleNames.forEach(name => {
                const server = getServerByName(name);
                if (canRunBulkAction(actionPath, server)) {
                    eligibleNames.push(name);
                    eligibleHosts.push({
                        name,
                        auth: getAuthLabel(server),
                        readiness: bulkReadyReason(actionPath, server)
                    });
                } else {
                    ineligible.push({
                        name,
                        auth: server ? getAuthLabel(server) : "Unknown",
                        reason: bulkIneligibleReason(actionPath, server)
                    });
                }
            });
            const hiddenHosts = hiddenNames.map(name => {
                const server = getServerByName(name);
                return {
                    name,
                    auth: server ? getAuthLabel(server) : "Unknown",
                    reason: "Hidden by current filter or page"
                };
            });
            return {
                actionPath,
                actionLabel,
                selectedNames,
                visibleNames,
                hiddenNames,
                eligibleNames,
                eligibleHosts,
                ineligible,
                skippedHosts: [...hiddenHosts, ...ineligible],
                confirmationText: bulkActionConfirmationText(actionPath),
                warning: bulkActionWarning(actionPath)
            };
        }

        function fillBulkReviewRows(id, items, emptyText, skipped = false) {
            const body = document.getElementById(id);
            if (!body) return;
            body.innerHTML = "";
            if (!items.length) {
                const row = document.createElement("tr");
                row.className = "muted";
                const cell = document.createElement("td");
                cell.colSpan = 3;
                cell.textContent = emptyText;
                row.appendChild(cell);
                body.appendChild(row);
                return;
            }
            items.forEach(item => {
                const row = document.createElement("tr");
                row.className = skipped ? "bulk-review-row-skipped" : "bulk-review-row-ready";
                [item.name, item.auth, skipped ? item.reason : item.readiness].forEach(value => {
                    const cell = document.createElement("td");
                    cell.textContent = value || "-";
                    row.appendChild(cell);
                });
                body.appendChild(row);
            });
        }

        function closeBulkReviewModal(result) {
            const modal = document.getElementById("bulk-review-modal");
            modal.classList.remove("active");
            if (bulkReviewResolve) {
                const resolve = bulkReviewResolve;
                bulkReviewResolve = null;
                resolve(!!result);
            }
        }

        function requestBulkActionReview(plan) {
            const modal = document.getElementById("bulk-review-modal");
            document.getElementById("bulk-review-title").textContent = `Review bulk ${plan.actionLabel}`;
            document.getElementById("bulk-review-summary").textContent = `${pluralize(plan.eligibleNames.length, "eligible visible host")} will run. ${pluralize(plan.skippedHosts.length, "host")} will be skipped.`;
            document.getElementById("bulk-review-eligible-label").textContent = `Eligible hosts (${plan.eligibleNames.length})`;
            document.getElementById("bulk-review-skipped-label").textContent = `Skipped hosts (${plan.skippedHosts.length})`;
            fillBulkReviewRows("bulk-review-eligible", plan.eligibleHosts, "No eligible hosts.");
            fillBulkReviewRows("bulk-review-skipped", plan.skippedHosts, "No skipped hosts.", true);
            document.getElementById("bulk-review-warning").textContent = plan.warning || "";
            document.getElementById("bulk-review-confirm").disabled = plan.eligibleNames.length === 0;
            modal.classList.add("active");
            document.getElementById("bulk-review-confirm").focus({ preventScroll: true });
            return new Promise(resolve => {
                bulkReviewResolve = resolve;
            });
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

        async function confirmBulkAction(plan) {
            if (!plan.confirmationText) return true;
            const message = `Bulk ${plan.actionLabel} will run on ${pluralize(plan.eligibleNames.length, "eligible visible host")}.`;
            return window.confirmTypedAction(message, plan.confirmationText);
        }

	        async function runBulkAction(actionPath, actionLabel) {
	            if (bulkActionInFlightLabel) return;
            const plan = buildBulkActionPlan(actionPath, actionLabel);
            if (plan.visibleNames.length === 0) {
                if (plan.selectedNames.length > 0) {
                    alert(`No visible selected hosts for bulk ${actionLabel}.`);
                }
                return;
            }
            if (plan.eligibleNames.length === 0) {
                alert(`No visible selected hosts can run bulk ${actionLabel}.`);
                return;
            }
            if (!(await requestBulkActionReview(plan))) {
                return;
            }
            if (!(await confirmBulkAction(plan))) {
                return;
            }

	            const hostActionKeys = plan.eligibleNames.map(name => singleHostActionKey(name, `bulk ${actionLabel}`));
	            bulkActionInFlightLabel = actionLabel;
	            hostActionKeys.forEach(key => singleHostActionsInFlight.add(key));
	            renderSingleHostActionState();
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

	                if (failures.length > 0) {
	                    console.error(`Bulk ${actionLabel} failures:`, failures);
	                    alert(`Bulk ${actionLabel} completed with ${failures.length} failure(s): ${failures.join(', ')}`);
	                } else if (plan.hiddenNames.length > 0 || plan.ineligible.length > 0) {
	                    const skipped = [];
	                    if (plan.hiddenNames.length > 0) skipped.push(`${plan.hiddenNames.length} hidden selected host(s)`);
	                    if (plan.ineligible.length > 0) skipped.push(`${plan.ineligible.length} ineligible visible host(s)`);
	                    alert(`Bulk ${actionLabel} completed; ${skipped.join(" and ")} were skipped.`);
	                }

	                await fetchServers(true);
	            } finally {
	                hostActionKeys.forEach(key => singleHostActionsInFlight.delete(key));
	                bulkActionInFlightLabel = "";
	                renderSingleHostActionState();
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
        document.getElementById('bulk-review-cancel').addEventListener('click', () => closeBulkReviewModal(false));
        document.getElementById('bulk-review-confirm').addEventListener('click', () => closeBulkReviewModal(true));
        document.getElementById('bulk-review-modal').addEventListener('click', (e) => {
            if (e.target && e.target.id === 'bulk-review-modal') {
                closeBulkReviewModal(false);
            }
        });
        document.getElementById('refresh-all-facts').addEventListener('click', async () => {
            await refreshSelectedHostFacts();
        });

        function trapBulkReviewModalFocus(event) {
            const backdrop = document.getElementById('bulk-review-modal');
            if (!backdrop || !backdrop.classList.contains('active')) return false;
            const focusable = passwordModalFocusableElements(backdrop);
            if (!focusable.length) {
                event.preventDefault();
                return true;
            }
            const first = focusable[0];
            const last = focusable[focusable.length - 1];
            if (!backdrop.contains(document.activeElement)) {
                event.preventDefault();
                first.focus({ preventScroll: true });
                return true;
            }
            if (event.shiftKey && document.activeElement === first) {
                event.preventDefault();
                last.focus({ preventScroll: true });
                return true;
            }
            if (!event.shiftKey && document.activeElement === last) {
                event.preventDefault();
                first.focus({ preventScroll: true });
                return true;
            }
            return false;
        }

        async function refreshSelectedHostFacts() {
            if (refreshAllFactsInFlight) return;
            const plan = buildBulkActionPlan("facts-refresh", "refresh facts");
	            if (plan.visibleNames.length === 0) {
	                if (plan.selectedNames.length > 0) {
	                    alert("No visible selected hosts for facts refresh.");
	                }
	                return;
	            }
	            if (plan.eligibleNames.length === 0) {
	                alert("No visible selected hosts can refresh facts right now.");
	                return;
	            }
            if (!(await requestBulkActionReview(plan))) {
                return;
            }
	            const hostActionKeys = plan.eligibleNames.map(name => singleHostActionKey(name, "refresh facts"));
	            refreshAllFactsInFlight = true;
	            hostActionKeys.forEach(key => singleHostActionsInFlight.add(key));
	            renderSingleHostActionState();
	            updateRefreshAllFactsState();
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
	                await fetchDashboardSummary();
	                await fetchServers(true);
	                if (failures.length > 0) {
	                    alert(`Facts refresh completed with ${failures.length} failure(s): ${failures.join(", ")}`);
	                } else if (plan.hiddenNames.length > 0 || plan.ineligible.length > 0) {
	                    const skipped = [];
	                    if (plan.hiddenNames.length > 0) skipped.push(`${plan.hiddenNames.length} hidden selected host(s)`);
	                    if (plan.ineligible.length > 0) skipped.push(`${plan.ineligible.length} active or unavailable visible host(s)`);
	                    alert(`Facts refresh completed; ${skipped.join(" and ")} were skipped.`);
	                }
	            } finally {
	                hostActionKeys.forEach(key => singleHostActionsInFlight.delete(key));
	                refreshAllFactsInFlight = false;
	                renderSingleHostActionState();
	                updateRefreshAllFactsState();
	            }
	        }
