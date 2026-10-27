// Browser effects for AdminPoliciesAdapter; accepted state and command plans stay in the injected interaction owner.
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
        lifecycle.own(() => packageDeferralEditor?.destroy());
        const {
            scheduledPolicyRows,
            scheduledPolicyAdministration,
            escapeHtml,
            scheduledPolicyView,
            parseErrorResponse,
            encodeURIComponent,
            renderAdminWorkspace,
            applyScheduledTimezone,
            fetchAppTimezoneSettings,
            renderAdminDestructiveControls,
        } = options;
        let scheduledPolicyPreviewTimer = 0;

        const weekdayOptions = [
            { value: "mon", label: "Mon", fullLabel: "Monday" },
            { value: "tue", label: "Tue", fullLabel: "Tuesday" },
            { value: "wed", label: "Wed", fullLabel: "Wednesday" },
            { value: "thu", label: "Thu", fullLabel: "Thursday" },
            { value: "fri", label: "Fri", fullLabel: "Friday" },
            { value: "sat", label: "Sat", fullLabel: "Saturday" },
            { value: "sun", label: "Sun", fullLabel: "Sunday" },
        ];

        function weekdayOrder(token) {
            return weekdayOptions.findIndex((item) => item.value === token);
        }

        function normalizeWeekdayToken(raw) {
            switch (
                String(raw || "")
                    .trim()
                    .toLowerCase()
            ) {
                case "mon":
                case "monday":
                    return "mon";
                case "tue":
                case "tues":
                case "tuesday":
                    return "tue";
                case "wed":
                case "wednesday":
                    return "wed";
                case "thu":
                case "thur":
                case "thurs":
                case "thursday":
                    return "thu";
                case "fri":
                case "friday":
                    return "fri";
                case "sat":
                case "saturday":
                    return "sat";
                case "sun":
                case "sunday":
                    return "sun";
                default:
                    return "";
            }
        }

        function normalizeWeekdays(values) {
            const seen = new Set();
            return (Array.isArray(values) ? values : [])
                .map((value) => normalizeWeekdayToken(value))
                .filter(Boolean)
                .filter((value) => {
                    if (seen.has(value)) return false;
                    seen.add(value);
                    return true;
                })
                .sort((a, b) => weekdayOrder(a) - weekdayOrder(b));
        }

        function formatWeekdayLabel(token) {
            const match = weekdayOptions.find((item) => item.value === token);
            return match ? match.label : token;
        }

        function formatWeekdayList(weekdays) {
            const normalized = normalizeWeekdays(weekdays);
            return normalized.length
                ? normalized.map(formatWeekdayLabel).join(", ")
                : "No weekdays selected";
        }

        function humanizeExecutionMode(mode) {
            switch (String(mode || "").trim()) {
                case "scan_only":
                    return "Scan only";
                case "approval_required":
                    return "Approval required";
                case "auto_apply":
                    return "Auto apply";
                case "autoremove":
                    return "Autoremove";
                default:
                    return "Unknown mode";
            }
        }

        function humanizePackageScope(scope) {
            switch (String(scope || "").trim()) {
                case "security":
                    return "Security updates";
                case "full":
                    return "Full updates";
                default:
                    return "Unknown scope";
            }
        }

        function humanizeUpgradeMode(mode) {
            switch (String(mode || "").trim()) {
                case "full":
                    return "Full upgrade";
                case "standard":
                case "":
                    return "Standard upgrade";
                default:
                    return "Unknown upgrade mode";
            }
        }

        function pluralize(count, singular, plural) {
            return `${count} ${count === 1 ? singular : plural}`;
        }

        function setBlackoutJsonStatus(kind, message, isError = false) {
            const node = document.getElementById(
                kind === "global"
                    ? "scheduled-global-blackouts-json-status"
                    : "policy-blackouts-json-status",
            );
            if (!node) return;
            node.textContent = String(message || "").trim();
            node.classList.toggle("form-feedback-error", !!message && isError);
            node.classList.toggle(
                "form-feedback-success",
                !!message && !isError,
            );
        }

        function syncBlackoutTextarea(kind) {
            const textarea = document.getElementById(
                kind === "global"
                    ? "scheduled-global-blackouts-json"
                    : "policy-blackouts-json",
            );
            if (!textarea) return;
            textarea.value = JSON.stringify(scheduledPolicyRows(kind), null, 2);
        }

        function setBlackoutEditorRows(kind, rows) {
            scheduledPolicyAdministration.dispatch({
                type: "blackoutRowsReceived",
                kind,
                rows,
            });
            renderBlackoutEditor(kind);
        }

        function buildBlackoutWeekdayButtons(kind, row, index) {
            return weekdayOptions
                .map((day) => {
                    const isActive = row.weekdays.includes(day.value);
                    const active = isActive ? " active" : "";
                    return `<button class="day-chip${active}" type="button" aria-pressed="${isActive ? "true" : "false"}" aria-label="${escapeHtml(day.fullLabel)}" data-blackout-kind="${escapeHtml(kind)}" data-blackout-action="toggle-day" data-index="${escapeHtml(String(index))}" data-day="${escapeHtml(day.value)}">${escapeHtml(day.label)}</button>`;
                })
                .join("");
        }

        function blackoutRowSummaryText(row) {
            const weekdays = normalizeWeekdays(
                Array.isArray(row?.weekdays) ? row.weekdays : [],
            );
            const startTime = String(row?.start_time || "").trim() || "--:--";
            const endTime = String(row?.end_time || "").trim() || "--:--";
            return `${formatWeekdayList(weekdays)} · ${startTime} to ${endTime}`;
        }

        function updateBlackoutRowSummary(kind, index) {
            const row = scheduledPolicyRows(kind)[index];
            const rowsID =
                kind === "global"
                    ? "global-blackout-rows"
                    : "policy-blackout-rows";
            const summary = document.querySelector(
                `#${rowsID} [data-blackout-row-index="${String(index)}"] [data-blackout-summary]`,
            );
            if (!row || !summary) return;
            summary.textContent = blackoutRowSummaryText(row);
        }

        function renderBlackoutEditor(kind) {
            const rows = scheduledPolicyRows(kind);
            const container = document.getElementById(
                kind === "global"
                    ? "global-blackout-rows"
                    : "policy-blackout-rows",
            );
            if (!container) return;
            if (!rows.length) {
                container.innerHTML =
                    '<div class="empty-editor-state subtle">No no-run windows yet.</div>';
                syncBlackoutTextarea(kind);
                if (kind === "policy") updatePolicySummary();
                else renderGlobalSettingsDraftState();
                return;
            }
            container.innerHTML = rows
                .map(
                    (row, index) => `
        <div class="blackout-row" data-blackout-row-index="${escapeHtml(String(index))}">
            <div class="blackout-row-top">
                <span class="pill pill-muted">${escapeHtml(`Window ${index + 1}`)}</span>
                <button class="btn-danger inline-btn small-btn" type="button" data-blackout-kind="${escapeHtml(kind)}" data-blackout-action="remove-window" data-index="${escapeHtml(String(index))}">Remove</button>
            </div>
            <div>
                <label class="form-label">Days</label>
                <div class="weekday-picker blackout-weekday-picker" role="group" aria-label="No-run window days">
                    ${buildBlackoutWeekdayButtons(kind, row, index)}
                </div>
            </div>
            <div class="table-secondary" data-blackout-summary>${escapeHtml(blackoutRowSummaryText(row))}</div>
            <div class="blackout-time-grid">
                <div>
                    <label class="form-label" for="${escapeHtml(`${kind}-blackout-start-${index}`)}">Start</label>
                    <input type="time" id="${escapeHtml(`${kind}-blackout-start-${index}`)}" value="${escapeHtml(row.start_time)}" data-blackout-kind="${escapeHtml(kind)}" data-blackout-field="start_time" data-index="${escapeHtml(String(index))}">
                </div>
                <div>
                    <label class="form-label" for="${escapeHtml(`${kind}-blackout-end-${index}`)}">End</label>
                    <input type="time" id="${escapeHtml(`${kind}-blackout-end-${index}`)}" value="${escapeHtml(row.end_time)}" data-blackout-kind="${escapeHtml(kind)}" data-blackout-field="end_time" data-index="${escapeHtml(String(index))}">
                </div>
            </div>
        </div>
    `,
                )
                .join("");
            syncBlackoutTextarea(kind);
            if (kind === "policy") updatePolicySummary();
            else renderGlobalSettingsDraftState();
        }

        function addBlackoutRow(kind) {
            scheduledPolicyAdministration.dispatch({
                type: "blackoutRowAdded",
                kind,
            });
            setBlackoutJsonStatus(kind, "");
            renderBlackoutEditor(kind);
        }

        function setPolicyFeedback(status, error = "") {
            const statusNode = document.getElementById("update-policy-status");
            const errorNode = document.getElementById("update-policy-error");
            if (statusNode)
                statusNode.textContent = String(status || "").trim();
            if (errorNode) errorNode.textContent = String(error || "").trim();
        }

        function setScheduledSettingsFeedback(status, error = "") {
            const statusNode = document.getElementById(
                "scheduled-settings-status",
            );
            const errorNode = document.getElementById(
                "scheduled-settings-error",
            );
            if (statusNode)
                statusNode.textContent = String(status || "").trim();
            if (errorNode) errorNode.textContent = String(error || "").trim();
        }

        function setPolicyFieldInvalid(fieldId, isInvalid) {
            const input = document.getElementById(fieldId);
            if (!input) return;
            input.classList.toggle("is-invalid", !!isInvalid);
            if (isInvalid) {
                input.setAttribute("aria-invalid", "true");
            } else {
                input.removeAttribute("aria-invalid");
            }
        }

        function clearPolicyFieldErrors() {
            setPolicyFieldInvalid("policy-name", false);
            setPolicyFieldInvalid("policy-target-tag", false);
            [
                "policy-date-local",
                "policy-month-day",
                "policy-canary-servers",
                "policy-canary-count",
                "policy-wave-size",
                "policy-wave-delay",
            ].forEach((id) => setPolicyFieldInvalid(id, false));
        }

        function setPolicyWeekdays(weekdays) {
            scheduledPolicyAdministration.dispatch({
                type: "editorChanged",
                patch: {
                    weekdays,
                    cadence_kind:
                        document.getElementById("policy-cadence-kind")?.value ||
                        "daily",
                },
            });
            const selectedWeekdays =
                scheduledPolicyView().editor.draft.weekdays;
            document
                .querySelectorAll("#policy-weekdays-picker .day-chip")
                .forEach((button) => {
                    const day = button.dataset.weekday || "";
                    const isActive = selectedWeekdays.includes(day);
                    button.classList.toggle("active", isActive);
                    button.setAttribute(
                        "aria-pressed",
                        isActive ? "true" : "false",
                    );
                });
            updatePolicySummary();
            schedulePolicyPreview();
        }

        function togglePolicyWeekday(day) {
            scheduledPolicyAdministration.dispatch({
                type: "policyWeekdayToggled",
                day,
            });
            const weekdays = scheduledPolicyView().editor.draft.weekdays;
            document
                .querySelectorAll("#policy-weekdays-picker .day-chip")
                .forEach((button) => {
                    const active = weekdays.includes(
                        button.dataset.weekday || "",
                    );
                    button.classList.toggle("active", active);
                    button.setAttribute(
                        "aria-pressed",
                        active ? "true" : "false",
                    );
                });
            updatePolicySummary();
            schedulePolicyPreview();
        }

        function setPolicyEditorModeLabel(text) {
            const label = document.getElementById("policy-editor-mode");
            if (!label) return;
            label.textContent = text;
        }

        function refreshPolicyFormVisibility() {
            const cadence = document.getElementById(
                "policy-cadence-kind",
            ).value;
            const visibility = scheduledPolicyView().editor.visibility;
            document
                .getElementById("policy-date-section")
                ?.classList.toggle("is-hidden", !visibility.once);
            document
                .getElementById("policy-once-help")
                ?.classList.toggle("is-hidden", !visibility.once);
            document
                .getElementById("policy-month-section")
                ?.classList.toggle("is-hidden", !visibility.monthly);
            document.getElementById("policy-month-day").disabled =
                !visibility.monthDay;
            document
                .getElementById("policy-canary-count-wrap")
                ?.classList.toggle("is-hidden", visibility.explicitCanaries);
            document
                .getElementById("policy-canary-servers-wrap")
                ?.classList.toggle("is-hidden", !visibility.explicitCanaries);
            const executionMode = document.getElementById(
                "policy-execution-mode",
            ).value;
            const cleanup = executionMode === "autoremove";
            ["policy-package-scope", "policy-upgrade-mode"].forEach((id) => {
                document
                    .getElementById(`${id}-wrap`)
                    ?.classList.toggle("is-hidden", cleanup);
                document.getElementById(id).disabled = cleanup;
            });
            document
                .getElementById("policy-autoremove-help")
                ?.classList.toggle("is-hidden", !cleanup);
            const weekdaySection = document.getElementById(
                "policy-weekday-section",
            );
            const approvalWrap = document.getElementById(
                "policy-approval-timeout-wrap",
            );
            const rolloutFields = document.getElementById(
                "policy-rollout-fields",
            );
            if (weekdaySection) {
                weekdaySection.classList.toggle(
                    "is-hidden",
                    cadence !== "weekly",
                );
            }
            if (approvalWrap) {
                approvalWrap.classList.toggle(
                    "is-hidden",
                    executionMode !== "approval_required",
                );
            }
            if (rolloutFields) {
                rolloutFields.classList.toggle(
                    "is-hidden",
                    document.getElementById("policy-rollout-mode")?.value !==
                        "canary_waves",
                );
            }
            if (executionMode === "approval_required") {
                const timeoutInput = document.getElementById(
                    "policy-approval-timeout",
                );
                if (timeoutInput && !String(timeoutInput.value || "").trim()) {
                    timeoutInput.value = "720";
                }
            }
        }

        function updatePolicySummary() {
            const summary = document.getElementById("policy-summary");
            if (!summary) return;
            const projection = scheduledPolicyView().editor.summary;
            summary.innerHTML = `
        <div class="summary-title">${escapeHtml(projection.title)}</div>
		<div class="summary-body">${escapeHtml(projection.body)}</div>
	`;
            renderPolicyDraftState();
        }

        function renderPolicyDraftState() {
            const view = scheduledPolicyView();
            const editor = view.editor;
            const operationKey = String(editor.draft.id || "__new_policy__");
            const submitting =
                view.commands.inFlightPolicyIDs.includes(operationKey);
            const bar = document.getElementById("policy-draft-action-bar");
            const validation = document.getElementById(
                "policy-draft-validation",
            );
            const save = document.getElementById("policy-save-btn");
            const discard = document.getElementById("policy-discard-btn");
            if (bar) bar.hidden = !editor.dirty;
            if (validation) {
                validation.textContent = editor.valid
                    ? "Ready to save this section."
                    : editor.validationMessage ||
                      "Complete required fields to save.";
            }
            if (save) {
                save.disabled = !editor.dirty || !editor.valid || submitting;
                save.textContent = editor.draft.id
                    ? "Update Policy"
                    : "Create Policy";
            }
            if (discard) discard.disabled = !editor.dirty || submitting;
        }

        function renderGlobalSettingsDraftState() {
            const view = scheduledPolicyView();
            const state = view.globalSettings;
            const submitting = view.commands.globalSettingsInFlight;
            const label = document.getElementById(
                "scheduled-settings-draft-state",
            );
            const save = document.getElementById("scheduled-settings-save");
            const discard = document.getElementById(
                "scheduled-settings-discard",
            );
            if (label) {
                label.textContent = state.dirty
                    ? state.valid
                        ? "Unsaved global schedule changes"
                        : state.message
                    : "No unsaved changes";
                label.classList.toggle("is-dirty", state.dirty);
            }
            if (save) {
                save.disabled = !state.dirty || !state.valid || submitting;
                save.title = state.valid ? "" : state.message;
            }
            if (discard) discard.disabled = !state.dirty || submitting;
        }

        function policyPreviewReasonLabel(reason) {
            switch (String(reason || "")) {
                case "server_disabled":
                    return "server disabled";
                case "excluded_tag":
                    return "excluded tag";
                case "disabled_by_override":
                    return "override disabled";
                case "no_target_match":
                    return "no target match";
                default:
                    return "skipped";
            }
        }

        function renderPolicyPreviewList(
            items,
            emptyText,
            includeReason = false,
        ) {
            if (!Array.isArray(items) || !items.length) {
                return `<span class="subtle">${escapeHtml(emptyText)}</span>`;
            }
            return items
                .map((item) => {
                    const name = escapeHtml(item?.name || "");
                    const reason =
                        includeReason && item?.reason
                            ? ` · ${policyPreviewReasonLabel(item.reason)}`
                            : "";
                    const rollout =
                        !includeReason && item?.rollout_stage
                            ? ` · ${item.rollout_stage === "canary" ? "canary" : item.rollout_stage === "wave" ? `wave ${item.wave}` : "immediate"}`
                            : "";
                    const title =
                        Array.isArray(item?.tags) && item.tags.length
                            ? ` title="${escapeHtml(`Tags: ${item.tags.join(", ")}`)}"`
                            : "";
                    return `<span class="preview-chip${includeReason ? " preview-chip-muted" : ""}"${title}>${name}${escapeHtml(reason + rollout)}</span>`;
                })
                .join("");
        }

        function policyPreviewAdmissionLabel(outcome) {
            switch (String(outcome || "")) {
                case "admitted":
                    return "Expected: admitted";
                case "canary_unavailable":
                    return "Blocked: explicit canary unavailable";
                case "blocked_no_run":
                    return "Expected: blocked by no-run window";
                case "no_matching_servers":
                    return "Expected: no matching servers";
                case "policy_disabled":
                    return "Expected: policy disabled";
                default:
                    return "Expected outcome unavailable";
            }
        }

        function renderPolicyPreviewOccurrence(occurrence) {
            const matchedServerCount = Number.isFinite(
                Number(occurrence?.matched_server_count),
            )
                ? Number(occurrence.matched_server_count)
                : Array.isArray(occurrence?.matched_servers)
                  ? occurrence.matched_servers.length
                  : 0;
            const windows = Array.isArray(occurrence?.applicable_no_run_windows)
                ? occurrence.applicable_no_run_windows
                : [];
            const clock = [
                occurrence?.abbreviation,
                occurrence?.offset,
                occurrence?.timezone,
            ]
                .filter(Boolean)
                .join(" · ");
            const canonical =
                occurrence?.canonical_choice === "earlier_fallback_occurrence"
                    ? '<span class="preview-dst-note">Repeated local time · scheduler uses the earlier occurrence</span>'
                    : "";
            const noRun = windows.length
                ? `<div class="preview-no-run">${windows.map((window) => escapeHtml(`${window.source || "policy"} ${window.start_time || ""}-${window.end_time || ""}${window.overnight ? " overnight" : ""}`)).join(" · ")}</div>`
                : '<div class="table-secondary">No applicable no-run window</div>';
            return `
        <li class="preview-occurrence">
            <div class="preview-occurrence-primary">
                <div>
                    <strong>${escapeHtml(occurrence?.local_civil_time || "Local time unavailable")}</strong>
                    <div class="table-secondary">${escapeHtml(clock)}</div>
                </div>
                <span class="pill${occurrence?.admission_outcome === "admitted" ? "" : " pill-muted"}">${escapeHtml(policyPreviewAdmissionLabel(occurrence?.admission_outcome))}</span>
            </div>
            <div class="preview-utc">UTC ${escapeHtml(occurrence?.scheduled_for_utc || "unavailable")}</div>
            <div class="table-secondary">${escapeHtml(pluralize(matchedServerCount, "matching server", "matching servers"))}</div>
            ${noRun}
            ${canonical}
        </li>
    `;
        }

        function renderPolicyPreviewDiagnostics(elementId, items, className) {
            const element = document.getElementById(elementId);
            if (!element) return;
            element.innerHTML = (Array.isArray(items) ? items : [])
                .map(
                    (item) =>
                        `<div class="${className}">${escapeHtml(item?.message || item || "")}</div>`,
                )
                .join("");
        }

        function renderPolicyConflictWindow(window) {
            const effective = Boolean(window?.effective);
            const suppressedByNoRun =
                window?.draft_admission_outcome === "blocked_no_run" ||
                window?.competing_admission_outcome === "blocked_no_run";
            const outcome = effective
                ? "Effective overlap"
                : suppressedByNoRun
                  ? "Suppressed by no-run"
                  : "Inactive overlap";
            const admissions = [
                `Draft: ${policyPreviewAdmissionLabel(window?.draft_admission_outcome).replace("Expected: ", "")}`,
                `Competing: ${policyPreviewAdmissionLabel(window?.competing_admission_outcome).replace("Expected: ", "")}`,
            ].join(" · ");
            return `
        <li class="preview-conflict-window">
            <div class="preview-conflict-window-head">
                <strong>${escapeHtml(window?.local_civil_time || "Local time unavailable")}</strong>
                <span class="pill${effective ? " pill-warning" : " pill-muted"}">${escapeHtml(outcome)}</span>
            </div>
            <div class="table-secondary">${escapeHtml(window?.timezone || "Timezone unavailable")}</div>
            <div class="preview-utc">UTC ${escapeHtml(window?.window_start_utc || "unavailable")} – ${escapeHtml(window?.window_end_utc || "unavailable")}</div>
            <div class="table-secondary">${escapeHtml(admissions)}</div>
        </li>
    `;
        }

        function renderPolicyConflict(conflict) {
            const sharedServers = Array.isArray(conflict?.shared_servers)
                ? conflict.shared_servers
                : [];
            const windows = Array.isArray(conflict?.occurrence_windows)
                ? conflict.occurrence_windows
                : [];
            const overlapLabel =
                conflict?.overlap_kind === "full"
                    ? "Full target overlap"
                    : "Partial target overlap";
            return `
        <article class="preview-conflict">
            <div class="preview-conflict-head">
                <div>
                    <strong>${escapeHtml(conflict?.policy_name || "Unnamed policy")}</strong>
                    <div class="table-secondary">Policy #${escapeHtml(conflict?.policy_id ?? "unknown")}</div>
                </div>
                <span class="pill pill-warning">${escapeHtml(overlapLabel)}</span>
            </div>
            <div>
                <span class="table-shell-label">Shared servers</span>
                <div class="preview-list">${renderPolicyPreviewList(
                    sharedServers.map((name) => ({ name })),
                    "None",
                )}</div>
            </div>
            <ol class="preview-conflict-windows">
                ${windows.map(renderPolicyConflictWindow).join("")}
            </ol>
        </article>
    `;
        }

        function renderPolicyPreview(preview) {
            const matched = Array.isArray(preview?.matched_servers)
                ? preview.matched_servers
                : [];
            const excluded = Array.isArray(preview?.excluded_servers)
                ? preview.excluded_servers
                : [];
            const disabled = Array.isArray(preview?.disabled_by_override)
                ? preview.disabled_by_override
                : [];
            const occurrences = Array.isArray(preview?.upcoming_occurrences)
                ? preview.upcoming_occurrences
                : [];
            const conflicts = Array.isArray(preview?.schedule_conflicts)
                ? preview.schedule_conflicts
                : [];
            const validationErrors = Array.isArray(preview?.validation_errors)
                ? preview.validation_errors
                : [];
            const operationalWarnings = Array.isArray(
                preview?.operational_warnings,
            )
                ? preview.operational_warnings
                : Array.isArray(preview?.warnings)
                  ? preview.warnings
                  : [];
            const informationalFacts = Array.isArray(
                preview?.informational_facts,
            )
                ? preview.informational_facts
                : [];
            const skipped = [
                ...disabled.map((item) => ({
                    ...item,
                    reason: "disabled_by_override",
                })),
                ...excluded,
            ];
            document.getElementById("policy-preview-summary").textContent =
                matched.length
                    ? `${pluralize(matched.length, "server", "servers")} would match this policy.`
                    : "No current server would match this policy.";
            document.getElementById("policy-preview-count").textContent =
                `${matched.length} matched`;
            document.getElementById("policy-preview-matched").innerHTML =
                renderPolicyPreviewList(matched, "None");
            document.getElementById("policy-preview-skipped").innerHTML =
                renderPolicyPreviewList(skipped, "None", true);
            document.getElementById(
                "policy-preview-occurrence-count",
            ).textContent = `${occurrences.length} projected`;
            document.getElementById("policy-preview-occurrences").innerHTML =
                occurrences.length
                    ? occurrences.map(renderPolicyPreviewOccurrence).join("")
                    : '<li class="subtle">No canonical occurrence is available for this draft.</li>';
            document.getElementById(
                "policy-preview-conflict-count",
            ).textContent = pluralize(conflicts.length, "policy", "policies");
            document.getElementById("policy-preview-conflicts").innerHTML =
                conflicts.length
                    ? conflicts.map(renderPolicyConflict).join("")
                    : '<span class="subtle">No enabled policy overlap is projected.</span>';
            renderPolicyPreviewDiagnostics(
                "policy-preview-validation-errors",
                validationErrors,
                "preview-error",
            );
            renderPolicyPreviewDiagnostics(
                "policy-preview-warnings",
                operationalWarnings,
                "preview-warning",
            );
            renderPolicyPreviewDiagnostics(
                "policy-preview-facts",
                informationalFacts,
                "preview-fact",
            );
        }

        function setPolicyPreviewMessage(message, countText = "0 matched") {
            document.getElementById("policy-preview-summary").textContent =
                message;
            document.getElementById("policy-preview-count").textContent =
                countText;
            document.getElementById("policy-preview-matched").innerHTML =
                '<span class="subtle">None</span>';
            document.getElementById("policy-preview-skipped").innerHTML =
                '<span class="subtle">None</span>';
            document.getElementById(
                "policy-preview-occurrence-count",
            ).textContent = "0 projected";
            document.getElementById("policy-preview-occurrences").innerHTML =
                '<li class="subtle">Complete the policy fields to project its schedule.</li>';
            document.getElementById(
                "policy-preview-conflict-count",
            ).textContent = "0 policies";
            document.getElementById("policy-preview-conflicts").innerHTML =
                '<span class="subtle">Complete the policy fields to project competing policy overlaps.</span>';
            renderPolicyPreviewDiagnostics(
                "policy-preview-validation-errors",
                [],
                "preview-error",
            );
            renderPolicyPreviewDiagnostics(
                "policy-preview-warnings",
                [],
                "preview-warning",
            );
            renderPolicyPreviewDiagnostics(
                "policy-preview-facts",
                [],
                "preview-fact",
            );
        }

        async function refreshPolicyPreview() {
            const lifetime = lifecycle.capture();

            const effect = scheduledPolicyAdministration
                .dispatch({ type: "previewRequested" })
                .find((item) => item.type === "fetchPreview");
            if (!effect) {
                setPolicyPreviewMessage(
                    scheduledPolicyView().editor.preview.message ||
                        "Complete policy fields to preview matching servers.",
                );
                return;
            }
            setPolicyPreviewMessage("Refreshing target preview...", "...");
            try {
                const res = await fetch("/api/update-policies/preview", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify(effect.payload),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    throw new Error(
                        await parseErrorResponse(
                            res,
                            "Failed to preview scheduled policy.",
                        ),
                    );
                }
                const data = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                scheduledPolicyAdministration.dispatch({
                    type: "previewReceived",
                    requestId: effect.requestId,
                    preview: data,
                });
                renderPolicyPreview(scheduledPolicyView().editor.preview.data);
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                scheduledPolicyAdministration.dispatch({
                    type: "previewFailed",
                    requestId: effect.requestId,
                    error: err.message || "Failed to preview scheduled policy.",
                });
                setPolicyPreviewMessage(
                    scheduledPolicyView().editor.preview.message,
                );
            }
        }

        function schedulePolicyPreview() {
            lifecycle.clearTimeout(scheduledPolicyPreviewTimer);
            scheduledPolicyPreviewTimer = lifecycle.setTimeout(
                refreshPolicyPreview,
                250,
            );
        }

        function formatCadence(policy) {
            const timeLocal = policy.time_local || "--:--";
            if (policy.cadence_kind === "weekly") {
                return `Every ${formatWeekdayList(policy.weekdays)} at ${timeLocal}`;
            }
            if (policy.cadence_kind === "once")
                return `Once on ${policy.date_local || "a local date"} at ${timeLocal}`;
            if (policy.cadence_kind === "monthly")
                return `Monthly on ${policy.month_last_day ? "the last day" : `day ${policy.month_day}`} at ${timeLocal}`;
            return `Daily at ${timeLocal}`;
        }

        function renderPolicyExecution(policy) {
            const mode = humanizeExecutionMode(policy.execution_mode);
            const scope =
                policy.execution_mode === "autoremove"
                    ? "Remove unused packages automatically"
                    : humanizePackageScope(policy.package_scope);
            const upgradeMode =
                policy.execution_mode === "autoremove"
                    ? ""
                    : ` · ${humanizeUpgradeMode(policy.upgrade_mode || "standard")}`;
            const timeout =
                policy.execution_mode === "approval_required"
                    ? ` · ${policy.approval_timeout_minutes || 720} minute approval window`
                    : "";
            const rollout =
                policy.rollout_mode === "canary_waves"
                    ? ` · canary ${policy.canary_mode === "explicit" ? (policy.canary_servers || []).join(", ") : policy.canary_count || 1}, waves of ${policy.wave_size || 10} / ${policy.wave_delay_minutes || 15} min`
                    : " · immediate rollout";
            return `
		<div>${escapeHtml(mode)}</div>
		<div class="table-secondary">${escapeHtml(`${scope}${upgradeMode}${timeout}${rollout}`)}</div>
	`;
        }

        function renderPolicySchedule(policy) {
            const noRunCount = Array.isArray(policy.policy_blackouts)
                ? policy.policy_blackouts.length
                : 0;
            const noRunText = noRunCount
                ? `${pluralize(noRunCount, "policy no-run window", "policy no-run windows")}`
                : "No policy no-run windows";
            const timezoneText = scheduledPolicyView().timezone
                ? `App timezone: ${scheduledPolicyView().timezone}`
                : "";
            const detailText = [
                noRunText,
                timezoneText,
                policy.once_consumed_at
                    ? `One-time occurrence consumed ${policy.once_consumed_at}`
                    : "",
            ]
                .filter(Boolean)
                .join(" · ");
            return `
        <div>${escapeHtml(formatCadence(policy))}</div>
        <div class="table-secondary">${escapeHtml(detailText)}</div>
    `;
        }

        function renderMatchedServers(policy) {
            const matchedServers = Array.isArray(policy.matched_servers)
                ? policy.matched_servers
                : [];
            if (!matchedServers.length) {
                const emptyMessage =
                    policy && policy.enabled === false
                        ? "Disabled policies do not match servers until enabled."
                        : "No current server matches this target.";
                return `
            <div><span class="pill pill-muted">0 matched</span></div>
            <div class="table-secondary">${escapeHtml(emptyMessage)}</div>
        `;
            }
            return `
        <div><span class="pill">${escapeHtml(pluralize(matchedServers.length, "matched server", "matched servers"))}</span></div>
        <div class="table-secondary">${escapeHtml(matchedServers.join(", "))}</div>
    `;
        }

        function formatPolicyTargets(policy) {
            const bits = [];
            if (String(policy?.target_tag || "").trim())
                bits.push(`tag ${policy.target_tag}`);
            if (
                Array.isArray(policy?.include_tags) &&
                policy.include_tags.length
            )
                bits.push(`include ${policy.include_tags.join(", ")}`);
            if (
                Array.isArray(policy?.exclude_tags) &&
                policy.exclude_tags.length
            )
                bits.push(`exclude ${policy.exclude_tags.join(", ")}`);
            if (
                Array.isArray(policy?.target_servers) &&
                policy.target_servers.length
            )
                bits.push(`servers ${policy.target_servers.join(", ")}`);
            return bits.length ? bits.join(" / ") : "No targets";
        }

        function safeRunStatusClassToken(status) {
            const normalized = String(status || "unknown")
                .toLowerCase()
                .replace(/[^a-z0-9_-]/g, "-");
            switch (normalized) {
                case "queued":
                case "running":
                case "waiting_approval":
                case "succeeded":
                case "failed":
                case "skipped":
                case "cancelled":
                case "interrupted":
                    return normalized;
                default:
                    return "unknown";
            }
        }

        const jobPhaseOrder = [
            "dial",
            "prechecks",
            "apt_update",
            "approval_wait",
            "apt_upgrade",
            "autoremove",
            "apply",
            "postchecks",
            "snapshot",
            "encrypt",
            "decrypt",
            "lookup",
            "complete",
        ];

        function formatJobPhaseLabel(phase) {
            return String(phase || "unknown")
                .replace(/_/g, " ")
                .replace(/\b\w/g, (char) => char.toUpperCase());
        }

        function formatJobTimestamp(value) {
            if (!String(value || "").trim()) return "-";
            const formatted = window.formatAppTimestamp
                ? window.formatAppTimestamp(value, { includeUTC: true })
                : { primary: value, secondary: "", title: value };
            return formatted.secondary
                ? `${formatted.primary} (${formatted.secondary})`
                : formatted.primary;
        }

        function prettyJobJSON(raw) {
            const text = String(raw || "").trim();
            if (!text) return "{}";
            try {
                return JSON.stringify(JSON.parse(text), null, 2);
            } catch (_) {
                if (!lifecycle.active || _?.name === "AbortError") return;
                return text;
            }
        }

        function renderJobPhaseTimeline(job) {
            const container = document.getElementById("job-detail-phases");
            if (!container) return;
            const currentPhase = String(job?.phase || "").trim();
            const phases = jobPhaseOrder.includes(currentPhase)
                ? jobPhaseOrder
                : [...jobPhaseOrder, currentPhase].filter(Boolean);
            const currentIndex = phases.indexOf(currentPhase);
            container.innerHTML = "";
            phases.forEach((phase, index) => {
                const item = document.createElement("span");
                item.className = "job-phase-step";
                if (currentIndex >= 0 && index < currentIndex)
                    item.classList.add("is-complete");
                if (phase === currentPhase) item.classList.add("is-current");
                item.textContent = formatJobPhaseLabel(phase);
                container.appendChild(item);
            });
        }

        function renderJobDetail(job, reportURL) {
            scheduledPolicyAdministration.dispatch({
                type: "jobReceived",
                job,
                data: job,
            });
            document.getElementById("job-detail-title").textContent =
                `Job ${job.id || ""}`;
            document.getElementById("job-detail-status").innerHTML =
                `<span class="status-chip status-${safeRunStatusClassToken(job.status)}">${escapeHtml(job.status || "unknown")}</span>`;
            document.getElementById("job-detail-phase").textContent =
                formatJobPhaseLabel(job.phase);
            document.getElementById("job-detail-kind").textContent =
                job.kind || "-";
            document.getElementById("job-detail-server").textContent =
                job.server_name || "-";
            document.getElementById("job-detail-actor").textContent =
                job.actor || "-";
            document.getElementById("job-detail-client-ip").textContent =
                job.client_ip || "-";
            document.getElementById("job-detail-created").textContent =
                formatJobTimestamp(job.created_at);
            document.getElementById("job-detail-updated").textContent =
                formatJobTimestamp(job.updated_at);
            document.getElementById("job-detail-started").textContent =
                formatJobTimestamp(job.started_at);
            document.getElementById("job-detail-finished").textContent =
                formatJobTimestamp(job.finished_at);
            document.getElementById("job-detail-summary").textContent =
                job.summary || "-";
            document.getElementById("job-detail-retry").textContent =
                prettyJobJSON(job.retry_policy_json);
            document.getElementById("job-detail-meta").textContent =
                prettyJobJSON(job.meta_json);
            document.getElementById("job-detail-logs").textContent =
                job.logs_text || "";
            document.getElementById("job-detail-report").href =
                reportURL ||
                `/api/reports/jobs/${encodeURIComponent(job.id || "")}`;
            renderJobPhaseTimeline(job);
        }

        function closeJobDetailModal() {
            const modal = document.getElementById("job-detail-modal");
            if (!modal) return;
            modal.classList.remove("active");
            scheduledPolicyAdministration.dispatch({ type: "jobClosed" });
        }

        async function openJobDetail(jobID) {
            const lifetime = lifecycle.capture();

            const cleanID = String(jobID || "").trim();
            if (!cleanID) return;
            const request = scheduledPolicyAdministration
                .dispatch({ type: "jobSelected", jobID: cleanID })
                .find((effect) => effect.type === "fetchSnapshot");
            if (!request) return;
            try {
                const res = await fetch(
                    `/api/jobs/${encodeURIComponent(cleanID)}`,
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    const message = await parseErrorResponse(
                        res,
                        "Failed to load job details.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    window.notifyApp(message);
                    return;
                }
                const data = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!data.job) {
                    window.notifyApp("Job details were not returned.");
                    return;
                }
                scheduledPolicyAdministration.dispatch({
                    type: "jobReceived",
                    requestId: request.requestId,
                    job: data.job,
                    data: data.job,
                });
                renderJobDetail(
                    scheduledPolicyView().selectedJob,
                    data.report_url,
                );
                document
                    .getElementById("job-detail-modal")
                    .classList.add("active");
                document
                    .getElementById("job-detail-close")
                    .focus({ preventScroll: true });
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                scheduledPolicyAdministration.dispatch({
                    type: "snapshotFailed",
                    stream: "job",
                    requestId: request.requestId,
                    error: err.message || "Failed to load job details.",
                });
                console.error("Failed to load job details:", err);
                window.notifyApp("Failed to load job details.");
            }
        }

        async function copyJobDetailText(kind) {
            const lifetime = lifecycle.capture();

            const job = scheduledPolicyView().selectedJob;
            if (!job) return;
            const text =
                kind === "logs"
                    ? job.logs_text || ""
                    : `Job ${job.id}\nStatus: ${job.status || "unknown"}\nPhase: ${job.phase || "unknown"}\nSummary: ${job.summary || ""}`;
            if (!String(text || "").trim()) {
                window.notifyApp("Nothing to copy.");
                return;
            }
            try {
                await navigator.clipboard.writeText(text);
                if (!lifecycle.isCurrent(lifetime)) return;
                window.notifyApp(
                    kind === "logs"
                        ? "Job logs copied."
                        : "Job summary copied.",
                );
            } catch (_) {
                if (!lifecycle.isCurrent(lifetime) || _?.name === "AbortError")
                    return;
                window.notifyApp(
                    "Failed to copy. Select the text and copy it manually.",
                );
            }
        }

        let packageDeferralEditor;

        function deferralEditor() {
            return (packageDeferralEditor ||= window.PackageDeferrals.mount(
                document.getElementById("policy-package-deferrals"),
            ));
        }

        function resetPolicyForm(options = {}) {
            void deferralEditor()?.clear();
            if (options.updateStore !== false)
                scheduledPolicyAdministration.dispatch({ type: "editorReset" });
            document.getElementById("policy-id").value = "";
            document.getElementById("policy-name").value = "";
            document.getElementById("policy-target-tag").value = "";
            document.getElementById("policy-include-tags").value = "";
            document.getElementById("policy-exclude-tags").value = "";
            document.getElementById("policy-target-servers").value = "";
            document.getElementById("policy-time-local").value = "02:00";
            document.getElementById("policy-date-local").value = "";
            document.getElementById("policy-month-day").value = "1";
            document.getElementById("policy-month-last-day").checked = false;
            document.getElementById("policy-canary-mode").value = "automatic";
            document.getElementById("policy-canary-servers").value = "";
            document.getElementById("policy-execution-mode").value =
                "scan_only";
            document.getElementById("policy-package-scope").value = "security";
            document.getElementById("policy-upgrade-mode").value = "standard";
            document.getElementById("policy-cadence-kind").value = "daily";
            document.getElementById("policy-approval-timeout").value = "720";
            document.getElementById("policy-rollout-mode").value = "immediate";
            document.getElementById("policy-canary-count").value = "1";
            document.getElementById("policy-wave-size").value = "10";
            document.getElementById("policy-wave-delay").value = "15";
            document.getElementById("policy-enabled").checked = true;
            clearPolicyFieldErrors();
            setPolicyFeedback("", "");
            setPolicyEditorModeLabel("Create new policy");
            setPolicyWeekdays([]);
            if (options.updateStore !== false)
                setBlackoutEditorRows("policy", []);
            else renderBlackoutEditor("policy");
            setBlackoutJsonStatus("policy", "");
            refreshPolicyFormVisibility();
            updatePolicySummary();
            schedulePolicyPreview();
            renderPolicyDraftState();
        }

        function applyPolicyToForm(policy, options = {}) {
            void deferralEditor()?.select(
                policy.id ? { policy_id: Number(policy.id) } : null,
            );
            if (options.updateStore !== false)
                scheduledPolicyAdministration.dispatch({
                    type: "editorLoaded",
                    policy,
                });
            document.getElementById("policy-id").value = String(
                policy.id || "",
            );
            document.getElementById("policy-name").value = policy.name || "";
            document.getElementById("policy-target-tag").value =
                policy.target_tag || "";
            document.getElementById("policy-include-tags").value = (
                policy.include_tags || []
            ).join(", ");
            document.getElementById("policy-exclude-tags").value = (
                policy.exclude_tags || []
            ).join(", ");
            document.getElementById("policy-target-servers").value = (
                policy.target_servers || []
            ).join(", ");
            document.getElementById("policy-time-local").value =
                policy.time_local || "02:00";
            document.getElementById("policy-date-local").value =
                policy.date_local || "";
            document.getElementById("policy-month-day").value =
                policy.month_day || 1;
            document.getElementById("policy-month-last-day").checked =
                !!policy.month_last_day;
            document.getElementById("policy-canary-mode").value =
                policy.canary_mode || "automatic";
            document.getElementById("policy-canary-servers").value = (
                policy.canary_servers || []
            ).join(", ");
            document.getElementById("policy-execution-mode").value =
                policy.execution_mode || "scan_only";
            document.getElementById("policy-package-scope").value =
                policy.package_scope || "security";
            document.getElementById("policy-upgrade-mode").value =
                policy.upgrade_mode || "standard";
            document.getElementById("policy-cadence-kind").value =
                policy.cadence_kind || "daily";
            document.getElementById("policy-approval-timeout").value =
                policy.approval_timeout_minutes || 720;
            document.getElementById("policy-rollout-mode").value =
                policy.rollout_mode || "immediate";
            document.getElementById("policy-canary-count").value =
                policy.canary_count || 1;
            document.getElementById("policy-wave-size").value =
                policy.wave_size || 10;
            document.getElementById("policy-wave-delay").value =
                policy.wave_delay_minutes || 15;
            document.getElementById("policy-enabled").checked =
                !!policy.enabled;
            clearPolicyFieldErrors();
            setPolicyFeedback("", "");
            setPolicyWeekdays(policy.weekdays || []);
            if (options.updateStore !== false)
                setBlackoutEditorRows("policy", policy.policy_blackouts || []);
            else renderBlackoutEditor("policy");
            setBlackoutJsonStatus("policy", "");
            setPolicyEditorModeLabel(`Editing #${policy.id}`);
            refreshPolicyFormVisibility();
            updatePolicySummary();
            schedulePolicyPreview();
            renderPolicyDraftState();
        }

        function applyPolicyReplacementToDOM(replacement) {
            if (replacement?.type === "load") {
                const draft = scheduledPolicyView().editor.draft;
                applyPolicyToForm(
                    {
                        ...draft,
                        policy_blackouts:
                            scheduledPolicyView().editor.policyBlackouts,
                    },
                    { updateStore: false },
                );
            } else {
                resetPolicyForm({ updateStore: false });
            }
            document
                .getElementById("update-policy-form")
                ?.scrollIntoView({ behavior: "smooth", block: "start" });
        }

        async function requestPolicyReplacement(replacement, trigger) {
            const lifetime = lifecycle.capture();

            const effects = scheduledPolicyAdministration.dispatch({
                type: "editorReplacementRequested",
                replacement,
            });
            const confirmation = effects.find(
                (effect) => effect.type === "confirmEditorReplacement",
            );
            if (confirmation) {
                trigger?.focus({ preventScroll: true });
                const editor = scheduledPolicyView().editor;
                const confirmed = await window.confirmAction(
                    "Review the unsaved browser draft before replacing it.",
                    {
                        danger: true,
                        title: "Discard unsaved policy changes",
                        operation:
                            replacement?.type === "load"
                                ? "Open another policy"
                                : "Start a new policy draft",
                        resources: editor.draft.name
                            ? `Unsaved draft "${editor.draft.name}"`
                            : "Current unsaved policy draft",
                        consequences:
                            "All unsaved policy fields and no-run window edits in this browser are discarded.",
                        reversibility:
                            "Not reversible unless the same values are entered again.",
                        authentication:
                            "No additional authentication is required because no accepted server data is changed.",
                        confirmLabel: "Discard Changes",
                    },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!confirmed) return;
                scheduledPolicyAdministration.dispatch({
                    type: "editorReplacementConfirmed",
                    replacement: confirmation.replacement,
                });
            }
            applyPolicyReplacementToDOM(replacement);
        }

        function discardPolicyDraft() {
            scheduledPolicyAdministration.dispatch({
                type: "editorDiscardRequested",
            });
            const editor = scheduledPolicyView().editor;
            const replacement = editor.draft.id
                ? {
                      type: "load",
                      policy: {
                          ...editor.draft,
                          policy_blackouts: editor.policyBlackouts,
                      },
                  }
                : { type: "reset" };
            applyPolicyReplacementToDOM(replacement);
        }

        function discardGlobalSettingsDraft() {
            scheduledPolicyAdministration.dispatch({
                type: "globalSettingsDiscardRequested",
            });
            renderBlackoutEditor("global");
            setScheduledSettingsFeedback("", "");
            renderGlobalSettingsDraftState();
        }

        function renderScheduledPolicies() {
            const tbody = document.querySelector(
                "#scheduled-policy-table tbody",
            );
            if (!tbody) return;
            tbody.innerHTML = "";
            const policies = scheduledPolicyView().policies;
            if (!policies.length) {
                const row = document.createElement("tr");
                row.innerHTML =
                    '<td colspan="5" class="subtle">No scheduled update policies yet.</td>';
                tbody.appendChild(row);
                renderAdminWorkspace();
                return;
            }
            policies.forEach((policy) => {
                const row = document.createElement("tr");
                row.innerHTML = `
            <td>
                <div class="table-title-row">
                    <div>${escapeHtml(policy.name || "")}</div>
                    <span class="pill ${policy.enabled ? "" : "pill-muted"}">${policy.enabled ? "Enabled" : "Disabled"}</span>
                </div>
                <div class="table-secondary">Targets: ${escapeHtml(formatPolicyTargets(policy))}</div>
            </td>
            <td>${renderPolicySchedule(policy)}</td>
            <td>${renderPolicyExecution(policy)}</td>
            <td>${renderMatchedServers(policy)}</td>
            <td>
                <div class="table-actions">
                    <button class="btn-ghost" type="button" data-action="edit-policy" data-id="${escapeHtml(String(policy.id))}">Edit</button>
                    <span class="admin-danger-action-group">
                        <button class="btn-danger" type="button" data-action="delete-policy" data-admin-danger-command="scheduled:deletePolicy" data-id="${escapeHtml(String(policy.id))}">Delete</button>
                    </span>
                </div>
            </td>
        `;
                tbody.appendChild(row);
            });
            renderAdminWorkspace();
        }

        function renderMaintenanceCalendarFilter() {
            const select = document.getElementById(
                "maintenance-calendar-policy",
            );
            if (!select) return;
            const current =
                scheduledPolicyView().selectedCalendarPolicyID || select.value;
            select.innerHTML = '<option value="">All policies</option>';
            scheduledPolicyView().policies.forEach((policy) => {
                const option = document.createElement("option");
                option.value = String(policy.id || "");
                option.textContent = policy.name || `Policy ${policy.id}`;
                select.appendChild(option);
            });
            if (
                current &&
                Array.from(select.options).some(
                    (option) => option.value === current,
                )
            ) {
                select.value = current;
            }
        }

        function formatCalendarDate(day) {
            const date = String(day?.date || "");
            const weekday = formatWeekdayLabel(day?.weekday || "");
            return [weekday, date].filter(Boolean).join(" ");
        }

        function formatRolloutBatches(batches) {
            return (batches || [])
                .map(
                    (batch) =>
                        `${batch.stage === "canary" ? "Canary" : batch.stage === "wave" ? `Wave ${batch.index}` : "All servers"}: ${(batch.servers || []).join(", ")}${batch.release_delay_minutes ? ` (+${batch.release_delay_minutes} min)` : ""}`,
                )
                .join(" · ");
        }

        function renderCalendarSlot(slot) {
            const serverCount = Array.isArray(slot?.matched_servers)
                ? slot.matched_servers.length
                : 0;
            const details = [
                slot?.timezone_offset || "",
                humanizeExecutionMode(slot?.execution_mode),
                slot?.execution_mode === "autoremove"
                    ? "Remove unused packages"
                    : humanizePackageScope(slot?.package_scope),
                pluralize(serverCount, "server", "servers"),
                formatRolloutBatches(slot?.rollout_batches),
            ]
                .filter(Boolean)
                .join(" · ");
            return `
        <span class="calendar-chip calendar-chip-allowed" title="${escapeHtml(details)}">
            ${escapeHtml(`Allowed ${slot?.time_local || "--:--"} ${slot?.timezone_offset || ""}`)}
            ${slot?.rollout_batches?.length ? `<span class="table-secondary">${escapeHtml(formatRolloutBatches(slot.rollout_batches))}</span>` : ""}
        </span>
    `;
        }

        function renderCalendarBlockedWindow(window) {
            const source = window?.source === "policy" ? "policy" : "global";
            const overnight = window?.overnight ? " overnight" : "";
            const applies = window?.applies_to_slot ? " applies to slot" : "";
            const days = formatWeekdayList(window?.weekdays || []);
            const title = `${source} ${days}${overnight}${applies}`;
            return `
        <span class="calendar-chip calendar-chip-blocked${window?.applies_to_slot ? " is-active" : ""}" title="${escapeHtml(title)}">
            ${escapeHtml(`${source} ${window?.start_time || "--:--"}-${window?.end_time || "--:--"}${overnight}`)}
        </span>
    `;
        }

        function calendarBlockedReason(reason) {
            return (
                {
                    month_day_missing:
                        "This month has no selected day; occurrence skipped",
                    once_consumed: "One-time occurrence consumed",
                    nonexistent_local_time:
                        "Local time does not exist; occurrence skipped",
                }[reason] || reason
            );
        }

        function renderMaintenanceCalendar(calendar) {
            const container = document.getElementById(
                "maintenance-calendar-list",
            );
            const status = document.getElementById(
                "maintenance-calendar-status",
            );
            if (!container) return;
            const policies = Array.isArray(calendar?.policies)
                ? calendar.policies
                : [];
            const acceptedRollouts = Array.isArray(calendar?.accepted_rollouts)
                ? calendar.accepted_rollouts
                : [];
            if (status) {
                const range =
                    calendar?.start_date && calendar?.end_date
                        ? `${calendar.start_date} to ${calendar.end_date}`
                        : "";
                const tz =
                    calendar?.timezone ||
                    scheduledPolicyView().timezone ||
                    "UTC";
                status.textContent = range
                    ? `${range} · ${tz}`
                    : `Calendar timezone: ${tz}`;
            }
            if (!policies.length && !acceptedRollouts.length) {
                container.innerHTML =
                    '<div class="empty-editor-state subtle">No scheduled policies to show.</div>';
                return;
            }
            const accepted = acceptedRollouts
                .map(
                    (occurrence) =>
                        `<div class="calendar-policy"><strong>${escapeHtml(occurrence.policy?.name || "Accepted rollout")} · accepted revision ${escapeHtml(String(occurrence.policy?.revision || 1))}</strong><div class="table-secondary">UTC ${escapeHtml(occurrence.scheduled_for_utc || "")} · ${escapeHtml(formatRolloutBatches(occurrence.batches))}</div>${occurrence.block_reason ? `<div class="run-skip-reason">${escapeHtml(occurrence.block_reason)}</div>` : ""}</div>`,
                )
                .join("");
            container.innerHTML =
                accepted +
                policies
                    .map((policy) => {
                        const days = Array.isArray(policy.days)
                            ? policy.days
                            : [];
                        const matchedCount = Array.isArray(
                            policy.matched_servers,
                        )
                            ? policy.matched_servers.length
                            : 0;
                        return `
            <div class="calendar-policy">
                <div class="calendar-policy-head">
                    <div>
                        <strong>${escapeHtml(policy.name || "")}</strong>
                        ${policy.once_consumed_at ? `<div class="run-skip-reason">One-time occurrence consumed ${escapeHtml(policy.once_consumed_at)}</div>` : ""}
                        ${policy.admission_reason ? `<div class="run-skip-reason">${escapeHtml(policy.admission_reason)}</div>` : ""}
                        <div class="table-secondary">${escapeHtml(`${formatCadence(policy)} · ${pluralize(matchedCount, "matched server", "matched servers")}`)}</div>
                    </div>
                    <span class="pill ${policy.enabled ? "" : "pill-muted"}">${policy.enabled ? "Enabled" : "Disabled"}</span>
                </div>
                <div class="calendar-day-grid">
                    ${days
                        .map((day) => {
                            const slots = Array.isArray(day.allowed_slots)
                                ? day.allowed_slots
                                : [];
                            const windows = Array.isArray(day.blocked_windows)
                                ? day.blocked_windows
                                : [];
                            const reasons = Array.isArray(day.blocked_reasons)
                                ? day.blocked_reasons
                                : [];
                            return `
                            <div class="calendar-day">
                                <div class="calendar-day-head">
                                    <span>${escapeHtml(formatCalendarDate(day))}</span>
                                    <span class="table-secondary">${escapeHtml(day.timezone_offset || "")}</span>
                                </div>
                                <div class="calendar-chip-row">
                                    ${slots.length ? slots.map(renderCalendarSlot).join("") : ""}
                                    ${windows.length ? windows.map(renderCalendarBlockedWindow).join("") : ""}
                                    ${!slots.length && !windows.length ? '<span class="subtle">No scheduled slot or no-run window</span>' : ""}
                                </div>
                                ${reasons.length ? `<div class="table-secondary">${escapeHtml(`Blocked: ${reasons.map(calendarBlockedReason).join(", ")}`)}</div>` : ""}
                            </div>
                        `;
                        })
                        .join("")}
                </div>
            </div>
        `;
                    })
                    .join("");
        }

        function formatRunDuration(milliseconds) {
            const totalSeconds = Math.max(
                0,
                Math.round(Number(milliseconds) / 1000),
            );
            if (!Number.isFinite(totalSeconds)) return "Not finished";
            const hours = Math.floor(totalSeconds / 3600);
            const minutes = Math.floor((totalSeconds % 3600) / 60);
            const seconds = totalSeconds % 60;
            if (hours) return `${hours}h ${minutes}m ${seconds}s`;
            if (minutes) return `${minutes}m ${seconds}s`;
            return `${seconds}s`;
        }

        function scheduledRunTimestamp(run, field, displayField) {
            const raw = String(run?.[field] || "").trim();
            if (!raw) return null;
            const resolvedTimezone = window.getAppTimezoneResolved
                ? window.getAppTimezoneResolved()
                : "";
            const options = { includeUTC: true };
            if (!resolvedTimezone && String(run?.[displayField] || "").trim()) {
                options.preformattedPrimary = run[displayField];
            }
            return window.formatAppTimestamp
                ? window.formatAppTimestamp(raw, options)
                : { primary: raw, secondary: "", title: raw };
        }

        function renderScheduledRuns() {
            const tbody = document.querySelector("#scheduled-runs-table tbody");
            if (!tbody) return;
            const history = scheduledPolicyView().runHistory || {};
            const runs = Array.isArray(history.items) ? history.items : [];
            const query = history.query || {};
            const total = Number(history.total) || 0;
            const page = Number(history.page) || 1;
            const totalPages = Number(history.totalPages) || 0;
            const resultSummary = document.getElementById(
                "scheduled-runs-result-summary",
            );
            const pageInfo = document.getElementById(
                "scheduled-runs-page-info",
            );
            const previous = document.getElementById("scheduled-runs-prev");
            const next = document.getElementById("scheduled-runs-next");
            if (resultSummary)
                resultSummary.textContent = `${total} matching run${total === 1 ? "" : "s"}.`;
            if (pageInfo)
                pageInfo.textContent = totalPages
                    ? `Page ${page} of ${totalPages}`
                    : "Page 1 of 1";
            if (previous) previous.disabled = page <= 1;
            if (next) next.disabled = totalPages === 0 || page >= totalPages;
            tbody.innerHTML = "";
            if (!runs.length) {
                const row = document.createElement("tr");
                const hasFilters = [
                    query.policy,
                    query.server,
                    query.outcome,
                    query.from,
                    query.to,
                ].some(Boolean);
                row.innerHTML = `<td colspan="6" class="subtle" data-label="Result">${hasFilters ? "No scheduled runs match these filters." : "No scheduled runs recorded yet."}</td>`;
                tbody.appendChild(row);
                renderAdminWorkspace();
                return;
            }
            runs.forEach((run) => {
                const row = document.createElement("tr");
                const outcome = run.terminal_outcome || run.status || "unknown";
                const statusToken = safeRunStatusClassToken(outcome);
                const scheduled = scheduledRunTimestamp(
                    run,
                    "scheduled_for_utc",
                    "scheduled_for_display",
                ) || { primary: "-", secondary: "", title: "" };
                const started = scheduledRunTimestamp(
                    run,
                    "started_at",
                    "started_at_display",
                );
                const finished = scheduledRunTimestamp(
                    run,
                    "finished_at",
                    "finished_at_display",
                );
                const duration =
                    run.duration_ms === undefined || run.duration_ms === null
                        ? "Not finished"
                        : formatRunDuration(run.duration_ms);
                const jobDetailURL =
                    run.job_detail_url ||
                    (run.job_id
                        ? `/api/jobs/${encodeURIComponent(run.job_id)}`
                        : "");
                const reportURL =
                    run.report_url ||
                    (run.job_id
                        ? `/api/reports/jobs/${encodeURIComponent(run.job_id)}`
                        : "");
                const auditURL = run.audit_url || "";
                row.innerHTML = `
            <td data-label="Scheduled" title="${escapeHtml(scheduled.title || "")}">
                <div>${escapeHtml(scheduled.primary || "")}</div>
                ${scheduled.secondary ? `<div class="table-secondary">${escapeHtml(scheduled.secondary)}</div>` : ""}
            </td>
            <td data-label="Policy &amp; server">
                <strong>${escapeHtml(run.policy_name || "-")}</strong>
                <div class="table-secondary">${escapeHtml(run.server_name || "-")}</div>
                ${run.policy_revision ? `<div class="table-secondary">Revision ${escapeHtml(String(run.policy_revision))} · ${escapeHtml(run.rollout_stage === "wave" ? `wave ${run.wave}` : run.rollout_stage || "immediate")}</div>` : ""}
            </td>
            <td data-label="Lifecycle">
                <div class="run-lifecycle">
                    <span><strong>Started:</strong> ${escapeHtml(started?.primary || "Not started")}</span>
                    <span><strong>Finished:</strong> ${escapeHtml(finished?.primary || "Not finished")}</span>
                    <span><strong>Duration:</strong> ${escapeHtml(duration)}</span>
                </div>
            </td>
            <td data-label="Outcome">
                <span class="status-chip status-${statusToken}">${escapeHtml(outcome)}</span>
                ${run.exact_skip_reason ? `<div class="run-skip-reason">Reason: ${escapeHtml(run.exact_skip_reason)}</div>` : ""}
            </td>
            <td data-label="Summary">${escapeHtml(run.summary || run.reason || "-")}</td>
            <td data-label="Investigate">
                <div class="run-investigation-links">
                    ${jobDetailURL ? `<button class="inline-btn btn-ghost" type="button" data-action="job-detail" data-job-id="${escapeHtml(String(run.job_id))}">Job details</button>` : ""}
                    ${reportURL ? `<a class="inline-btn btn-ghost" href="${escapeHtml(reportURL)}">Report</a>` : ""}
                    ${auditURL ? `<a class="inline-btn btn-ghost" href="${escapeHtml(auditURL)}">Audit trail</a>` : ""}
                    ${!jobDetailURL && !reportURL && !auditURL ? '<span class="subtle">No linked details</span>' : ""}
                </div>
            </td>
        `;
                tbody.appendChild(row);
            });
            renderAdminWorkspace();
        }

        function handleScheduledRunsTableClick(event) {
            const detailButton = event.target.closest(
                "[data-action='job-detail']",
            );
            if (!detailButton) return;
            openJobDetail(detailButton.dataset.jobId);
        }

        async function fetchScheduledPolicies(request) {
            const lifetime = lifecycle.capture();

            request =
                request ||
                scheduledPolicyAdministration
                    .dispatch({ type: "snapshotRequested", stream: "policies" })
                    .find((effect) => effect.type === "fetchSnapshot");
            if (!request) return;
            const res = await fetch("/api/update-policies");
            if (!lifecycle.isCurrent(lifetime)) return;
            if (!res.ok) {
                throw new Error(
                    await parseErrorResponse(
                        res,
                        "Failed to load scheduled policies.",
                    ),
                );
            }
            const data = await res.json().catch(() => ({}));
            if (!lifecycle.isCurrent(lifetime)) return;
            const followUp = scheduledPolicyAdministration.dispatch({
                type: "snapshotReceived",
                stream: "policies",
                requestId: request.requestId,
                receivedAt: new Date().toISOString(),
                data,
            });
            if (data.timezone) {
                applyScheduledTimezone(data);
            }
            renderScheduledPolicies();
            renderMaintenanceCalendarFilter();
            await runScheduledEffects(followUp);
            if (!lifecycle.isCurrent(lifetime)) return;
        }

        async function fetchScheduledSettings(request) {
            const lifetime = lifecycle.capture();

            request =
                request ||
                scheduledPolicyAdministration
                    .dispatch({ type: "snapshotRequested", stream: "settings" })
                    .find((effect) => effect.type === "fetchSnapshot");
            if (!request) return;
            const res = await fetch("/api/update-policies/settings");
            if (!lifecycle.isCurrent(lifetime)) return;
            if (!res.ok) {
                throw new Error(
                    await parseErrorResponse(
                        res,
                        "Failed to load scheduled update settings.",
                    ),
                );
            }
            const data = await res.json().catch(() => ({}));
            if (!lifecycle.isCurrent(lifetime)) return;
            const followUp = scheduledPolicyAdministration.dispatch({
                type: "snapshotReceived",
                stream: "settings",
                requestId: request.requestId,
                receivedAt: new Date().toISOString(),
                data,
            });
            applyScheduledTimezone(
                data.timezone ? data : scheduledPolicyView().timezone || "UTC",
            );
            setBlackoutEditorRows("global", data.global_blackouts || []);
            setBlackoutJsonStatus("global", "");
            await runScheduledEffects(followUp);
            if (!lifecycle.isCurrent(lifetime)) return;
        }

        async function fetchScheduledRuns(request) {
            const lifetime = lifecycle.capture();

            request =
                request ||
                scheduledPolicyAdministration
                    .dispatch({ type: "snapshotRequested", stream: "runs" })
                    .find((effect) => effect.type === "fetchSnapshot");
            if (!request) return;
            const query =
                request.query || scheduledPolicyView().runHistory?.query || {};
            const params = new URLSearchParams({
                page: String(query.page || 1),
                page_size: String(query.pageSize || 25),
            });
            if (query.policy) params.set("policy", query.policy);
            if (query.server) params.set("server", query.server);
            if (query.outcome) params.set("outcome", query.outcome);
            if (query.from) params.set("from", query.from);
            if (query.to) params.set("to", query.to);
            const res = await fetch(
                `/api/update-policies/runs?${params.toString()}`,
            );
            if (!lifecycle.isCurrent(lifetime)) return;
            if (!res.ok) {
                throw new Error(
                    await parseErrorResponse(
                        res,
                        "Failed to load scheduled runs.",
                    ),
                );
            }
            const data = await res.json().catch(() => ({}));
            if (!lifecycle.isCurrent(lifetime)) return;
            const followUp = scheduledPolicyAdministration.dispatch({
                type: "snapshotReceived",
                stream: "runs",
                requestId: request.requestId,
                receivedAt: new Date().toISOString(),
                data,
            });
            const accepted = followUp.some(
                (effect) => effect.type === "render" && effect.area === "runs",
            );
            if (!accepted) return;
            if (data.timezone) {
                applyScheduledTimezone(data);
            }
            renderScheduledRuns();
            await runScheduledEffects(followUp);
            if (!lifecycle.isCurrent(lifetime)) return;
        }

        async function fetchMaintenanceCalendar(request) {
            const lifetime = lifecycle.capture();

            const policyID = String(
                request?.policyID ||
                    scheduledPolicyView().selectedCalendarPolicyID ||
                    document.getElementById("maintenance-calendar-policy")
                        ?.value ||
                    "",
            ).trim();
            request =
                request ||
                scheduledPolicyAdministration
                    .dispatch({
                        type: "snapshotRequested",
                        stream: "calendar",
                        payload: { policyID },
                    })
                    .find((effect) => effect.type === "fetchSnapshot");
            if (!request) return;
            const params = new URLSearchParams({ days: "14" });
            if (policyID) params.set("policy_id", policyID);
            const res = await fetch(
                `/api/update-policies/calendar?${params.toString()}`,
            );
            if (!lifecycle.isCurrent(lifetime)) return;
            if (!res.ok) {
                throw new Error(
                    await parseErrorResponse(
                        res,
                        "Failed to load maintenance window calendar.",
                    ),
                );
            }
            const data = await res.json().catch(() => ({}));
            if (!lifecycle.isCurrent(lifetime)) return;
            const followUp = scheduledPolicyAdministration.dispatch({
                type: "snapshotReceived",
                stream: "calendar",
                requestId: request.requestId,
                receivedAt: new Date().toISOString(),
                data,
            });
            if (data.timezone) {
                applyScheduledTimezone(data);
            }
            renderMaintenanceCalendar(scheduledPolicyView().calendar);
            await runScheduledEffects(followUp);
            if (!lifecycle.isCurrent(lifetime)) return;
        }

        async function reloadMaintenanceCalendar() {
            const lifetime = lifecycle.capture();

            const status = document.getElementById(
                "maintenance-calendar-status",
            );
            try {
                if (status) status.textContent = "Loading calendar...";
                await fetchMaintenanceCalendar();
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                const requestId =
                    scheduledPolicyView().snapshots.calendar.inFlight;
                if (requestId)
                    scheduledPolicyAdministration.dispatch({
                        type: "snapshotFailed",
                        stream: "calendar",
                        requestId,
                        error:
                            err.message ||
                            "Failed to load maintenance window calendar.",
                    });
                console.error(
                    "Failed to load maintenance window calendar:",
                    err,
                );
                if (status)
                    status.textContent =
                        err.message ||
                        "Failed to load maintenance window calendar.";
            }
        }

        async function runScheduledEffects(effects) {
            const lifetime = lifecycle.capture();

            let firstError = null;
            for (const effect of effects) {
                if (effect.type !== "fetchSnapshot") continue;
                try {
                    if (effect.stream === "policies")
                        await fetchScheduledPolicies(effect);
                    if (!lifecycle.isCurrent(lifetime)) return;
                    if (effect.stream === "settings")
                        await fetchScheduledSettings(effect);
                    if (!lifecycle.isCurrent(lifetime)) return;
                    if (effect.stream === "runs")
                        await fetchScheduledRuns(effect);
                    if (!lifecycle.isCurrent(lifetime)) return;
                    if (effect.stream === "calendar")
                        await fetchMaintenanceCalendar(effect);
                    if (!lifecycle.isCurrent(lifetime)) return;
                } catch (err) {
                    if (
                        !lifecycle.isCurrent(lifetime) ||
                        err?.name === "AbortError"
                    )
                        return;
                    const failureEffects =
                        scheduledPolicyAdministration.dispatch({
                            type: "snapshotFailed",
                            stream: effect.stream,
                            requestId: effect.requestId,
                            error:
                                err.message ||
                                "Failed to refresh scheduled policy data.",
                        });
                    if (!failureEffects.length) continue;
                    renderAdminWorkspace();
                    firstError = firstError || err;
                }
            }
            if (firstError) throw firstError;
        }

        async function refreshScheduledUpdateViews() {
            const lifetime = lifecycle.capture();

            try {
                await fetchAppTimezoneSettings(true);
                if (!lifecycle.isCurrent(lifetime)) return;
                await runScheduledEffects(
                    ["policies", "settings"].flatMap((stream) =>
                        scheduledPolicyAdministration.dispatch({
                            type: "snapshotRequested",
                            stream,
                        }),
                    ),
                );
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                console.error("Failed to refresh scheduled update views:", err);
                setPolicyFeedback(
                    "",
                    err.message || "Failed to load scheduled update views.",
                );
            }
        }

        function collectPolicyPayload(options = {}) {
            const silent = !!options.silent;
            if (!silent) {
                clearPolicyFieldErrors();
                setPolicyFeedback("", "");
            }
            scheduledPolicyAdministration.dispatch({
                type: "editorChanged",
                patch: {
                    id: document.getElementById("policy-id").value,
                    name: document.getElementById("policy-name").value,
                    enabled: document.getElementById("policy-enabled").checked,
                    target_tag:
                        document.getElementById("policy-target-tag").value,
                    include_tags: document.getElementById("policy-include-tags")
                        .value,
                    exclude_tags: document.getElementById("policy-exclude-tags")
                        .value,
                    target_servers: document.getElementById(
                        "policy-target-servers",
                    ).value,
                    cadence_kind: document.getElementById("policy-cadence-kind")
                        .value,
                    date_local:
                        document.getElementById("policy-date-local").value,
                    month_day:
                        document.getElementById("policy-month-day").value,
                    month_last_day: document.getElementById(
                        "policy-month-last-day",
                    ).checked,
                    canary_mode:
                        document.getElementById("policy-canary-mode").value,
                    canary_servers: document.getElementById(
                        "policy-canary-servers",
                    ).value,
                    execution_mode: document.getElementById(
                        "policy-execution-mode",
                    ).value,
                    package_scope: document.getElementById(
                        "policy-package-scope",
                    ).value,
                    upgrade_mode: document.getElementById("policy-upgrade-mode")
                        .value,
                    time_local:
                        document.getElementById("policy-time-local").value,
                    approval_timeout_minutes: document.getElementById(
                        "policy-approval-timeout",
                    ).value,
                    rollout_mode: document.getElementById("policy-rollout-mode")
                        .value,
                    canary_count: document.getElementById("policy-canary-count")
                        .value,
                    wave_size:
                        document.getElementById("policy-wave-size").value,
                    wave_delay_minutes:
                        document.getElementById("policy-wave-delay").value,
                },
            });
            const result = scheduledPolicyAdministration.validatePolicyDraft();
            const errors = result.errors || {};
            let firstInvalidId = "";
            if (errors.name) {
                if (!silent) setPolicyFieldInvalid("policy-name", true);
                firstInvalidId = firstInvalidId || "policy-name";
            }
            if (errors.target_tag) {
                if (!silent) setPolicyFieldInvalid("policy-target-tag", true);
                firstInvalidId = firstInvalidId || "policy-target-tag";
            }
            for (const [field, id] of Object.entries({
                date_local: "policy-date-local",
                month_day: "policy-month-day",
                canary_servers: "policy-canary-servers",
                canary_count: "policy-canary-count",
                wave_size: "policy-wave-size",
                wave_delay_minutes: "policy-wave-delay",
            })) {
                if (errors[field]) {
                    if (!silent) setPolicyFieldInvalid(id, true);
                    firstInvalidId = firstInvalidId || id;
                }
            }
            if (firstInvalidId) {
                if (!silent) document.getElementById(firstInvalidId)?.focus();
            }
            if (!result.ok)
                throw new Error(
                    result.message ||
                        errors.blackouts ||
                        "Complete the scheduled policy fields.",
                );
            return result.payload;
        }

        async function saveScheduledPolicy(event) {
            const lifetime = lifecycle.capture();

            event.preventDefault();
            let plan;
            try {
                collectPolicyPayload();
                const command = scheduledPolicyAdministration.dispatch({
                    type: "commandRequested",
                    command: "savePolicy",
                });
                const execution = command.find(
                    (effect) => effect.type === "executeCommand",
                );
                if (!execution)
                    throw new Error(
                        command.find(
                            (effect) => effect.type === "commandRejected",
                        )?.message || "Scheduled policy action is unavailable.",
                    );
                plan = execution.plan;
                const url =
                    plan.command === "updatePolicy"
                        ? `/api/update-policies/${encodeURIComponent(plan.policyID)}`
                        : "/api/update-policies";
                const method = plan.command === "updatePolicy" ? "PUT" : "POST";
                const saveBtn = document.getElementById("policy-save-btn");
                if (saveBtn) saveBtn.disabled = true;
                const res = await fetch(url, {
                    method,
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify(plan.payload),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    throw new Error(
                        await parseErrorResponse(
                            res,
                            "Failed to save scheduled policy.",
                        ),
                    );
                }
                lifecycle.discardResponse(res);
                const successMessage =
                    plan.command === "updatePolicy"
                        ? "Policy updated."
                        : "Policy created.";
                resetPolicyForm();
                await runScheduledEffects(
                    scheduledPolicyAdministration.dispatch({
                        type: "commandCompleted",
                        plan,
                        message: successMessage,
                    }),
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                setPolicyFeedback(successMessage, "");
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                if (plan)
                    scheduledPolicyAdministration.dispatch({
                        type: "commandFailed",
                        plan,
                        message:
                            err.message || "Failed to save scheduled policy.",
                    });
                setPolicyFeedback(
                    "",
                    err.message || "Failed to save scheduled policy.",
                );
            } finally {
                if (!lifecycle.isCurrent(lifetime)) return;
                renderPolicyDraftState();
            }
        }

        async function saveScheduledSettings() {
            const lifetime = lifecycle.capture();

            let plan;
            try {
                setScheduledSettingsFeedback("", "");
                const command = scheduledPolicyAdministration.dispatch({
                    type: "commandRequested",
                    command: "saveGlobalSettings",
                });
                const execution = command.find(
                    (effect) => effect.type === "executeCommand",
                );
                if (!execution)
                    throw new Error(
                        command.find(
                            (effect) => effect.type === "commandRejected",
                        )?.message || "Global settings are unavailable.",
                    );
                plan = execution.plan;
                const button = document.getElementById(
                    "scheduled-settings-save",
                );
                if (button) button.disabled = true;
                const res = await fetch("/api/update-policies/settings", {
                    method: "PUT",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify(plan.payload),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    throw new Error(
                        await parseErrorResponse(
                            res,
                            "Failed to save global no-run windows.",
                        ),
                    );
                }
                const data = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                applyScheduledTimezone(
                    data.timezone
                        ? data
                        : scheduledPolicyView().timezone || "UTC",
                );
                if (
                    !String(
                        data?.resolved_timezone ?? data?.resolvedTimezone ?? "",
                    ).trim()
                ) {
                    try {
                        await fetchScheduledRuns();
                        if (!lifecycle.isCurrent(lifetime)) return;
                    } catch (refreshErr) {
                        if (
                            !lifecycle.isCurrent(lifetime) ||
                            refreshErr?.name === "AbortError"
                        )
                            return;
                        const requestId =
                            scheduledPolicyView().snapshots.runs.inFlight;
                        if (requestId)
                            scheduledPolicyAdministration.dispatch({
                                type: "snapshotFailed",
                                stream: "runs",
                                requestId,
                                error:
                                    refreshErr.message ||
                                    "Failed to load scheduled runs.",
                            });
                        console.error(
                            "Failed to refresh scheduled runs after settings save:",
                            refreshErr,
                        );
                    }
                }
                const successMessage = "Global no-run windows saved.";
                await runScheduledEffects(
                    scheduledPolicyAdministration.dispatch({
                        type: "commandCompleted",
                        plan,
                        message: successMessage,
                    }),
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                setScheduledSettingsFeedback(successMessage, "");
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                if (plan)
                    scheduledPolicyAdministration.dispatch({
                        type: "commandFailed",
                        plan,
                        message:
                            err.message ||
                            "Failed to save global no-run windows.",
                    });
                setScheduledSettingsFeedback(
                    "",
                    err.message || "Failed to save global no-run windows.",
                );
            } finally {
                if (!lifecycle.isCurrent(lifetime)) return;
                renderGlobalSettingsDraftState();
            }
        }

        async function deleteScheduledPolicy(id) {
            const lifetime = lifecycle.capture();

            const plan = scheduledPolicyAdministration.planCommand(
                "deletePolicy",
                id,
            );
            const policy = plan.policy;
            const required = policy?.name || String(id);
            if (!plan.enabled) {
                setPolicyFeedback(
                    "",
                    plan.reason || "Scheduled policy action is unavailable.",
                );
                return;
            }
            if (
                !(await window.confirmTypedAction(
                    "Type the policy name to confirm permanent deletion.",
                    required,
                    {
                        title: "Delete scheduled update policy",
                        operation:
                            "Permanently delete one recurring update policy",
                        resources: `${required} (policy ${id})`,
                        consequences:
                            "Future runs from this policy stop; existing job and audit records remain.",
                        reversibility:
                            "Not reversible. Recreating the policy requires entering its configuration again.",
                        authentication:
                            "Typed policy-name confirmation is required; the current Admin session authorizes the request.",
                        confirmLabel: "Delete Policy",
                    },
                ))
            ) {
                return;
            }
            if (!lifecycle.isCurrent(lifetime)) return;
            let activePlan;
            try {
                const command = scheduledPolicyAdministration.dispatch({
                    type: "commandRequested",
                    command: "deletePolicy",
                    policyID: id,
                });
                const execution = command.find(
                    (effect) => effect.type === "executeCommand",
                );
                if (!execution)
                    throw new Error(
                        command.find(
                            (effect) => effect.type === "commandRejected",
                        )?.message || "Scheduled policy action is unavailable.",
                    );
                activePlan = execution.plan;
                renderAdminDestructiveControls();
                const res = await fetch(
                    `/api/update-policies/${encodeURIComponent(activePlan.policyID)}`,
                    { method: "DELETE" },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    throw new Error(
                        await parseErrorResponse(
                            res,
                            "Failed to delete scheduled policy.",
                        ),
                    );
                }
                lifecycle.discardResponse(res);
                if (document.getElementById("policy-id").value === String(id)) {
                    resetPolicyForm();
                }
                setPolicyFeedback("Policy deleted.", "");
                await runScheduledEffects(
                    scheduledPolicyAdministration.dispatch({
                        type: "commandCompleted",
                        plan: activePlan,
                        message: "Policy deleted.",
                    }),
                );
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                if (activePlan)
                    scheduledPolicyAdministration.dispatch({
                        type: "commandFailed",
                        plan: activePlan,
                        message:
                            err?.message ||
                            "Failed to delete scheduled policy.",
                    });
                setPolicyFeedback(
                    "",
                    err?.message || "Failed to delete scheduled policy.",
                );
            } finally {
                if (!lifecycle.isCurrent(lifetime)) return;
                renderAdminDestructiveControls();
            }
        }

        function handleScheduledPolicyTableClick(event) {
            const button = event.target.closest("button[data-action]");
            if (!button) return;
            const id = String(button.dataset.id || "").trim();
            const policy = scheduledPolicyView().policies.find(
                (item) => String(item.id) === id,
            );
            if (!policy) return;
            if (button.dataset.action === "edit-policy") {
                requestPolicyReplacement({ type: "load", policy }, button);
                return;
            }
            if (button.dataset.action === "delete-policy") {
                deleteScheduledPolicy(id);
            }
        }

        function updateBlackoutRowField(kind, index, field, value) {
            scheduledPolicyAdministration.dispatch({
                type: "blackoutRowChanged",
                kind,
                index,
                field,
                value,
            });
            syncBlackoutTextarea(kind);
            updateBlackoutRowSummary(kind, index);
            if (kind === "policy") updatePolicySummary();
            else renderGlobalSettingsDraftState();
        }

        function handleBlackoutEditorClick(event) {
            const button = event.target.closest("[data-blackout-action]");
            if (!button) return;
            const kind = button.dataset.blackoutKind;
            const action = button.dataset.blackoutAction;
            const index = Number(button.dataset.index || -1);
            setBlackoutJsonStatus(kind, "");
            if (action === "remove-window") {
                if (index >= 0) {
                    scheduledPolicyAdministration.dispatch({
                        type: "blackoutRowRemoved",
                        kind,
                        index,
                    });
                    renderBlackoutEditor(kind);
                }
                return;
            }
            if (action === "toggle-day" && index >= 0) {
                scheduledPolicyAdministration.dispatch({
                    type: "blackoutWeekdayToggled",
                    kind,
                    index,
                    day: button.dataset.day,
                });
                const isActive = scheduledPolicyRows(kind)[
                    index
                ]?.weekdays.includes(normalizeWeekdayToken(button.dataset.day));
                button.classList.toggle("active", isActive);
                button.setAttribute(
                    "aria-pressed",
                    isActive ? "true" : "false",
                );
                syncBlackoutTextarea(kind);
                updateBlackoutRowSummary(kind, index);
                if (kind === "policy") updatePolicySummary();
                else renderGlobalSettingsDraftState();
            }
        }

        function handleBlackoutEditorInput(event) {
            const input = event.target.closest("[data-blackout-field]");
            if (!input) return;
            const kind = input.dataset.blackoutKind;
            const field = input.dataset.blackoutField;
            const index = Number(input.dataset.index || -1);
            if (index < 0 || !field) return;
            setBlackoutJsonStatus(kind, "");
            updateBlackoutRowField(kind, index, field, input.value);
        }

        function applyBlackoutJson(kind, label) {
            const textarea = document.getElementById(
                kind === "global"
                    ? "scheduled-global-blackouts-json"
                    : "policy-blackouts-json",
            );
            if (!textarea) return;
            const effect = scheduledPolicyAdministration.dispatch({
                type: "blackoutJSONApplied",
                kind,
                label,
                raw: textarea.value,
            })[0];
            if (effect?.type === "blackoutJSONAccepted") {
                renderBlackoutEditor(kind);
                setBlackoutJsonStatus(kind, effect.message, false);
                if (kind === "global") {
                    setScheduledSettingsFeedback("", "");
                    renderGlobalSettingsDraftState();
                }
            } else {
                setBlackoutJsonStatus(
                    kind,
                    effect?.message ||
                        `Failed to apply ${label.toLowerCase()}.`,
                    true,
                );
            }
        }

        function bindPolicyFormInteractions() {
            const summaryFields = [
                "policy-name",
                "policy-target-tag",
                "policy-include-tags",
                "policy-exclude-tags",
                "policy-target-servers",
                "policy-time-local",
                "policy-execution-mode",
                "policy-package-scope",
                "policy-upgrade-mode",
                "policy-cadence-kind",
                "policy-enabled",
                "policy-approval-timeout",
                "policy-rollout-mode",
                "policy-canary-count",
                "policy-date-local",
                "policy-month-day",
                "policy-month-last-day",
                "policy-canary-mode",
                "policy-canary-servers",
                "policy-wave-size",
                "policy-wave-delay",
            ];
            summaryFields.forEach((fieldId) => {
                lifecycle.listen(
                    document.getElementById(fieldId),
                    "input",
                    () => {
                        try {
                            collectPolicyPayload({ silent: true });
                        } catch (_) {
                            if (!lifecycle.active || _?.name === "AbortError")
                                return;
                        }
                        if (fieldId === "policy-name")
                            setPolicyFieldInvalid("policy-name", false);
                        if (fieldId === "policy-target-tag")
                            setPolicyFieldInvalid("policy-target-tag", false);
                        refreshPolicyFormVisibility();
                        updatePolicySummary();
                        schedulePolicyPreview();
                    },
                );
                lifecycle.listen(
                    document.getElementById(fieldId),
                    "change",
                    () => {
                        try {
                            collectPolicyPayload({ silent: true });
                        } catch (_) {
                            if (!lifecycle.active || _?.name === "AbortError")
                                return;
                        }
                        if (fieldId === "policy-name")
                            setPolicyFieldInvalid("policy-name", false);
                        if (fieldId === "policy-target-tag")
                            setPolicyFieldInvalid("policy-target-tag", false);
                        refreshPolicyFormVisibility();
                        updatePolicySummary();
                        schedulePolicyPreview();
                    },
                );
            });

            lifecycle.listen(
                document.getElementById("policy-weekdays-picker"),
                "click",
                (event) => {
                    const button = event.target.closest("[data-weekday]");
                    if (!button) return;
                    togglePolicyWeekday(button.dataset.weekday);
                },
            );

            lifecycle.listen(
                document.getElementById("policy-weekdays-clear"),
                "click",
                () => {
                    setPolicyWeekdays([]);
                },
            );
        }
        return Object.freeze({
            init: lifecycle.init,
            destroy: lifecycle.destroy,
            addBlackoutRow: (...args) =>
                lifecycle.active ? addBlackoutRow(...args) : undefined,
            updatePolicySummary: (...args) =>
                lifecycle.active ? updatePolicySummary(...args) : undefined,
            renderGlobalSettingsDraftState: (...args) =>
                lifecycle.active
                    ? renderGlobalSettingsDraftState(...args)
                    : undefined,
            schedulePolicyPreview: (...args) =>
                lifecycle.active ? schedulePolicyPreview(...args) : undefined,
            closeJobDetailModal: (...args) =>
                lifecycle.active ? closeJobDetailModal(...args) : undefined,
            copyJobDetailText: (...args) =>
                lifecycle.active ? copyJobDetailText(...args) : undefined,
            resetPolicyForm: (...args) =>
                lifecycle.active ? resetPolicyForm(...args) : undefined,
            requestPolicyReplacement: (...args) =>
                lifecycle.active
                    ? requestPolicyReplacement(...args)
                    : undefined,
            discardPolicyDraft: (...args) =>
                lifecycle.active ? discardPolicyDraft(...args) : undefined,
            discardGlobalSettingsDraft: (...args) =>
                lifecycle.active
                    ? discardGlobalSettingsDraft(...args)
                    : undefined,
            renderScheduledPolicies: (...args) =>
                lifecycle.active ? renderScheduledPolicies(...args) : undefined,
            renderScheduledRuns: (...args) =>
                lifecycle.active ? renderScheduledRuns(...args) : undefined,
            handleScheduledRunsTableClick: (...args) =>
                lifecycle.active
                    ? handleScheduledRunsTableClick(...args)
                    : undefined,
            fetchScheduledRuns: (...args) =>
                lifecycle.active ? fetchScheduledRuns(...args) : undefined,
            reloadMaintenanceCalendar: (...args) =>
                lifecycle.active
                    ? reloadMaintenanceCalendar(...args)
                    : undefined,
            runScheduledEffects: (...args) =>
                lifecycle.active ? runScheduledEffects(...args) : undefined,
            refreshScheduledUpdateViews: (...args) =>
                lifecycle.active
                    ? refreshScheduledUpdateViews(...args)
                    : undefined,
            saveScheduledPolicy: (...args) =>
                lifecycle.active ? saveScheduledPolicy(...args) : undefined,
            saveScheduledSettings: (...args) =>
                lifecycle.active ? saveScheduledSettings(...args) : undefined,
            handleScheduledPolicyTableClick: (...args) =>
                lifecycle.active
                    ? handleScheduledPolicyTableClick(...args)
                    : undefined,
            handleBlackoutEditorClick: (...args) =>
                lifecycle.active
                    ? handleBlackoutEditorClick(...args)
                    : undefined,
            handleBlackoutEditorInput: (...args) =>
                lifecycle.active
                    ? handleBlackoutEditorInput(...args)
                    : undefined,
            applyBlackoutJson: (...args) =>
                lifecycle.active ? applyBlackoutJson(...args) : undefined,
            bindPolicyFormInteractions: (...args) =>
                lifecycle.active
                    ? bindPolicyFormInteractions(...args)
                    : undefined,
        });
    }
    root.AdminPoliciesAdapter = Object.freeze({ create });
})(globalThis);
