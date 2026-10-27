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
        const scheduledPolicyInteraction =
            window.ScheduledPolicyAdministrationInteraction.createStore();
        const adminPageInteraction = window.AdminPageInteraction.createStore({
            scheduled: scheduledPolicyInteraction,
        });
        const {
            closeSessionIPRevealModal,
            handleSessionIPRevealModalKeydown,
            hideTemporarySessionIPReveal,
            openSessionIPRevealModal,
            submitSessionIPReveal,
        } = window.AdminSessionIPReveal;
        const scheduledPolicyAdministration = Object.freeze({
            dispatch(event) {
                return adminPageInteraction.dispatch({
                    type: "scheduledEvent",
                    event,
                });
            },
            getView() {
                return adminPageInteraction.getView().scheduled;
            },
            planCommand(command, payload) {
                return adminPageInteraction.planCommand(
                    `scheduled:${command}`,
                    payload,
                );
            },
            validatePolicyDraft() {
                return scheduledPolicyInteraction.validatePolicyDraft();
            },
        });

        let appTimezonePicker = null;
        let appTimezonePreviewTimer = 0;
        let adminSectionObserver = null;
        let adminSectionNavigationLockUntil = 0;

        const adminSectionPreferenceKey =
            "simplelinuxupdater.admin.collapsed-sections.v1";

        const adminNotifications = window.AdminNotificationsAdapter.create({
            adminPageView: (...args) => adminPageView(...args),
            formatAdminRefreshTime: (...args) =>
                formatAdminRefreshTime(...args),
            renderAdminWorkspace: (...args) => renderAdminWorkspace(...args),
            adminPageInteraction: adminPageInteraction,
            beginAdminSnapshot: (...args) => beginAdminSnapshot(...args),
            parseErrorResponse: (...args) => parseErrorResponse(...args),
            beginAdminCommand: (...args) => beginAdminCommand(...args),
            finishAdminCommand: (...args) => finishAdminCommand(...args),
            encodeURIComponent: (...args) => encodeURIComponent(...args),
        });
        const {
            renderNotificationDraftState,
            syncNotificationDraftFromDOM,
            setDiscordWebhookIntent,
            setTelegramCredentialsIntent,
            setNotificationWebhookIntent,
            discardNotificationDraft,
            fetchNotificationSettings,
            fetchNotificationDeliveryDiagnostics,
            saveNotificationSettings,
            sendNotificationTest,
        } = adminNotifications;
        adminNotifications.init();
        lifecycle.own(adminNotifications.destroy);
        const adminAccount = window.AdminAccountAdapter.create({
            adminPageView: (...args) => adminPageView(...args),
            adminPageInteraction: adminPageInteraction,
            hideTemporarySessionIPReveal: (...args) =>
                hideTemporarySessionIPReveal(...args),
            renderAdminWorkspace: (...args) => renderAdminWorkspace(...args),
            beginAdminSnapshot: (...args) => beginAdminSnapshot(...args),
            escapeHtml: (...args) => escapeHtml(...args),
            parseErrorResponse: (...args) => parseErrorResponse(...args),
            beginAdminCommand: (...args) => beginAdminCommand(...args),
            finishAdminCommand: (...args) => finishAdminCommand(...args),
            encodeURIComponent: (...args) => encodeURIComponent(...args),
            openSessionIPRevealModal: (...args) =>
                openSessionIPRevealModal(...args),
        });
        const {
            setAuthPasswordFeedback,
            renderAuthPasswordControls,
            togglePasswordVisibility,
            updatePasswordCapsLockWarning,
            formatSessionTime,
            renderAuthSessions,
            fetchAuthSessionStatus,
            fetchRecentAdminActivity,
            clearOtherAuthSessions,
            handleAuthSessionListClick,
            changeAdminPassword,
            clearAuthSessions,
            fetchMetricsTokenStatus,
            rotateMetricsToken,
            disableMetricsToken,
            copyMetricsToken,
        } = adminAccount;
        adminAccount.init();
        lifecycle.own(adminAccount.destroy);
        const adminBackup = window.AdminBackupAdapter.create({
            decodeURIComponent: (...args) => decodeURIComponent(...args),
            formatSessionTime: (...args) => formatSessionTime(...args),
            adminPageView: (...args) => adminPageView(...args),
            escapeHtml: (...args) => escapeHtml(...args),
            renderAdminDestructiveControls: (...args) =>
                renderAdminDestructiveControls(...args),
            adminPageInteraction: adminPageInteraction,
            beginAdminCommand: (...args) => beginAdminCommand(...args),
            parseErrorResponse: (...args) => parseErrorResponse(...args),
            finishAdminCommand: (...args) => finishAdminCommand(...args),
            beginAdminSnapshot: (...args) => beginAdminSnapshot(...args),
            renderAdminWorkspace: (...args) => renderAdminWorkspace(...args),
            updateFileLabel: (...args) => updateFileLabel(...args),
        });
        const {
            renderBackupRestoreReview,
            submitBackupAutomation,
            fetchBackupStatus,
            exportBackup,
            restoreBackup,
            verifyBackup,
        } = adminBackup;
        adminBackup.init();
        lifecycle.own(adminBackup.destroy);
        const adminPolicies = window.AdminPoliciesAdapter.create({
            scheduledPolicyRows: (...args) => scheduledPolicyRows(...args),
            scheduledPolicyAdministration: scheduledPolicyAdministration,
            escapeHtml: (...args) => escapeHtml(...args),
            scheduledPolicyView: (...args) => scheduledPolicyView(...args),
            parseErrorResponse: (...args) => parseErrorResponse(...args),
            encodeURIComponent: (...args) => encodeURIComponent(...args),
            renderAdminWorkspace: (...args) => renderAdminWorkspace(...args),
            applyScheduledTimezone: (...args) =>
                applyScheduledTimezone(...args),
            fetchAppTimezoneSettings: (...args) =>
                fetchAppTimezoneSettings(...args),
            renderAdminDestructiveControls: (...args) =>
                renderAdminDestructiveControls(...args),
        });
        const {
            addBlackoutRow,
            updatePolicySummary,
            renderGlobalSettingsDraftState,
            schedulePolicyPreview,
            closeJobDetailModal,
            copyJobDetailText,
            resetPolicyForm,
            requestPolicyReplacement,
            discardPolicyDraft,
            discardGlobalSettingsDraft,
            renderScheduledPolicies,
            renderScheduledRuns,
            handleScheduledRunsTableClick,
            fetchScheduledRuns,
            reloadMaintenanceCalendar,
            runScheduledEffects,
            refreshScheduledUpdateViews,
            saveScheduledPolicy,
            saveScheduledSettings,
            handleScheduledPolicyTableClick,
            handleBlackoutEditorClick,
            handleBlackoutEditorInput,
            applyBlackoutJson,
            bindPolicyFormInteractions,
        } = adminPolicies;
        adminPolicies.init();
        lifecycle.own(adminPolicies.destroy);

        function adminPageView() {
            return adminPageInteraction.getView();
        }

        function formatAdminRefreshTime(value) {
            const timestamp = String(value || "").trim();
            if (!timestamp) return "";
            const parsed = new Date(timestamp);
            if (Number.isNaN(parsed.getTime())) return "";
            return new Intl.DateTimeFormat(undefined, {
                dateStyle: "medium",
                timeStyle: "short",
            }).format(parsed);
        }

        function renderAdminDestructiveControls() {
            const view = adminPageView();
            const locked = Boolean(view.commands?.destructiveInFlight);
            const generateMetricsToken = document.getElementById(
                "metrics-token-generate",
            );
            if (generateMetricsToken) {
                generateMetricsToken.disabled = locked || view.metrics.enabled;
                generateMetricsToken.title = locked
                    ? "Another destructive Admin action is already in progress."
                    : view.metrics.enabled
                      ? "A Metrics API token already exists. Rotate it from the danger zone."
                      : "";
            }
            document
                .querySelectorAll("[data-admin-danger-command]")
                .forEach((button) => {
                    const command = button.dataset.adminDangerCommand;
                    let disabled = locked;
                    let reason = locked
                        ? "Another destructive Admin action is already in progress."
                        : "";
                    if (!locked && command === "clearOtherSessions") {
                        const plan = adminPageInteraction.planCommand(command);
                        disabled = !plan.enabled;
                        reason = plan.reason || "";
                    }
                    if (!locked && command === "scheduled:deletePolicy") {
                        const plan = adminPageInteraction.planCommand(
                            command,
                            button.dataset.id || "",
                        );
                        disabled = !plan.enabled;
                        reason = plan.reason || "";
                    }
                    if (!locked && command === "restoreBackup") {
                        const plan = adminPageInteraction.planCommand(command);
                        disabled = !plan.enabled;
                        reason = plan.reason || "";
                    }
                    if (
                        !locked &&
                        ["rotateMetricsToken", "disableMetricsToken"].includes(
                            command,
                        ) &&
                        !view.metrics.enabled
                    ) {
                        disabled = true;
                        reason = "No Metrics API token is configured.";
                    }
                    button.disabled = disabled;
                    button.title = disabled ? reason : "";
                });
        }

        function renderAdminWorkspace() {
            const workspace = adminPageView().workspace;
            if (!workspace) return;
            const lifecycleLabels = {
                loading: "Loading",
                current: "Current",
                stale: "Stale",
                failed: "Failed",
                unavailable: "Unavailable",
            };
            workspace.sections.forEach((section) => {
                const root = document.querySelector(
                    `[data-admin-section="${section.id}"]`,
                );
                const link = document.querySelector(
                    `[data-admin-section-link="${section.id}"]`,
                );
                const navSummary = document.querySelector(
                    `[data-admin-section-nav-summary="${section.id}"]`,
                );
                const summary = document.querySelector(
                    `[data-admin-section-summary="${section.id}"]`,
                );
                const lifecycle = document.querySelector(
                    `[data-admin-section-lifecycle="${section.id}"]`,
                );
                const toggle = document.querySelector(
                    `[data-admin-section-toggle="${section.id}"]`,
                );
                const content = document.querySelector(
                    `[data-admin-section-content="${section.id}"]`,
                );

                root?.classList.toggle("is-collapsed", section.collapsed);
                root?.setAttribute(
                    "aria-busy",
                    String(section.lifecycle?.status === "loading"),
                );
                if (link) {
                    if (workspace.activeSection === section.id)
                        link.setAttribute("aria-current", "location");
                    else link.removeAttribute("aria-current");
                }
                if (navSummary) navSummary.textContent = section.summary;
                if (summary) {
                    summary.textContent = section.summary;
                    summary.hidden = !section.collapsed;
                }
                if (lifecycle) {
                    const status = section.lifecycle?.status || "unavailable";
                    const statusNode = lifecycle.querySelector(
                        "[data-admin-section-status]",
                    );
                    const refreshedNode = lifecycle.querySelector(
                        "[data-admin-section-refreshed]",
                    );
                    const retry = lifecycle.querySelector(
                        "[data-admin-section-retry]",
                    );
                    const refreshed = formatAdminRefreshTime(
                        section.lifecycle?.lastSuccessfulRefresh,
                    );
                    lifecycle.dataset.status = status;
                    lifecycle.title = section.lifecycle?.error || "";
                    if (statusNode)
                        statusNode.textContent =
                            lifecycleLabels[status] ||
                            lifecycleLabels.unavailable;
                    if (refreshedNode) {
                        refreshedNode.textContent = refreshed
                            ? `Updated ${refreshed}`
                            : "";
                        refreshedNode.hidden = !refreshed;
                    }
                    if (retry) {
                        retry.hidden = !["stale", "failed"].includes(status);
                        retry.disabled = status === "loading";
                    }
                }
                if (toggle) {
                    toggle.setAttribute(
                        "aria-expanded",
                        String(!section.collapsed),
                    );
                    toggle.setAttribute(
                        "aria-label",
                        `${section.collapsed ? "Expand" : "Collapse"} ${section.label}`,
                    );
                    const label = toggle.querySelector(
                        "[data-admin-section-toggle-label]",
                    );
                    if (label)
                        label.textContent = section.collapsed
                            ? "Expand"
                            : "Collapse";
                }
                if (content) content.hidden = section.collapsed;
            });
            renderAdminDestructiveControls();
        }

        function runAdminWorkspaceEffects(effects) {
            (effects || []).forEach((effect) => {
                if (effect.type === "render" && effect.area === "workspace") {
                    renderAdminWorkspace();
                }
                if (effect.type === "persistSectionPreferences") {
                    try {
                        localStorage.setItem(
                            adminSectionPreferenceKey,
                            JSON.stringify(effect.collapsedSections || []),
                        );
                    } catch (_) {
                        if (!lifecycle.active || _?.name === "AbortError")
                            return;
                    }
                }
                if (effect.type === "focusSection") {
                    const section = document.querySelector(
                        `[data-admin-section="${effect.sectionID}"]`,
                    );
                    const heading = document.getElementById(
                        `admin-section-${effect.sectionID}-heading`,
                    );
                    section?.scrollIntoView({
                        behavior: "auto",
                        block: "start",
                    });
                    heading?.focus({ preventScroll: true });
                }
                if (effect.type === "loadSectionData") {
                    void loadAdminSectionData(effect.sectionID, effect.reason);
                }
            });
        }

        async function loadAdminSectionData(
            sectionID,
            reason = "first-activation",
        ) {
            const lifetime = lifecycle.capture();

            try {
                if (sectionID === "application-checks")
                    await applicationPostcheckAdapter.load();
                if (sectionID === "app-time")
                    await fetchAppTimezoneSettings(true);
                if (sectionID === "notifications")
                    await Promise.all([
                        fetchNotificationSettings(),
                        fetchNotificationDeliveryDiagnostics(),
                    ]);
                if (sectionID === "account-security")
                    await fetchAuthSessionStatus();
                if (sectionID === "recent-activity")
                    await fetchRecentAdminActivity();
                if (sectionID === "backup") await fetchBackupStatus();
                if (sectionID === "metrics") await fetchMetricsTokenStatus();
                if (sectionID === "scheduled-runs") {
                    const effects = scheduledPolicyAdministration.dispatch({
                        type: "snapshotRequested",
                        stream: "runs",
                    });
                    renderAdminWorkspace();
                    await runScheduledEffects(effects);
                    if (!lifecycle.isCurrent(lifetime)) return;
                }
                if (sectionID === "scheduled-policies") {
                    const streams =
                        reason === "retry"
                            ? ["policies", "settings", "calendar"]
                            : ["calendar"];
                    const effects = streams.flatMap((stream) =>
                        scheduledPolicyAdministration.dispatch({
                            type: "snapshotRequested",
                            stream,
                        }),
                    );
                    renderAdminWorkspace();
                    await runScheduledEffects(effects);
                    if (!lifecycle.isCurrent(lifetime)) return;
                }
            } catch (error) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    error?.name === "AbortError"
                )
                    return;
                console.error(
                    `Failed to load Admin section "${sectionID}":`,
                    error,
                );
            } finally {
                if (!lifecycle.isCurrent(lifetime)) return;
                renderAdminWorkspace();
            }
        }

        function adminSectionIDFromHash() {
            const prefix = "#admin-section-";
            return window.location.hash.startsWith(prefix)
                ? window.location.hash.slice(prefix.length)
                : "";
        }

        function navigateAdminSection(sectionID, updateHistory = false) {
            const effects = adminPageInteraction.dispatch({
                type: "sectionNavigationRequested",
                sectionID,
            });
            if (!effects.length) return;
            adminSectionNavigationLockUntil = Date.now() + 750;
            if (updateHistory) {
                history.pushState(null, "", `#admin-section-${sectionID}`);
            }
            runAdminWorkspaceEffects(effects);
        }

        function initializeAdminWorkspace() {
            let collapsedSections = [];
            try {
                const stored = JSON.parse(
                    localStorage.getItem(adminSectionPreferenceKey) || "[]",
                );
                if (Array.isArray(stored)) collapsedSections = stored;
            } catch (_) {
                if (!lifecycle.active || _?.name === "AbortError") return;
            }
            runAdminWorkspaceEffects(
                adminPageInteraction.dispatch({
                    type: "sectionPreferencesRestored",
                    collapsedSections,
                }),
            );

            lifecycle.listen(
                document.getElementById("admin-section-nav"),
                "click",
                (event) => {
                    const link = event.target.closest(
                        "[data-admin-section-link]",
                    );
                    if (!link) return;
                    event.preventDefault();
                    navigateAdminSection(link.dataset.adminSectionLink, true);
                },
            );
            document
                .querySelectorAll("[data-admin-section-toggle]")
                .forEach((toggle) => {
                    lifecycle.listen(toggle, "click", () => {
                        runAdminWorkspaceEffects(
                            adminPageInteraction.dispatch({
                                type: "sectionCollapseToggled",
                                sectionID: toggle.dataset.adminSectionToggle,
                            }),
                        );
                    });
                });
            document
                .querySelectorAll("[data-admin-section-retry]")
                .forEach((retry) => {
                    lifecycle.listen(retry, "click", () => {
                        runAdminWorkspaceEffects(
                            adminPageInteraction.dispatch({
                                type: "sectionRetryRequested",
                                sectionID: retry.dataset.adminSectionRetry,
                            }),
                        );
                    });
                });
            lifecycle.listen(window, "hashchange", () => {
                const sectionID = adminSectionIDFromHash();
                if (sectionID) navigateAdminSection(sectionID);
            });

            if ("IntersectionObserver" in window) {
                adminSectionObserver = new IntersectionObserver(
                    (entries) => {
                        if (Date.now() < adminSectionNavigationLockUntil)
                            return;
                        const visible = entries
                            .filter((entry) => entry.isIntersecting)
                            .sort(
                                (left, right) =>
                                    right.intersectionRatio -
                                    left.intersectionRatio,
                            )[0];
                        const sectionID =
                            visible?.target?.dataset?.adminSection;
                        if (!sectionID) return;
                        runAdminWorkspaceEffects(
                            adminPageInteraction.dispatch({
                                type: "sectionActivated",
                                sectionID,
                            }),
                        );
                    },
                    {
                        rootMargin: "-150px 0px -55% 0px",
                        threshold: [0.01, 0.25, 0.5],
                    },
                );
                document
                    .querySelectorAll("[data-admin-section]")
                    .forEach((section) =>
                        adminSectionObserver.observe(section),
                    );
            }

            const deepLinkedSection = adminSectionIDFromHash();
            if (deepLinkedSection) {
                lifecycle.requestAnimationFrame(() =>
                    navigateAdminSection(deepLinkedSection),
                );
            } else {
                renderAdminWorkspace();
            }
        }

        function beginAdminCommand(command, payload = {}) {
            if (!lifecycle.active) return null;
            const effects = adminPageInteraction.dispatch({
                type: "commandRequested",
                command,
                payload,
            });
            renderAdminDestructiveControls();
            return (
                effects.find((item) => item.type === "executeCommand")?.plan ||
                null
            );
        }

        function finishAdminCommand(plan, data, message, failed = false) {
            if (!plan) return [];
            const effects = adminPageInteraction.dispatch({
                type: failed ? "commandFailed" : "commandCompleted",
                plan,
                data,
                message,
            });
            renderAdminDestructiveControls();
            return effects;
        }

        function beginAdminSnapshot(stream) {
            const effects = adminPageInteraction.dispatch({
                type: "snapshotRequested",
                stream,
            });
            renderAdminWorkspace();
            return (
                effects.find((item) => item.type === "fetchSnapshot")
                    ?.requestID || null
            );
        }

        function scheduledPolicyView() {
            return scheduledPolicyAdministration.getView();
        }

        function scheduledPolicyRows(kind) {
            const editor = scheduledPolicyView().editor;
            return kind === "global"
                ? editor.globalBlackouts
                : editor.policyBlackouts;
        }

        function browserSupportedTimezones() {
            try {
                if (
                    typeof Intl === "undefined" ||
                    typeof Intl.supportedValuesOf !== "function"
                ) {
                    return [];
                }
                return Intl.supportedValuesOf("timeZone");
            } catch (_) {
                if (!lifecycle.active || _?.name === "AbortError") return;
                return [];
            }
        }

        function ensureTimezoneSelectHasValue(value) {
            const rawTimezone = String(value || "").trim();
            const timezone = rawTimezone === "Local" ? "" : rawTimezone;
            if (appTimezonePicker) appTimezonePicker.setValue(timezone);
        }

        function renderTimezoneSaveState() {
            const view = adminPageView();
            const button = document.getElementById("app-timezone-save");
            const discard = document.getElementById("app-timezone-discard");
            const state = document.getElementById("app-timezone-draft-state");
            if (!button) return;
            const inFlight = view.commands.inFlight.includes("saveTimezone");
            button.disabled = !view.timezone.dirty || inFlight;
            button.title = view.timezone.dirty
                ? "Save the selected app timezone."
                : "Choose a different timezone to save.";
            if (discard) discard.disabled = !view.timezone.dirty || inFlight;
            if (state) {
                state.textContent = view.timezone.dirty
                    ? "Unsaved timezone change"
                    : "No unsaved changes";
                state.classList.toggle("is-dirty", view.timezone.dirty);
            }
        }

        function renderCurrentAppTime(resolvedTimezone = "") {
            const preview = document.getElementById("app-timezone-preview");
            if (
                !preview ||
                !window.AdminTimezonePicker?.formatCurrentTimePreview
            )
                return;
            const acceptedTimezone =
                String(
                    resolvedTimezone || preview.dataset.timezone || "",
                ).trim() || "UTC";
            preview.dataset.timezone = acceptedTimezone;
            preview.textContent =
                window.AdminTimezonePicker.formatCurrentTimePreview(
                    acceptedTimezone,
                );
            const source = document.getElementById("app-timezone-source");
            if (source) {
                source.textContent = String(
                    adminPageView().timezone.configured || "",
                ).trim()
                    ? "Explicit timezone"
                    : "System default";
            }
        }

        function populateTimezonePicker() {
            const trigger = document.getElementById("app-timezone-input");
            if (!trigger || !window.AdminTimezonePicker) return;
            const rawCurrentValue =
                trigger.value || adminPageView().timezone.draft || "";
            const currentValue =
                rawCurrentValue === "Local" ? "" : rawCurrentValue;
            const systemTimezone = adminPageView().timezone.resolved || "";
            const browserTimezone = (() => {
                try {
                    return (
                        Intl.DateTimeFormat().resolvedOptions().timeZone || ""
                    );
                } catch (_) {
                    if (!lifecycle.active || _?.name === "AbortError") return;
                    return "";
                }
            })();
            const combined = [
                "",
                "UTC",
                ...browserSupportedTimezones(),
                ...ianaTimezoneOptions,
                ...fixedOffsetTimezoneOptions,
            ];
            const suggested = [
                currentValue,
                browserTimezone,
                "UTC",
                "America/Toronto",
                "America/New_York",
                "America/Chicago",
                "America/Denver",
                "America/Los_Angeles",
                "Europe/London",
                "Europe/Paris",
            ].filter(Boolean);
            const options = window.AdminTimezonePicker.buildOptions(combined, {
                suggested,
                systemTimezone,
            });
            appTimezonePicker = window.AdminTimezonePicker.createPicker({
                trigger,
                popover: document.getElementById("app-timezone-popover"),
                search: document.getElementById("app-timezone-search"),
                listbox: document.getElementById("app-timezone-options"),
                empty: document.getElementById("app-timezone-empty"),
                options,
            });
            if (!appTimezonePicker) return;
            appTimezonePicker.setValue(currentValue);
            const note = document.getElementById("app-timezone-picker-note");
            if (note) {
                note.textContent = `Search ${options.length} choices by city, region, timezone name, or UTC offset. Fixed offsets do not follow daylight-saving changes.`;
            }
            renderTimezoneSaveState();
            if (!appTimezonePreviewTimer) {
                appTimezonePreviewTimer = lifecycle.setInterval(
                    () => renderCurrentAppTime(),
                    30_000,
                );
            }
        }

        function renderScheduledTimezone(payload) {
            const timezoneState = window.setAppTimezoneCache
                ? window.setAppTimezoneCache(payload)
                : {
                      timezone:
                          String(payload || "").trim() ||
                          scheduledPolicyView().timezone ||
                          "UTC",
                  };
            const timezone = timezoneState.timezone || "UTC";
            const configuredTimezone = String(
                timezoneState.editable_timezone ??
                    timezoneState.editableTimezone ??
                    "",
            ).trim();
            if (appTimezonePicker) {
                appTimezonePicker.setSystemTimezone(
                    configuredTimezone ? "" : timezoneState.resolved_timezone,
                );
            }
            renderCurrentAppTime(timezoneState.resolved_timezone || timezone);
            scheduledPolicyAdministration.dispatch({
                type: "timezoneReceived",
                timezone,
            });
            const timezoneSelection = adminPageView().timezone.draft;
            const timezoneLabel = document.getElementById("scheduled-timezone");
            if (timezoneLabel) {
                timezoneLabel.textContent = timezone;
            }
            const timezoneInput = document.getElementById("app-timezone-input");
            const timezonePickerRoot = document.getElementById(
                "app-timezone-picker",
            );
            if (
                timezoneInput &&
                !timezonePickerRoot?.contains(document.activeElement)
            ) {
                ensureTimezoneSelectHasValue(timezoneSelection);
            }
            renderTimezoneSaveState();
            updatePolicySummary();
            renderScheduledPolicies();
            renderScheduledRuns(scheduledPolicyView().runs);
            renderAdminWorkspace();
            schedulePolicyPreview();
        }

        function applyScheduledTimezone(payload, requestID = null) {
            const effects = adminPageInteraction.dispatch({
                type: "timezoneSnapshotReceived",
                requestID,
                receivedAt: new Date().toISOString(),
                data: payload,
            });
            if (effects.length === 0) return false;
            renderScheduledTimezone(payload);
            return true;
        }

        function setAppTimezoneFeedback(successMessage, errorMessage) {
            const success = document.getElementById("app-timezone-status");
            const error = document.getElementById("app-timezone-error");
            if (success) success.textContent = successMessage || "";
            if (error) error.textContent = errorMessage || "";
        }

        function discardTimezoneDraft() {
            adminPageInteraction.dispatch({ type: "timezoneDiscardRequested" });
            ensureTimezoneSelectHasValue(adminPageView().timezone.draft);
            renderTimezoneSaveState();
            setAppTimezoneFeedback("", "");
        }

        async function fetchAppTimezoneSettings(force = false) {
            const lifetime = lifecycle.capture();

            const requestID = beginAdminSnapshot("timezone");
            const timezonePayload = window.ensureAppTimezoneLoaded
                ? await window.ensureAppTimezoneLoaded(force)
                : scheduledPolicyView().timezone;
            if (!lifecycle.isCurrent(lifetime)) return;
            applyScheduledTimezone(timezonePayload, requestID);
        }

        async function saveAppTimezoneSettings() {
            const lifetime = lifecycle.capture();

            let plan;
            try {
                setAppTimezoneFeedback("", "");
                const input = document.getElementById("app-timezone-input");
                const button = document.getElementById("app-timezone-save");
                const timezone = appTimezonePicker
                    ? appTimezonePicker.getValue()
                    : input
                      ? input.value.trim()
                      : "";
                adminPageInteraction.dispatch({
                    type: "timezoneDraftChanged",
                    timezone,
                });
                plan = beginAdminCommand("saveTimezone");
                if (!plan) return;
                renderTimezoneSaveState();
                const res = await fetch("/api/app-settings/timezone", {
                    method: "PUT",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify(plan.payload),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    const message = await parseErrorResponse(
                        res,
                        "Failed to save app timezone.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    finishAdminCommand(plan, null, message, true);
                    setAppTimezoneFeedback("", message);
                    return;
                }
                const data = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                finishAdminCommand(plan, data, "App timezone saved.");
                renderScheduledTimezone(data);
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
                        console.error(
                            "Failed to refresh scheduled runs after timezone save:",
                            refreshErr,
                        );
                    }
                }
                setAppTimezoneFeedback("App timezone saved.", "");
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                finishAdminCommand(
                    plan,
                    null,
                    err.message || "Failed to save app timezone.",
                    true,
                );
                setAppTimezoneFeedback(
                    "",
                    err.message || "Failed to save app timezone.",
                );
            } finally {
                if (!lifecycle.isCurrent(lifetime)) return;
                renderTimezoneSaveState();
            }
        }

        lifecycle.listen(document, "change", (event) => {
            if (event.target && event.target.id === "backup-restore-file") {
                adminPageInteraction.dispatch({
                    type: "backupFileSelected",
                    file: event.target.files?.[0] || null,
                });
                updateFileLabel(event.target, "Choose backup file");
                renderBackupRestoreReview();
            }
        });

        lifecycle.listen(document.getElementById("logout-btn"), "click", () =>
            window.logout(),
        );
        lifecycle.listen(
            document.getElementById("metrics-token-generate"),
            "click",
            () => rotateMetricsToken(false),
        );
        lifecycle.listen(
            document.getElementById("metrics-token-rotate"),
            "click",
            () => rotateMetricsToken(true),
        );
        lifecycle.listen(
            document.getElementById("metrics-token-disable"),
            "click",
            disableMetricsToken,
        );
        lifecycle.listen(
            document.getElementById("metrics-token-copy"),
            "click",
            copyMetricsToken,
        );
        lifecycle.listen(
            document.getElementById("backup-automation-form"),
            "submit",
            (event) => {
                event.preventDefault();
                submitBackupAutomation(false);
            },
        );
        lifecycle.listen(
            document.getElementById("backup-automation-run"),
            "click",
            () => submitBackupAutomation(true),
        );
        lifecycle.listen(
            document.getElementById("backup-export-btn"),
            "click",
            exportBackup,
        );
        lifecycle.listen(
            document.getElementById("backup-verify-btn"),
            "click",
            verifyBackup,
        );
        lifecycle.listen(
            document.getElementById("backup-restore-btn"),
            "click",
            restoreBackup,
        );
        lifecycle.listen(
            document.getElementById("backup-restore-passphrase"),
            "input",
            (event) => {
                adminPageInteraction.dispatch({
                    type: "backupPassphraseChanged",
                    valid: event.target.value.length >= 12,
                });
                renderBackupRestoreReview();
            },
        );
        lifecycle.listen(
            document.getElementById("app-timezone-save"),
            "click",
            saveAppTimezoneSettings,
        );
        lifecycle.listen(
            document.getElementById("app-timezone-discard"),
            "click",
            discardTimezoneDraft,
        );
        lifecycle.listen(
            document.getElementById("app-timezone-input"),
            "input",
            (event) => {
                adminPageInteraction.dispatch({
                    type: "timezoneDraftChanged",
                    timezone: event.target.value,
                });
                setAppTimezoneFeedback("", "");
                renderTimezoneSaveState();
            },
        );
        lifecycle.listen(
            document.getElementById("notification-save"),
            "click",
            saveNotificationSettings,
        );
        lifecycle.listen(
            document.getElementById("notification-discard"),
            "click",
            discardNotificationDraft,
        );
        document
            .querySelectorAll("[data-notification-test]")
            .forEach((button) => {
                lifecycle.listen(button, "click", () =>
                    sendNotificationTest(
                        button.dataset.notificationTest || "webhook",
                    ),
                );
            });
        lifecycle.listen(
            document.getElementById("notification-diagnostics-retry"),
            "click",
            fetchNotificationDeliveryDiagnostics,
        );
        lifecycle.listen(
            document.getElementById("notification-webhook-url"),
            "input",
            syncNotificationDraftFromDOM,
        );
        lifecycle.listen(
            document.getElementById("notification-webhook-replace"),
            "click",
            () => setNotificationWebhookIntent("replace"),
        );
        lifecycle.listen(
            document.getElementById("notification-webhook-clear"),
            "click",
            () => setNotificationWebhookIntent("clear"),
        );
        lifecycle.listen(
            document.getElementById("notification-webhook-cancel"),
            "click",
            () => setNotificationWebhookIntent("preserve"),
        );
        lifecycle.listen(
            document.getElementById("notification-enabled"),
            "change",
            syncNotificationDraftFromDOM,
        );
        lifecycle.listen(
            document.getElementById("notification-reminders-enabled"),
            "change",
            syncNotificationDraftFromDOM,
        );
        lifecycle.listen(
            document.getElementById("notification-reminder-interval"),
            "input",
            syncNotificationDraftFromDOM,
        );
        lifecycle.listen(
            document.getElementById("notification-discord-url"),
            "input",
            syncNotificationDraftFromDOM,
        );
        lifecycle.listen(
            document.getElementById("notification-discord-replace"),
            "click",
            () => setDiscordWebhookIntent("replace"),
        );
        lifecycle.listen(
            document.getElementById("notification-discord-clear"),
            "click",
            () => setDiscordWebhookIntent("clear"),
        );
        lifecycle.listen(
            document.getElementById("notification-discord-cancel"),
            "click",
            () => setDiscordWebhookIntent("preserve"),
        );
        lifecycle.listen(
            document.getElementById("notification-discord-enabled"),
            "change",
            syncNotificationDraftFromDOM,
        );
        lifecycle.listen(
            document.getElementById("notification-telegram-token"),
            "input",
            syncNotificationDraftFromDOM,
        );
        lifecycle.listen(
            document.getElementById("notification-telegram-chat-id"),
            "input",
            syncNotificationDraftFromDOM,
        );
        lifecycle.listen(
            document.getElementById("notification-telegram-replace"),
            "click",
            () => setTelegramCredentialsIntent("replace"),
        );
        lifecycle.listen(
            document.getElementById("notification-telegram-clear"),
            "click",
            () => setTelegramCredentialsIntent("clear"),
        );
        lifecycle.listen(
            document.getElementById("notification-telegram-cancel"),
            "click",
            () => setTelegramCredentialsIntent("preserve"),
        );
        lifecycle.listen(
            document.getElementById("notification-telegram-enabled"),
            "change",
            syncNotificationDraftFromDOM,
        );
        document
            .querySelectorAll("[data-notification-event]")
            .forEach((input) =>
                lifecycle.listen(input, "change", syncNotificationDraftFromDOM),
            );
        lifecycle.listen(
            document.getElementById("auth-password-save"),
            "click",
            changeAdminPassword,
        );
        [
            "auth-current-password",
            "auth-new-password",
            "auth-confirm-password",
        ].forEach((id) => {
            const input = document.getElementById(id);
            lifecycle.listen(input, "input", () => {
                setAuthPasswordFeedback("", "");
                renderAuthPasswordControls();
            });
            lifecycle.listen(input, "keydown", updatePasswordCapsLockWarning);
            lifecycle.listen(input, "keyup", updatePasswordCapsLockWarning);
        });
        document
            .querySelectorAll("[data-password-toggle]")
            .forEach((button) =>
                lifecycle.listen(button, "click", togglePasswordVisibility),
            );
        lifecycle.listen(
            document.getElementById("auth-password-invalidate-others"),
            "change",
            renderAuthPasswordControls,
        );
        lifecycle.listen(
            document.getElementById("auth-sessions-clear"),
            "click",
            clearAuthSessions,
        );
        lifecycle.listen(
            document.getElementById("auth-sessions-clear-others"),
            "click",
            clearOtherAuthSessions,
        );
        lifecycle.listen(
            document.getElementById("auth-session-inventory"),
            "click",
            handleAuthSessionListClick,
        );
        lifecycle.listen(
            document.getElementById("auth-sessions-show-all"),
            "click",
            () => {
                const expanded = !adminPageView().account.otherSessionsExpanded;
                adminPageInteraction.dispatch({
                    type: "sessionListExpandedChanged",
                    expanded,
                });
                renderAuthSessions();
            },
        );
        lifecycle.listen(
            document.getElementById("session-ip-reveal-form"),
            "submit",
            submitSessionIPReveal,
        );
        lifecycle.listen(
            document.getElementById("session-ip-reveal-cancel"),
            "click",
            closeSessionIPRevealModal,
        );
        lifecycle.listen(
            document.getElementById("session-ip-reveal-modal"),
            "click",
            (event) => {
                if (event.target?.id === "session-ip-reveal-modal")
                    closeSessionIPRevealModal();
            },
        );
        lifecycle.listen(document, "visibilitychange", () => {
            if (!document.hidden) return;
            hideTemporarySessionIPReveal();
            const modal = document.getElementById("session-ip-reveal-modal");
            if (modal?.classList.contains("active"))
                closeSessionIPRevealModal({ restoreFocus: false });
        });
        lifecycle.listen(window, "pagehide", hideTemporarySessionIPReveal);
        lifecycle.listen(
            document.getElementById("update-policy-form"),
            "submit",
            saveScheduledPolicy,
        );
        lifecycle.listen(
            document.getElementById("policy-new-btn"),
            "click",
            (event) =>
                requestPolicyReplacement(
                    { type: "reset" },
                    event.currentTarget,
                ),
        );
        lifecycle.listen(
            document.getElementById("policy-discard-btn"),
            "click",
            discardPolicyDraft,
        );
        lifecycle.listen(
            document.getElementById("scheduled-settings-save"),
            "click",
            saveScheduledSettings,
        );
        lifecycle.listen(
            document.getElementById("scheduled-settings-discard"),
            "click",
            discardGlobalSettingsDraft,
        );
        lifecycle.listen(
            document.getElementById("maintenance-calendar-refresh"),
            "click",
            reloadMaintenanceCalendar,
        );
        lifecycle.listen(
            document.getElementById("maintenance-calendar-policy"),
            "change",
            (event) => {
                scheduledPolicyAdministration.dispatch({
                    type: "calendarPolicySelected",
                    policyID: event.target.value,
                });
                reloadMaintenanceCalendar();
            },
        );
        lifecycle.listen(
            document.querySelector("#scheduled-policy-table tbody"),
            "click",
            handleScheduledPolicyTableClick,
        );
        lifecycle.listen(
            document.querySelector("#scheduled-runs-table tbody"),
            "click",
            handleScheduledRunsTableClick,
        );
        lifecycle.listen(
            document.getElementById("scheduled-runs-filter"),
            "submit",
            async (event) => {
                const lifetime = lifecycle.capture();

                event.preventDefault();
                scheduledPolicyAdministration.dispatch({
                    type: "runQueryChanged",
                    patch: {
                        policy: document.getElementById(
                            "scheduled-runs-policy-filter",
                        ).value,
                        server: document.getElementById(
                            "scheduled-runs-server-filter",
                        ).value,
                        outcome: document.getElementById(
                            "scheduled-runs-outcome-filter",
                        ).value,
                        from: document.getElementById(
                            "scheduled-runs-from-filter",
                        ).value,
                        to: document.getElementById("scheduled-runs-to-filter")
                            .value,
                    },
                });
                renderScheduledRuns();
                renderAdminWorkspace();
                await runScheduledEffects(
                    scheduledPolicyAdministration.dispatch({
                        type: "runQueryApplied",
                    }),
                );
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("scheduled-runs-reset"),
            "click",
            async () => {
                const lifetime = lifecycle.capture();

                [
                    "scheduled-runs-policy-filter",
                    "scheduled-runs-server-filter",
                    "scheduled-runs-from-filter",
                    "scheduled-runs-to-filter",
                ].forEach((id) => {
                    document.getElementById(id).value = "";
                });
                document.getElementById("scheduled-runs-outcome-filter").value =
                    "";
                renderAdminWorkspace();
                await runScheduledEffects(
                    scheduledPolicyAdministration.dispatch({
                        type: "runQueryReset",
                    }),
                );
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("scheduled-runs-prev"),
            "click",
            async () => {
                const lifetime = lifecycle.capture();

                const page = scheduledPolicyView().runHistory?.page || 1;
                await runScheduledEffects(
                    scheduledPolicyAdministration.dispatch({
                        type: "runPageRequested",
                        page: page - 1,
                    }),
                );
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("scheduled-runs-next"),
            "click",
            async () => {
                const lifetime = lifecycle.capture();

                const page = scheduledPolicyView().runHistory?.page || 1;
                await runScheduledEffects(
                    scheduledPolicyAdministration.dispatch({
                        type: "runPageRequested",
                        page: page + 1,
                    }),
                );
                if (!lifecycle.isCurrent(lifetime)) return;
            },
        );
        lifecycle.listen(
            document.getElementById("policy-blackout-add"),
            "click",
            () => addBlackoutRow("policy"),
        );
        lifecycle.listen(
            document.getElementById("global-blackout-add"),
            "click",
            () => addBlackoutRow("global"),
        );
        lifecycle.listen(
            document.getElementById("policy-blackouts-json-apply"),
            "click",
            () => applyBlackoutJson("policy", "Policy no-run windows"),
        );
        lifecycle.listen(
            document.getElementById("scheduled-global-blackouts-json-apply"),
            "click",
            () => applyBlackoutJson("global", "Global no-run windows"),
        );
        lifecycle.listen(
            document.getElementById("policy-blackout-rows"),
            "click",
            handleBlackoutEditorClick,
        );
        lifecycle.listen(
            document.getElementById("global-blackout-rows"),
            "click",
            handleBlackoutEditorClick,
        );
        lifecycle.listen(
            document.getElementById("policy-blackout-rows"),
            "input",
            handleBlackoutEditorInput,
        );
        lifecycle.listen(
            document.getElementById("global-blackout-rows"),
            "input",
            handleBlackoutEditorInput,
        );
        lifecycle.listen(
            document.getElementById("job-detail-close"),
            "click",
            closeJobDetailModal,
        );
        lifecycle.listen(
            document.getElementById("job-detail-copy-summary"),
            "click",
            () => copyJobDetailText("summary"),
        );
        lifecycle.listen(
            document.getElementById("job-detail-copy-logs"),
            "click",
            () => copyJobDetailText("logs"),
        );
        lifecycle.listen(
            document.getElementById("job-detail-modal"),
            "click",
            (event) => {
                if (event.target && event.target.id === "job-detail-modal") {
                    closeJobDetailModal();
                }
            },
        );
        lifecycle.listen(document, "keydown", (event) => {
            handleSessionIPRevealModalKeydown(event);
            const modal = document.getElementById("job-detail-modal");
            if (
                event.key === "Escape" &&
                modal &&
                modal.classList.contains("active")
            ) {
                closeJobDetailModal();
            }
        });
        lifecycle.listen(window, "beforeunload", (event) => {
            if (!adminPageView().hasMeaningfulDirty) return;
            event.preventDefault();
            event.returnValue = "";
        });

        const applicationPostcheckAdapter = ApplicationPostcheckAdapter.mount({
            store: adminPageInteraction,
            onChange: renderAdminWorkspace,
        });

        bindPolicyFormInteractions();
        initializeAdminWorkspace();
        populateTimezonePicker();
        resetPolicyForm();
        fetchAuthSessionStatus();
        refreshScheduledUpdateViews();
        updateFileLabel(
            document.getElementById("backup-restore-file"),
            "Choose backup file",
        );
        renderBackupRestoreReview();
        renderNotificationDraftState();
        renderGlobalSettingsDraftState();

        lifecycle.own(() => {
            appTimezonePicker?.destroy?.();
            closeJobDetailModal();
            hideTemporarySessionIPReveal();
            closeSessionIPRevealModal({ restoreFocus: false });
            applicationPostcheckAdapter.destroy();
        });
        // Preserve refresh entry points used by existing browser consumers.
        window.fetchRecentAdminActivity = (...args) =>
            lifecycle.active
                ? fetchRecentAdminActivity(...args)
                : Promise.resolve();
        window.fetchNotificationSettings = (...args) =>
            lifecycle.active
                ? fetchNotificationSettings(...args)
                : Promise.resolve();
        window.fetchBackupStatus = (...args) =>
            lifecycle.active ? fetchBackupStatus(...args) : Promise.resolve();
        window.fetchAuthSessionStatus = (...args) =>
            lifecycle.active
                ? fetchAuthSessionStatus(...args)
                : Promise.resolve();
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
        lifecycle.listen(root, "pagehide", destroy, { once: true });
        current = Object.freeze({ destroy });
        return current;
    }
    root.AdminPage = Object.freeze({
        mount,
        destroy: () => current?.destroy(),
    });
    root.addEventListener("pageshow", (event) => {
        if (event.persisted) mount();
    });
    mount();
})(globalThis);
