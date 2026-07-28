const scheduledPolicyInteraction = window.ScheduledPolicyAdministrationInteraction.createStore();
const adminPageInteraction = window.AdminPageInteraction.createStore({ scheduled: scheduledPolicyInteraction });
const scheduledPolicyAdministration = Object.freeze({
    dispatch(event) {
        return adminPageInteraction.dispatch({ type: "scheduledEvent", event });
    },
    getView() {
        return adminPageInteraction.getView().scheduled;
    },
    planCommand(command, payload) {
        return adminPageInteraction.planCommand(`scheduled:${command}`, payload);
    },
    validatePolicyDraft() {
        return scheduledPolicyInteraction.validatePolicyDraft();
    }
});
let scheduledPolicyPreviewTimer = 0;
let appTimezonePicker = null;
let appTimezonePreviewTimer = 0;
let adminSectionObserver = null;
let adminSectionNavigationLockUntil = 0;
const sessionIPReveal = {
    sessionID: "",
    expiresAt: 0,
    intervalID: 0,
    timeoutID: 0,
    requestController: null,
    requestID: 0,
    trigger: null,
    background: []
};

const adminSectionPreferenceKey = "simplelinuxupdater.admin.collapsed-sections.v1";

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
        timeStyle: "short"
    }).format(parsed);
}

function renderAdminDestructiveControls() {
    const view = adminPageView();
    const locked = Boolean(view.commands?.destructiveInFlight);
    const generateMetricsToken = document.getElementById("metrics-token-generate");
    if (generateMetricsToken) {
        generateMetricsToken.disabled = locked || view.metrics.enabled;
        generateMetricsToken.title = locked
            ? "Another destructive Admin action is already in progress."
            : view.metrics.enabled ? "A Metrics API token already exists. Rotate it from the danger zone." : "";
    }
    document.querySelectorAll("[data-admin-danger-command]").forEach((button) => {
        const command = button.dataset.adminDangerCommand;
        let disabled = locked;
        let reason = locked ? "Another destructive Admin action is already in progress." : "";
        if (!locked && command === "clearOtherSessions") {
            const plan = adminPageInteraction.planCommand(command);
            disabled = !plan.enabled;
            reason = plan.reason || "";
        }
        if (!locked && command === "scheduled:deletePolicy") {
            const plan = adminPageInteraction.planCommand(command, button.dataset.id || "");
            disabled = !plan.enabled;
            reason = plan.reason || "";
        }
        if (!locked && command === "restoreBackup") {
            const plan = adminPageInteraction.planCommand(command);
            disabled = !plan.enabled;
            reason = plan.reason || "";
        }
        if (!locked && ["rotateMetricsToken", "disableMetricsToken"].includes(command) && !view.metrics.enabled) {
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
        unavailable: "Unavailable"
    };
    workspace.sections.forEach((section) => {
        const root = document.querySelector(`[data-admin-section="${section.id}"]`);
        const link = document.querySelector(`[data-admin-section-link="${section.id}"]`);
        const navSummary = document.querySelector(`[data-admin-section-nav-summary="${section.id}"]`);
        const summary = document.querySelector(`[data-admin-section-summary="${section.id}"]`);
        const lifecycle = document.querySelector(`[data-admin-section-lifecycle="${section.id}"]`);
        const toggle = document.querySelector(`[data-admin-section-toggle="${section.id}"]`);
        const content = document.querySelector(`[data-admin-section-content="${section.id}"]`);

        root?.classList.toggle("is-collapsed", section.collapsed);
        root?.setAttribute("aria-busy", String(section.lifecycle?.status === "loading"));
        if (link) {
            if (workspace.activeSection === section.id) link.setAttribute("aria-current", "location");
            else link.removeAttribute("aria-current");
        }
        if (navSummary) navSummary.textContent = section.summary;
        if (summary) {
            summary.textContent = section.summary;
            summary.hidden = !section.collapsed;
        }
        if (lifecycle) {
            const status = section.lifecycle?.status || "unavailable";
            const statusNode = lifecycle.querySelector("[data-admin-section-status]");
            const refreshedNode = lifecycle.querySelector("[data-admin-section-refreshed]");
            const retry = lifecycle.querySelector("[data-admin-section-retry]");
            const refreshed = formatAdminRefreshTime(section.lifecycle?.lastSuccessfulRefresh);
            lifecycle.dataset.status = status;
            lifecycle.title = section.lifecycle?.error || "";
            if (statusNode) statusNode.textContent = lifecycleLabels[status] || lifecycleLabels.unavailable;
            if (refreshedNode) {
                refreshedNode.textContent = refreshed ? `Updated ${refreshed}` : "";
                refreshedNode.hidden = !refreshed;
            }
            if (retry) {
                retry.hidden = !["stale", "failed"].includes(status);
                retry.disabled = status === "loading";
            }
        }
        if (toggle) {
            toggle.setAttribute("aria-expanded", String(!section.collapsed));
            toggle.setAttribute("aria-label", `${section.collapsed ? "Expand" : "Collapse"} ${section.label}`);
            const label = toggle.querySelector("[data-admin-section-toggle-label]");
            if (label) label.textContent = section.collapsed ? "Expand" : "Collapse";
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
                localStorage.setItem(adminSectionPreferenceKey, JSON.stringify(effect.collapsedSections || []));
            } catch (_) {}
        }
        if (effect.type === "focusSection") {
            const section = document.querySelector(`[data-admin-section="${effect.sectionID}"]`);
            const heading = document.getElementById(`admin-section-${effect.sectionID}-heading`);
            section?.scrollIntoView({ behavior: "auto", block: "start" });
            heading?.focus({ preventScroll: true });
        }
        if (effect.type === "loadSectionData") {
            void loadAdminSectionData(effect.sectionID, effect.reason);
        }
    });
}

async function loadAdminSectionData(sectionID, reason = "first-activation") {
    try {
        if (sectionID === "app-time") await fetchAppTimezoneSettings(true);
        if (sectionID === "notifications") await fetchNotificationSettings();
        if (sectionID === "account-security") await fetchAuthSessionStatus();
        if (sectionID === "backup") await fetchBackupStatus();
        if (sectionID === "metrics") await fetchMetricsTokenStatus();
        if (sectionID === "scheduled-runs") {
            const effects = scheduledPolicyAdministration.dispatch({ type: "snapshotRequested", stream: "runs" });
            renderAdminWorkspace();
            await runScheduledEffects(effects);
        }
        if (sectionID === "scheduled-policies") {
            const streams = reason === "retry" ? ["policies", "settings", "calendar"] : ["calendar"];
            const effects = streams.flatMap(stream => scheduledPolicyAdministration.dispatch({ type: "snapshotRequested", stream }));
            renderAdminWorkspace();
            await runScheduledEffects(effects);
        }
    } catch (error) {
        console.error(`Failed to load Admin section "${sectionID}":`, error);
    } finally {
        renderAdminWorkspace();
    }
}

function adminSectionIDFromHash() {
    const prefix = "#admin-section-";
    return window.location.hash.startsWith(prefix) ? window.location.hash.slice(prefix.length) : "";
}

function navigateAdminSection(sectionID, updateHistory = false) {
    const effects = adminPageInteraction.dispatch({ type: "sectionNavigationRequested", sectionID });
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
        const stored = JSON.parse(localStorage.getItem(adminSectionPreferenceKey) || "[]");
        if (Array.isArray(stored)) collapsedSections = stored;
    } catch (_) {}
    runAdminWorkspaceEffects(adminPageInteraction.dispatch({ type: "sectionPreferencesRestored", collapsedSections }));

    document.getElementById("admin-section-nav")?.addEventListener("click", (event) => {
        const link = event.target.closest("[data-admin-section-link]");
        if (!link) return;
        event.preventDefault();
        navigateAdminSection(link.dataset.adminSectionLink, true);
    });
    document.querySelectorAll("[data-admin-section-toggle]").forEach((toggle) => {
        toggle.addEventListener("click", () => {
            runAdminWorkspaceEffects(adminPageInteraction.dispatch({
                type: "sectionCollapseToggled",
                sectionID: toggle.dataset.adminSectionToggle
            }));
        });
    });
    document.querySelectorAll("[data-admin-section-retry]").forEach((retry) => {
        retry.addEventListener("click", () => {
            runAdminWorkspaceEffects(adminPageInteraction.dispatch({
                type: "sectionRetryRequested",
                sectionID: retry.dataset.adminSectionRetry
            }));
        });
    });
    window.addEventListener("hashchange", () => {
        const sectionID = adminSectionIDFromHash();
        if (sectionID) navigateAdminSection(sectionID);
    });

    if ("IntersectionObserver" in window) {
        adminSectionObserver = new IntersectionObserver((entries) => {
            if (Date.now() < adminSectionNavigationLockUntil) return;
            const visible = entries
                .filter(entry => entry.isIntersecting)
                .sort((left, right) => right.intersectionRatio - left.intersectionRatio)[0];
            const sectionID = visible?.target?.dataset?.adminSection;
            if (!sectionID) return;
            runAdminWorkspaceEffects(adminPageInteraction.dispatch({ type: "sectionActivated", sectionID }));
        }, { rootMargin: "-150px 0px -55% 0px", threshold: [0.01, 0.25, 0.5] });
        document.querySelectorAll("[data-admin-section]").forEach(section => adminSectionObserver.observe(section));
    }

    const deepLinkedSection = adminSectionIDFromHash();
    if (deepLinkedSection) {
        window.requestAnimationFrame(() => navigateAdminSection(deepLinkedSection));
    } else {
        renderAdminWorkspace();
    }
}

function beginAdminCommand(command, payload = {}) {
    const effects = adminPageInteraction.dispatch({ type: "commandRequested", command, payload });
    renderAdminDestructiveControls();
    return effects.find((item) => item.type === "executeCommand")?.plan || null;
}

function finishAdminCommand(plan, data, message, failed = false) {
    if (!plan) return [];
    const effects = adminPageInteraction.dispatch({ type: failed ? "commandFailed" : "commandCompleted", plan, data, message });
    renderAdminDestructiveControls();
    return effects;
}

function beginAdminSnapshot(stream) {
    const effects = adminPageInteraction.dispatch({ type: "snapshotRequested", stream });
    renderAdminWorkspace();
    return effects.find((item) => item.type === "fetchSnapshot")?.requestID || null;
}

function scheduledPolicyView() {
    return scheduledPolicyAdministration.getView();
}

function scheduledPolicyRows(kind) {
    const editor = scheduledPolicyView().editor;
    return kind === "global" ? editor.globalBlackouts : editor.policyBlackouts;
}

const weekdayOptions = [
    { value: "mon", label: "Mon", fullLabel: "Monday" },
    { value: "tue", label: "Tue", fullLabel: "Tuesday" },
    { value: "wed", label: "Wed", fullLabel: "Wednesday" },
    { value: "thu", label: "Thu", fullLabel: "Thursday" },
    { value: "fri", label: "Fri", fullLabel: "Friday" },
    { value: "sat", label: "Sat", fullLabel: "Saturday" },
    { value: "sun", label: "Sun", fullLabel: "Sunday" }
];
function browserSupportedTimezones() {
    try {
        if (typeof Intl === "undefined" || typeof Intl.supportedValuesOf !== "function") {
            return [];
        }
        return Intl.supportedValuesOf("timeZone");
    } catch (_) {
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
        state.textContent = view.timezone.dirty ? "Unsaved timezone change" : "No unsaved changes";
        state.classList.toggle("is-dirty", view.timezone.dirty);
    }
}

function renderCurrentAppTime(resolvedTimezone = "") {
    const preview = document.getElementById("app-timezone-preview");
    if (!preview || !window.AdminTimezonePicker?.formatCurrentTimePreview) return;
    const acceptedTimezone = String(resolvedTimezone || preview.dataset.timezone || "").trim() || "UTC";
    preview.dataset.timezone = acceptedTimezone;
    preview.textContent = window.AdminTimezonePicker.formatCurrentTimePreview(acceptedTimezone);
}

function populateTimezonePicker() {
    const trigger = document.getElementById("app-timezone-input");
    if (!trigger || !window.AdminTimezonePicker) return;
    const rawCurrentValue = trigger.value || adminPageView().timezone.draft || "";
    const currentValue = rawCurrentValue === "Local" ? "" : rawCurrentValue;
    const systemTimezone = adminPageView().timezone.resolved || "";
    const browserTimezone = (() => {
        try {
            return Intl.DateTimeFormat().resolvedOptions().timeZone || "";
        } catch (_) {
            return "";
        }
    })();
    const combined = [
        "",
        "UTC",
        ...browserSupportedTimezones(),
        ...ianaTimezoneOptions,
        ...fixedOffsetTimezoneOptions
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
        "Europe/Paris"
    ].filter(Boolean);
    const options = window.AdminTimezonePicker.buildOptions(combined, { suggested, systemTimezone });
    appTimezonePicker = window.AdminTimezonePicker.createPicker({
        trigger,
        popover: document.getElementById("app-timezone-popover"),
        search: document.getElementById("app-timezone-search"),
        listbox: document.getElementById("app-timezone-options"),
        empty: document.getElementById("app-timezone-empty"),
        options
    });
    if (!appTimezonePicker) return;
    appTimezonePicker.setValue(currentValue);
    const note = document.getElementById("app-timezone-picker-note");
    if (note) {
        note.textContent = `Search ${options.length} choices by city, region, timezone name, or UTC offset. Fixed offsets do not follow daylight-saving changes.`;
    }
    renderTimezoneSaveState();
    if (!appTimezonePreviewTimer) {
        appTimezonePreviewTimer = window.setInterval(() => renderCurrentAppTime(), 30_000);
    }
}

function renderScheduledTimezone(payload) {
    const timezoneState = window.setAppTimezoneCache
        ? window.setAppTimezoneCache(payload)
        : { timezone: String(payload || "").trim() || scheduledPolicyView().timezone || "UTC" };
    const timezone = timezoneState.timezone || "UTC";
    const configuredTimezone = String(timezoneState.editable_timezone ?? timezoneState.editableTimezone ?? "").trim();
    if (appTimezonePicker) {
        appTimezonePicker.setSystemTimezone(configuredTimezone ? "" : timezoneState.resolved_timezone);
    }
    renderCurrentAppTime(timezoneState.resolved_timezone || timezone);
    scheduledPolicyAdministration.dispatch({ type: "timezoneReceived", timezone });
    const timezoneSelection = adminPageView().timezone.draft;
    const timezoneLabel = document.getElementById("scheduled-timezone");
    if (timezoneLabel) {
        timezoneLabel.textContent = timezone;
    }
    const timezoneInput = document.getElementById("app-timezone-input");
    const timezonePickerRoot = document.getElementById("app-timezone-picker");
    if (timezoneInput && !timezonePickerRoot?.contains(document.activeElement)) {
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
    const effects = adminPageInteraction.dispatch({ type: "timezoneSnapshotReceived", requestID, receivedAt: new Date().toISOString(), data: payload });
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

function setNotificationFeedback(successMessage, errorMessage) {
    const success = document.getElementById("notification-status");
    const error = document.getElementById("notification-error");
    if (success) success.textContent = successMessage || "";
    if (error) error.textContent = errorMessage || "";
}

function selectedNotificationEvents() {
    return Array.from(document.querySelectorAll("[data-notification-event]"))
        .filter((input) => input.checked)
        .map((input) => input.dataset.notificationEvent)
        .filter(Boolean);
}

function renderNotificationLastDelivery(status) {
    const node = document.getElementById("notification-last-delivery");
    if (!node) return;
    if (!status || !status.delivered_at) {
        node.textContent = "Last delivery: none.";
        return;
    }
    const outcome = status.success ? "success" : "failed";
    const target = status.target_name ? ` for ${status.target_name}` : "";
    const code = status.status_code ? ` HTTP ${status.status_code}.` : "";
    const err = status.error ? ` ${status.error}` : "";
    node.textContent = `Last delivery: ${outcome} ${status.event_type || status.action || "notification"}${target} at ${status.delivered_at} after ${Number(status.attempts || 0)} attempt(s).${code}${err}`;
}

function renderNotificationSettings() {
    const view = adminPageView().notifications;
    const enabled = document.getElementById("notification-enabled");
    const webhookURL = document.getElementById("notification-webhook-url");
    const eventTypes = view.eventTypes;
    if (enabled) enabled.checked = view.enabled;
    if (webhookURL && document.activeElement !== webhookURL) webhookURL.value = view.webhookURL;
    document.querySelectorAll("[data-notification-event]").forEach((input) => {
        input.checked = eventTypes.includes(input.dataset.notificationEvent);
    });
    renderNotificationLastDelivery(view.lastDelivery);
    renderNotificationDraftState();
    renderAdminWorkspace();
}

function renderNotificationDraftState() {
    const view = adminPageView();
    const notification = view.notifications;
    const inFlight = view.commands.inFlight.includes("saveNotifications");
    const save = document.getElementById("notification-save");
    const discard = document.getElementById("notification-discard");
    const state = document.getElementById("notification-draft-state");
    if (save) {
        save.disabled = !notification.dirty || !notification.valid || inFlight;
        save.title = notification.valid ? "" : notification.message;
    }
    if (discard) discard.disabled = !notification.dirty || inFlight;
    if (state) {
        state.textContent = notification.dirty
            ? (notification.valid ? "Unsaved notification changes" : notification.message)
            : "No unsaved changes";
        state.classList.toggle("is-dirty", notification.dirty);
    }
}

function syncNotificationDraftFromDOM() {
    adminPageInteraction.dispatch({ type: "notificationDraftChanged", patch: {
        enabled: Boolean(document.getElementById("notification-enabled")?.checked),
        webhookURL: document.getElementById("notification-webhook-url")?.value?.trim() || "",
        eventTypes: selectedNotificationEvents()
    } });
    setNotificationFeedback("", "");
    renderNotificationDraftState();
}

function discardNotificationDraft() {
    adminPageInteraction.dispatch({ type: "notificationDiscardRequested" });
    renderNotificationSettings();
    setNotificationFeedback("", "");
}

function discardTimezoneDraft() {
    adminPageInteraction.dispatch({ type: "timezoneDiscardRequested" });
    ensureTimezoneSelectHasValue(adminPageView().timezone.draft);
    renderTimezoneSaveState();
    setAppTimezoneFeedback("", "");
}

function applyNotificationSettings(payload, requestID = null) {
    const effects = adminPageInteraction.dispatch({ type: "notificationSnapshotReceived", requestID, receivedAt: new Date().toISOString(), data: payload });
    if (effects.length === 0) return false;
    renderNotificationSettings();
    return true;
}

async function fetchNotificationSettings() {
    const requestID = beginAdminSnapshot("notifications");
    try {
        const res = await fetch("/api/notifications/settings", { cache: "no-store" });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to load notification settings.");
            adminPageInteraction.dispatch({ type: "snapshotFailed", stream: "notifications", requestID, error: message });
            setNotificationFeedback("", message);
            renderAdminWorkspace();
            return;
        }
        const data = await res.json().catch(() => ({}));
        applyNotificationSettings(data, requestID);
    } catch (err) {
        console.error("Failed to load notification settings:", err);
        adminPageInteraction.dispatch({ type: "snapshotFailed", stream: "notifications", requestID, error: "Failed to load notification settings." });
        setNotificationFeedback("", "Failed to load notification settings.");
        renderAdminWorkspace();
    }
}

async function saveNotificationSettings() {
    const button = document.getElementById("notification-save");
    let plan;
    try {
        setNotificationFeedback("", "");
        if (button) button.disabled = true;
        const payload = {
            enabled: Boolean(document.getElementById("notification-enabled")?.checked),
            webhook_url: document.getElementById("notification-webhook-url")?.value?.trim() || "",
            event_types: selectedNotificationEvents()
        };
        adminPageInteraction.dispatch({ type: "notificationDraftChanged", patch: { enabled: payload.enabled, webhookURL: payload.webhook_url, eventTypes: payload.event_types } });
        plan = beginAdminCommand("saveNotifications");
        if (!plan) return;
        const res = await fetch("/api/notifications/settings", {
            method: "PUT",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(payload)
        });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to save notification settings.");
            finishAdminCommand(plan, null, message, true);
            setNotificationFeedback("", message);
            return;
        }
        const data = await res.json().catch(() => ({}));
        finishAdminCommand(plan, data, "Notification settings saved.");
        renderNotificationSettings();
        setNotificationFeedback("Notification settings saved.", "");
    } catch (err) {
        console.error("Failed to save notification settings:", err);
        finishAdminCommand(plan, null, "Failed to save notification settings.", true);
        setNotificationFeedback("", "Failed to save notification settings.");
    } finally {
        renderNotificationDraftState();
    }
}

async function sendNotificationTest() {
    const button = document.getElementById("notification-test");
    let plan;
    try {
        setNotificationFeedback("", "");
        if (button) button.disabled = true;
        plan = beginAdminCommand("testNotification");
        if (!plan) return;
        const res = await fetch("/api/notifications/test", { method: "POST" });
        const payload = await res.json().catch(() => ({}));
        if (!res.ok) {
            finishAdminCommand(plan, payload, "Notification test failed.", true);
            renderNotificationLastDelivery(payload.last_delivery);
            setNotificationFeedback("", await parseErrorResponse(res, "Notification test failed."));
            return;
        }
        finishAdminCommand(plan, payload, "Notification test delivered.");
        renderNotificationLastDelivery(payload.last_delivery);
        setNotificationFeedback("Notification test delivered.", "");
    } catch (err) {
        console.error("Failed to send notification test:", err);
        finishAdminCommand(plan, null, "Notification test failed.", true);
        setNotificationFeedback("", "Notification test failed.");
    } finally {
        if (button) button.disabled = false;
    }
}

async function fetchAppTimezoneSettings(force = false) {
    const requestID = beginAdminSnapshot("timezone");
    const timezonePayload = window.ensureAppTimezoneLoaded
        ? await window.ensureAppTimezoneLoaded(force)
        : scheduledPolicyView().timezone;
    applyScheduledTimezone(timezonePayload, requestID);
}

async function saveAppTimezoneSettings() {
    let plan;
    try {
        setAppTimezoneFeedback("", "");
        const input = document.getElementById("app-timezone-input");
        const button = document.getElementById("app-timezone-save");
        const timezone = appTimezonePicker ? appTimezonePicker.getValue() : (input ? input.value.trim() : "");
        adminPageInteraction.dispatch({ type: "timezoneDraftChanged", timezone });
        plan = beginAdminCommand("saveTimezone");
        if (!plan) return;
        renderTimezoneSaveState();
        const res = await fetch("/api/app-settings/timezone", {
            method: "PUT",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(plan.payload)
        });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to save app timezone.");
            finishAdminCommand(plan, null, message, true);
            setAppTimezoneFeedback("", message);
            return;
        }
        const data = await res.json().catch(() => ({}));
        finishAdminCommand(plan, data, "App timezone saved.");
        renderScheduledTimezone(data);
        if (!String(data?.resolved_timezone ?? data?.resolvedTimezone ?? "").trim()) {
            try {
                await fetchScheduledRuns();
            } catch (refreshErr) {
                console.error("Failed to refresh scheduled runs after timezone save:", refreshErr);
            }
        }
        setAppTimezoneFeedback("App timezone saved.", "");
    } catch (err) {
        finishAdminCommand(plan, null, err.message || "Failed to save app timezone.", true);
        setAppTimezoneFeedback("", err.message || "Failed to save app timezone.");
    } finally {
        renderTimezoneSaveState();
    }
}

function setAuthPasswordFeedback(statusMessage, errorMessage, tone = "success") {
    const success = document.getElementById("auth-password-status");
    const error = document.getElementById("auth-password-error");
    if (success) {
        success.textContent = statusMessage || "";
        success.classList.toggle("form-feedback-success", tone === "success");
        success.classList.toggle("form-feedback-warning", tone === "warning");
    }
    if (error) error.textContent = errorMessage || "";
}

function passwordPolicyChecks(value, policy = adminPageView().account.passwordPolicy) {
    const password = String(value || "");
    const length = Array.from(password).length;
    return {
        length: length >= policy.minLength && length <= policy.maxLength,
        letter: !policy.requiresLetter || /[A-Za-z]/.test(password),
        digit: !policy.requiresDigit || /[0-9]/.test(password)
    };
}

function passwordDraftFacts() {
    const currentPassword = document.getElementById("auth-current-password")?.value || "";
    const newPassword = document.getElementById("auth-new-password")?.value || "";
    const confirmPassword = document.getElementById("auth-confirm-password")?.value || "";
    const checks = passwordPolicyChecks(newPassword);
    return {
        hasCurrentPassword: currentPassword.length > 0,
        hasNewPassword: newPassword.length > 0,
        passwordsMatch: newPassword === confirmPassword,
        passwordValid: Object.values(checks).every(Boolean),
        invalidateOtherSessions: Boolean(document.getElementById("auth-password-invalidate-others")?.checked),
        checks
    };
}

function renderAuthPasswordControls() {
    const policy = adminPageView().account.passwordPolicy;
    const newInput = document.getElementById("auth-new-password");
    const confirmInput = document.getElementById("auth-confirm-password");
    const save = document.getElementById("auth-password-save");
    const facts = passwordDraftFacts();
    [newInput, confirmInput].forEach(input => {
        if (!input) return;
        input.minLength = policy.minLength;
        input.maxLength = policy.maxLength;
    });
    const length = document.querySelector('[data-password-requirement="length"]');
    const letter = document.querySelector('[data-password-requirement="letter"]');
    const digit = document.querySelector('[data-password-requirement="digit"]');
    if (length) length.textContent = `${policy.minLength}–${policy.maxLength} characters`;
    if (letter) {
        letter.textContent = policy.requiresLetter ? "At least one letter" : "Letters are optional";
        letter.hidden = !policy.requiresLetter;
    }
    if (digit) {
        digit.textContent = policy.requiresDigit ? "At least one digit" : "Digits are optional";
        digit.hidden = !policy.requiresDigit;
    }
    const hasDraft = Boolean(newInput?.value);
    Object.entries(facts.checks).forEach(([name, met]) => {
        document.querySelector(`[data-password-requirement="${name}"]`)?.classList.toggle("is-met", hasDraft && met);
    });
    const plan = adminPageInteraction.planCommand("changePassword", facts);
    if (save) {
        save.disabled = !plan.enabled;
        save.title = plan.enabled ? "" : plan.reason || "";
    }
}

function togglePasswordVisibility(event) {
    const button = event.currentTarget;
    const input = document.getElementById(button.dataset.passwordToggle || "");
    if (!input) return;
    const showing = input.type === "text";
    input.type = showing ? "password" : "text";
    button.textContent = showing ? "Show" : "Hide";
    button.setAttribute("aria-pressed", String(!showing));
    const fieldName = input.id === "auth-current-password"
        ? "current password"
        : input.id === "auth-new-password" ? "new password" : "password confirmation";
    button.setAttribute("aria-label", `${showing ? "Show" : "Hide"} ${fieldName}`);
    input.focus();
}

function updatePasswordCapsLockWarning(event) {
    const warning = document.getElementById("auth-password-caps-warning");
    if (!warning || typeof event.getModifierState !== "function") return;
    warning.hidden = !event.getModifierState("CapsLock");
}

function resetPasswordEntryControls() {
    ["auth-current-password", "auth-new-password", "auth-confirm-password"].forEach(id => {
        const input = document.getElementById(id);
        const toggle = document.querySelector(`[data-password-toggle="${id}"]`);
        if (input) {
            input.value = "";
            input.type = "password";
        }
        if (toggle) {
            toggle.textContent = "Show";
            toggle.setAttribute("aria-pressed", "false");
            const fieldName = id === "auth-current-password"
                ? "current password"
                : id === "auth-new-password" ? "new password" : "password confirmation";
            toggle.setAttribute("aria-label", `Show ${fieldName}`);
        }
    });
    const warning = document.getElementById("auth-password-caps-warning");
    if (warning) warning.hidden = true;
    renderAuthPasswordControls();
}

function formatSessionTime(value) {
    const raw = String(value || "").trim();
    if (!raw) return "Unavailable";
    const parsed = new Date(raw);
    if (Number.isNaN(parsed.getTime())) return raw;
    return parsed.toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
}

function createAuthSessionCard(session) {
    const item = document.createElement("article");
    item.className = "session-item";
    const head = document.createElement("div");
    head.className = "session-item-head";
    const title = document.createElement("p");
    title.className = "session-item-title";
    title.textContent = session.clientLabel || "Unknown browser · Unknown OS";
    head.appendChild(title);
    if (session.current) {
        const badge = document.createElement("span");
        badge.className = "pill pill-success";
        badge.textContent = "This session";
        head.appendChild(badge);
    }
    item.appendChild(head);

    const details = document.createElement("dl");
    details.className = "session-item-details";
    [
        ["IP address", session.clientIP || "Unavailable", "ip"],
        ["Created", formatSessionTime(session.createdAt)],
        ["Last activity", formatSessionTime(session.lastSeenAt)],
        ["Expires", formatSessionTime(session.expiresAt)]
    ].forEach(([label, value, kind]) => {
        const group = document.createElement("div");
        const term = document.createElement("dt");
        const description = document.createElement("dd");
        term.textContent = label;
        description.textContent = value;
        if (kind === "ip") {
            description.dataset.sessionIpId = session.id;
            description.dataset.maskedIp = session.clientIP || "Unavailable";
        }
        group.append(term, description);
        details.appendChild(group);
    });
    item.appendChild(details);

    const actions = document.createElement("div");
    actions.className = "session-item-actions";
    const revealButton = document.createElement("button");
    revealButton.type = "button";
    revealButton.className = "btn-ghost session-revoke";
    revealButton.dataset.revealSessionId = session.id;
    revealButton.disabled = !session.clientIP;
    revealButton.textContent = "Reveal IP";
    const visibility = document.createElement("span");
    visibility.className = "session-ip-visibility";
    visibility.dataset.sessionIpVisibility = session.id;
    visibility.hidden = true;
    const revokeButton = document.createElement("button");
    revokeButton.type = "button";
    revokeButton.className = "btn-danger session-revoke";
    revokeButton.dataset.sessionId = session.id;
    revokeButton.dataset.currentSession = String(session.current);
    revokeButton.dataset.adminDangerCommand = "revokeSession";
    revokeButton.textContent = session.current ? "Logout This Session" : "Revoke Session";
    actions.append(revealButton, visibility, revokeButton);
    item.appendChild(actions);
    return item;
}

function renderAuthSessions() {
    hideTemporarySessionIPReveal();
    const view = adminPageView();
    const status = document.getElementById("auth-session-status");
    const currentList = document.getElementById("auth-current-session-list");
    const otherList = document.getElementById("auth-other-session-list");
    const currentGroup = document.getElementById("auth-current-session-group");
    const otherGroup = document.getElementById("auth-other-session-group");
    const otherTitle = document.getElementById("auth-other-session-title");
    const showAll = document.getElementById("auth-sessions-show-all");
    const clearOthers = document.getElementById("auth-sessions-clear-others");
    if (status) status.textContent = `${view.account.sessionCount} active session${view.account.sessionCount === 1 ? "" : "s"}.`;
    if (clearOthers) clearOthers.disabled = view.account.otherSessions.length === 0;
    if (!currentList || !otherList) return;
    currentList.replaceChildren();
    otherList.replaceChildren();
    if (view.account.currentSession) currentList.appendChild(createAuthSessionCard(view.account.currentSession));
    view.account.otherSessions.forEach(session => otherList.appendChild(createAuthSessionCard(session)));
    if (currentGroup) currentGroup.hidden = !view.account.currentSession;
    if (otherGroup) otherGroup.hidden = view.account.otherSessions.length === 0;
    if (otherTitle) otherTitle.textContent = `Other sessions (${view.account.otherSessions.length})`;
    if (showAll) {
        showAll.hidden = view.account.otherSessions.length <= 3;
        showAll.textContent = view.account.otherSessionsExpanded ? "Collapse" : "Show all";
    }
    otherList.classList.toggle("is-expanded", view.account.otherSessionsExpanded);
    renderAuthPasswordControls();
    renderAdminWorkspace();
}

function lockSessionIPRevealBackground(modal) {
    sessionIPReveal.background = Array.from(document.body.children)
        .filter(element => element !== modal)
        .map(element => ({
            element,
            inert: Boolean(element.inert),
            ariaHidden: element.getAttribute("aria-hidden")
        }));
    sessionIPReveal.background.forEach(({ element }) => {
        element.inert = true;
        element.setAttribute("aria-hidden", "true");
    });
    document.body.classList.add("modal-open");
}

function unlockSessionIPRevealBackground() {
    sessionIPReveal.background.forEach(({ element, inert, ariaHidden }) => {
        element.inert = inert;
        if (ariaHidden === null) element.removeAttribute("aria-hidden");
        else element.setAttribute("aria-hidden", ariaHidden);
    });
    sessionIPReveal.background = [];
    document.body.classList.remove("modal-open");
}

function closeSessionIPRevealModal(options = {}) {
    const modal = document.getElementById("session-ip-reveal-modal");
    const password = document.getElementById("session-ip-reveal-password");
    const error = document.getElementById("session-ip-reveal-error");
    const restoreFocus = options.restoreFocus !== false;
    const abortRequest = options.abortRequest !== false;
    const trigger = sessionIPReveal.trigger;
    if (abortRequest && sessionIPReveal.requestController) {
        sessionIPReveal.requestID += 1;
        sessionIPReveal.requestController.abort();
        sessionIPReveal.requestController = null;
    }
    if (modal) {
        modal.classList.remove("active");
        delete modal.dataset.sessionId;
    }
    unlockSessionIPRevealBackground();
    if (password) password.value = "";
    if (error) error.textContent = "";
    sessionIPReveal.trigger = null;
    if (restoreFocus && trigger?.isConnected && typeof trigger.focus === "function") {
        trigger.focus();
    }
}

function openSessionIPRevealModal(sessionID, trigger) {
    const modal = document.getElementById("session-ip-reveal-modal");
    const password = document.getElementById("session-ip-reveal-password");
    if (!modal || !password || !sessionID) return;
    hideTemporarySessionIPReveal();
    modal.dataset.sessionId = sessionID;
    sessionIPReveal.trigger = trigger || null;
    lockSessionIPRevealBackground(modal);
    modal.classList.add("active");
    window.requestAnimationFrame(() => password.focus());
}

function hideTemporarySessionIPReveal() {
    window.clearInterval(sessionIPReveal.intervalID);
    window.clearTimeout(sessionIPReveal.timeoutID);
    const sessionID = sessionIPReveal.sessionID;
    if (sessionID) {
        const ipNode = document.querySelector(`[data-session-ip-id="${CSS.escape(sessionID)}"]`);
        const visibility = document.querySelector(`[data-session-ip-visibility="${CSS.escape(sessionID)}"]`);
        const button = document.querySelector(`button[data-hide-session-ip="${CSS.escape(sessionID)}"]`);
        if (ipNode) ipNode.textContent = ipNode.dataset.maskedIp || "Unavailable";
        if (visibility) {
            visibility.hidden = true;
            visibility.textContent = "";
        }
        if (button) {
            delete button.dataset.hideSessionIp;
            button.dataset.revealSessionId = sessionID;
            button.textContent = "Reveal IP";
            button.disabled = false;
        }
    }
    sessionIPReveal.sessionID = "";
    sessionIPReveal.expiresAt = 0;
    sessionIPReveal.intervalID = 0;
    sessionIPReveal.timeoutID = 0;
}

function updateTemporarySessionIPReveal() {
    if (!sessionIPReveal.sessionID) return;
    const remaining = Math.max(0, Math.ceil((sessionIPReveal.expiresAt - Date.now()) / 1000));
    if (remaining === 0) {
        hideTemporarySessionIPReveal();
        return;
    }
    const visibility = document.querySelector(
        `[data-session-ip-visibility="${CSS.escape(sessionIPReveal.sessionID)}"]`
    );
    if (visibility) visibility.textContent = `Full IP visible for ${remaining} second${remaining === 1 ? "" : "s"}`;
}

function showTemporarySessionIPReveal(sessionID, fullIP, requestedSeconds) {
    hideTemporarySessionIPReveal();
    const ipNode = document.querySelector(`[data-session-ip-id="${CSS.escape(sessionID)}"]`);
    const visibility = document.querySelector(`[data-session-ip-visibility="${CSS.escape(sessionID)}"]`);
    const button = document.querySelector(`button[data-reveal-session-id="${CSS.escape(sessionID)}"]`);
    if (!ipNode || !visibility || !button || !fullIP) return false;
    const seconds = Math.min(30, Math.max(1, Number(requestedSeconds) || 30));
    sessionIPReveal.sessionID = sessionID;
    sessionIPReveal.expiresAt = Date.now() + (seconds * 1000);
    ipNode.textContent = fullIP;
    visibility.hidden = false;
    delete button.dataset.revealSessionId;
    button.dataset.hideSessionIp = sessionID;
    button.textContent = "Hide now";
    button.disabled = false;
    updateTemporarySessionIPReveal();
    sessionIPReveal.intervalID = window.setInterval(updateTemporarySessionIPReveal, 250);
    sessionIPReveal.timeoutID = window.setTimeout(hideTemporarySessionIPReveal, seconds * 1000);
    return true;
}

function handleSessionIPRevealModalKeydown(event) {
    const modal = document.getElementById("session-ip-reveal-modal");
    if (!modal?.classList.contains("active")) return;
    if (event.key === "Escape") {
        event.preventDefault();
        closeSessionIPRevealModal();
        return;
    }
    if (event.key !== "Tab") return;
    const dialog = modal.querySelector('[role="dialog"]');
    const focusable = Array.from(dialog?.querySelectorAll(
        "button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])"
    ) || []);
    if (!focusable.length) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
    }
}

async function submitSessionIPReveal(event) {
    event.preventDefault();
    const modal = document.getElementById("session-ip-reveal-modal");
    const password = document.getElementById("session-ip-reveal-password");
    const error = document.getElementById("session-ip-reveal-error");
    const submit = document.getElementById("session-ip-reveal-submit");
    const sessionID = modal?.dataset.sessionId || "";
    if (!sessionID || !password?.value) {
        if (error) error.textContent = "Current password is required.";
        return;
    }
    if (submit) submit.disabled = true;
    const requestID = ++sessionIPReveal.requestID;
    const requestController = new AbortController();
    sessionIPReveal.requestController?.abort();
    sessionIPReveal.requestController = requestController;
    try {
        const request = typeof window.__nativeFetch === "function" ? window.__nativeFetch : window.fetch;
        const res = await request(`/api/auth/sessions/${encodeURIComponent(sessionID)}/reveal-ip`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ current_password: password.value }),
            cache: "no-store",
            signal: requestController.signal
        });
        if (requestID !== sessionIPReveal.requestID || !modal?.classList.contains("active")) return;
        if (!res.ok) {
            if (error) error.textContent = await parseErrorResponse(res, "Failed to reveal session IP.");
            password.value = "";
            password.focus();
            return;
        }
        const data = await res.json().catch(() => ({}));
        const fullIP = String(data.ip || "").trim();
        if (!fullIP || !showTemporarySessionIPReveal(sessionID, fullIP, data.visible_for_seconds)) {
            if (error) error.textContent = "The full IP address is unavailable.";
            return;
        }
        sessionIPReveal.requestController = null;
        closeSessionIPRevealModal({ abortRequest: false });
    } catch (err) {
        if (err?.name === "AbortError") return;
        if (error) error.textContent = err.message || "Failed to reveal session IP.";
    } finally {
        if (requestID === sessionIPReveal.requestID) {
            sessionIPReveal.requestController = null;
            if (submit) submit.disabled = false;
        }
    }
}

async function fetchAuthSessionStatus() {
    hideTemporarySessionIPReveal();
    const status = document.getElementById("auth-session-status");
    if (!status) return;
    const requestID = beginAdminSnapshot("account");
    try {
        const res = await fetch("/api/auth/sessions");
        if (!res.ok) {
            adminPageInteraction.dispatch({ type: "snapshotFailed", stream: "account", requestID, error: "Session status unavailable." });
            status.textContent = "Session status unavailable.";
            renderAdminWorkspace();
            return;
        }
        const data = await res.json().catch(() => ({}));
        adminPageInteraction.dispatch({ type: "accountSnapshotReceived", requestID, receivedAt: new Date().toISOString(), data });
        renderAuthSessions();
    } catch (err) {
        console.error("Failed to fetch session status:", err);
        adminPageInteraction.dispatch({ type: "snapshotFailed", stream: "account", requestID, error: "Session status request failed." });
        status.textContent = "Session status request failed.";
        renderAdminWorkspace();
    }
}

async function clearOtherAuthSessions() {
    const count = adminPageView().account.otherSessions.length;
    if (!(await window.confirmAction("Review the sessions that will be invalidated before continuing.", {
        danger: true,
        title: "Logout other Admin sessions",
        operation: "Invalidate all other server-side sessions",
        resources: `${count} other signed-in browser session${count === 1 ? "" : "s"}`,
        consequences: "Other browsers will be logged out immediately. This browser remains signed in.",
        reversibility: "Not reversible. Affected users must sign in again.",
        authentication: "Your current signed-in Admin session authorizes this operation.",
        confirmLabel: "Logout Others"
    }))) return;
    const plan = beginAdminCommand("clearOtherSessions");
    if (!plan) return;
    try {
        const res = await fetch("/api/auth/sessions/others", { method: "DELETE" });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to clear other sessions.");
            finishAdminCommand(plan, null, message, true);
            window.notifyApp(message);
            return;
        }
        const data = await res.json().catch(() => ({}));
        finishAdminCommand(plan, data, "Other sessions cleared.");
        await fetchAuthSessionStatus();
        window.notifyApp(`${Number(data.deleted_sessions || 0)} other session(s) logged out.`);
    } catch (err) {
        finishAdminCommand(plan, null, err.message || "Failed to clear other sessions.", true);
        window.notifyApp(err.message || "Failed to clear other sessions.");
    }
}

async function revokeAuthSession(id, current) {
    const session = adminPageView().account.sessions.find(item => item.id === id);
    if (!(await window.confirmAction("Review the selected session before continuing.", {
        danger: true,
        title: current ? "Logout this Admin session" : "Revoke server-side session",
        operation: current ? "Invalidate the current session" : "Revoke one server-side session",
        resources: `${session?.clientLabel || "Selected browser"} · ${session?.clientIP || "IP unavailable"}`,
        consequences: current ? "This browser will return to the sign-in page immediately." : "The selected browser will be logged out immediately.",
        reversibility: "Not reversible. The browser must sign in again.",
        authentication: "Your current signed-in Admin session authorizes this operation.",
        confirmLabel: current ? "Logout" : "Revoke Session"
    }))) return;
    const plan = beginAdminCommand("revokeSession", { id });
    if (!plan) return;
    try {
        const res = await fetch(`/api/auth/sessions/${encodeURIComponent(plan.payload.id)}`, { method: "DELETE" });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to revoke session.");
            finishAdminCommand(plan, null, message, true);
            window.notifyApp(message);
            return;
        }
        finishAdminCommand(plan, null, "Session revoked.");
        if (current) {
            window.location.assign("/login");
            return;
        }
        await fetchAuthSessionStatus();
        window.notifyApp("Session revoked.");
    } catch (err) {
        finishAdminCommand(plan, null, err.message || "Failed to revoke session.", true);
        window.notifyApp(err.message || "Failed to revoke session.");
    }
}

function handleAuthSessionListClick(event) {
    const hideButton = event.target.closest("button[data-hide-session-ip]");
    if (hideButton) {
        hideTemporarySessionIPReveal();
        hideButton.focus();
        return;
    }
    const revealButton = event.target.closest("button[data-reveal-session-id]");
    if (revealButton) {
        openSessionIPRevealModal(revealButton.dataset.revealSessionId || "", revealButton);
        return;
    }
    const button = event.target.closest("button[data-session-id]");
    if (!button) return;
    revokeAuthSession(button.dataset.sessionId || "", button.dataset.currentSession === "true");
}

async function changeAdminPassword() {
    const currentInput = document.getElementById("auth-current-password");
    const newInput = document.getElementById("auth-new-password");
    const confirmInput = document.getElementById("auth-confirm-password");
    const button = document.getElementById("auth-password-save");
    let plan;
    try {
        setAuthPasswordFeedback("", "");
        if (button) button.disabled = true;
        const currentPassword = currentInput?.value || "";
        const newPassword = newInput?.value || "";
        const confirmPassword = confirmInput?.value || "";
        const facts = passwordDraftFacts();
        plan = beginAdminCommand("changePassword", facts);
        if (!plan) {
            const rejected = adminPageInteraction.planCommand("changePassword", facts);
            setAuthPasswordFeedback("", rejected.reason || "Review the password fields.");
            return;
        }
        const res = await fetch("/api/auth/password", {
            method: "PUT",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
                current_password: currentPassword,
                new_password: newPassword,
                confirm_password: confirmPassword,
                invalidate_other_sessions: plan.payload.invalidateOtherSessions
            })
        });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to change password.");
            finishAdminCommand(plan, null, message, true);
            setAuthPasswordFeedback("", message);
            return;
        }
        const data = await res.json().catch(() => ({}));
        const invalidated = Math.max(0, Number(data.invalidated_sessions) || 0);
        const preserved = Math.max(0, Number(data.preserved_sessions) || 0);
        const partial = data.outcome === "partial_failure";
        const message = partial
            ? `Password changed, but other sessions could not be invalidated. ${preserved} active session${preserved === 1 ? "" : "s"} preserved.`
            : data.invalidation_requested
                ? `Password changed. ${invalidated} other session${invalidated === 1 ? "" : "s"} invalidated; ${preserved} active session${preserved === 1 ? "" : "s"} preserved.`
                : `Password changed. 0 sessions invalidated; ${preserved} active session${preserved === 1 ? "" : "s"} preserved.`;
        finishAdminCommand(plan, data, message);
        resetPasswordEntryControls();
        setAuthPasswordFeedback(message, "", partial ? "warning" : "success");
        if (data.invalidation_requested) await fetchAuthSessionStatus();
    } catch (err) {
        finishAdminCommand(plan, null, err.message || "Failed to change password.", true);
        setAuthPasswordFeedback("", err.message || "Failed to change password.");
    } finally {
        renderAuthPasswordControls();
    }
}

async function clearAuthSessions() {
    const count = adminPageView().account.sessionCount;
    if (!(await window.confirmTypedAction("Type the confirmation text to invalidate every session.", "LOGOUT ALL", {
        title: "Logout every Admin session",
        operation: "Invalidate all server-side sessions",
        resources: `${count} signed-in browser session${count === 1 ? "" : "s"}`,
        consequences: "Every browser, including this one, will be logged out immediately.",
        reversibility: "Not reversible. Every user must sign in again.",
        authentication: "Typed confirmation is required; the current Admin session authorizes the request.",
        confirmLabel: "Logout All Sessions"
    }))) {
        return;
    }
    const plan = beginAdminCommand("clearSessions");
    if (!plan) return;
    try {
        const res = await fetch("/api/auth/sessions", { method: "DELETE" });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to clear sessions.");
            finishAdminCommand(plan, null, message, true);
            window.notifyApp(message);
            return;
        }
        finishAdminCommand(plan, { count: 0 }, "Sessions cleared.");
        adminPageInteraction.dispatch({ type: "metricsTokenHidden" });
        window.location.assign("/login");
    } catch (err) {
        finishAdminCommand(plan, null, err.message || "Failed to clear sessions.", true);
        window.notifyApp(err.message || "Failed to clear sessions.");
    }
}

function showMetricsTokenOnce(token) {
    const panel = document.getElementById("metrics-token-once");
    const value = document.getElementById("metrics-token-value");
    if (!panel || !value) return;
    if (!token) {
        adminPageInteraction.dispatch({ type: "metricsTokenHidden" });
        value.textContent = "";
        panel.style.display = "none";
        return;
    }
    value.textContent = token;
    panel.style.display = "block";
}

async function fetchMetricsTokenStatus(resetReveal = true) {
    const status = document.getElementById("metrics-token-status");
    if (!status) return;
    if (resetReveal) {
        showMetricsTokenOnce("");
    }
    const requestID = beginAdminSnapshot("metrics");
    try {
        const res = await fetch("/api/metrics/token");
        if (!res.ok) {
            adminPageInteraction.dispatch({ type: "snapshotFailed", stream: "metrics", requestID, error: "Metrics token status: unknown" });
            status.textContent = "Metrics token status: unknown";
            renderAdminWorkspace();
            return;
        }
        const data = await res.json().catch(() => ({}));
        adminPageInteraction.dispatch({ type: "metricsSnapshotReceived", requestID, receivedAt: new Date().toISOString(), data });
        status.textContent = data.enabled ? "Metrics API token: enabled" : "Metrics API token: disabled";
        renderAdminWorkspace();
    } catch (err) {
        console.error("Failed to fetch metrics token status:", err);
        adminPageInteraction.dispatch({ type: "snapshotFailed", stream: "metrics", requestID, error: "Metrics token status: request failed" });
        status.textContent = "Metrics token status: request failed";
        renderAdminWorkspace();
    }
}

async function rotateMetricsToken(askConfirm) {
    if (askConfirm && !(await window.confirmTypedAction("Type the confirmation text to replace the current Metrics API credential.", "ROTATE TOKEN", {
        title: "Rotate Metrics API token",
        operation: "Replace the active Metrics API token",
        resources: "The current /metrics credential and every scraper using it",
        consequences: "Existing scrapers will fail until they receive the new one-time token.",
        reversibility: "The previous token cannot be restored.",
        authentication: "Typed confirmation is required; the new token is displayed once.",
        confirmLabel: "Rotate Token"
    }))) {
        return;
    }
    const plan = beginAdminCommand("rotateMetricsToken");
    if (!plan) return;
    try {
        const res = await fetch("/api/metrics/token", { method: "POST" });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to rotate metrics token.");
            finishAdminCommand(plan, null, message, true);
            window.notifyApp(message);
            return;
        }
        const data = await res.json().catch(() => ({}));
        const token = (data && typeof data.token === "string") ? data.token : "";
        if (!token) {
            finishAdminCommand(plan, data, "Token rotation succeeded but no token was returned.", true);
            window.notifyApp("Token rotation succeeded but no token was returned.");
            return;
        }
        finishAdminCommand(plan, data, "Metrics token rotated.");
        showMetricsTokenOnce(token);
        fetchMetricsTokenStatus(false);
    } catch (err) {
        console.error("Failed to rotate metrics token:", err);
        finishAdminCommand(plan, null, "Failed to rotate metrics token.", true);
        window.notifyApp("Failed to rotate metrics token.");
    }
}

async function disableMetricsToken() {
    if (!(await window.confirmTypedAction("Type the confirmation text to disable authenticated Metrics API access.", "DISABLE METRICS", {
        title: "Disable Metrics API token",
        operation: "Disable the active Metrics API token",
        resources: "The /metrics endpoint and every configured scraper",
        consequences: "Metrics collection stops immediately for clients using this token.",
        reversibility: "Access can be restored only by generating a new token.",
        authentication: "Typed confirmation is required; the current Admin session authorizes the request.",
        confirmLabel: "Disable Token"
    }))) {
        return;
    }
    const plan = beginAdminCommand("disableMetricsToken");
    if (!plan) return;
    try {
        const res = await fetch("/api/metrics/token", { method: "DELETE" });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to disable metrics token.");
            finishAdminCommand(plan, null, message, true);
            window.notifyApp(message);
            return;
        }
        finishAdminCommand(plan, { enabled: false }, "Metrics token disabled.");
        showMetricsTokenOnce("");
        fetchMetricsTokenStatus();
    } catch (err) {
        console.error("Failed to disable metrics token:", err);
        finishAdminCommand(plan, null, "Failed to disable metrics token.", true);
        window.notifyApp("Failed to disable metrics token.");
    }
}

async function copyMetricsToken() {
    const token = adminPageView().metrics.revealedToken;
    if (!token) {
        window.notifyApp("No token to copy.");
        return;
    }
    try {
        await navigator.clipboard.writeText(token);
        window.notifyApp("Metrics token copied.");
    } catch (_) {
        window.notifyApp("Failed to copy token. Copy it manually from the box.");
    }
}

function deriveDownloadFilename(contentDisposition) {
    if (!contentDisposition) return "";
    const utf8Match = contentDisposition.match(/filename\*=UTF-8''([^;]+)/i);
    if (utf8Match && utf8Match[1]) {
        try {
            return decodeURIComponent(utf8Match[1]).replace(/[\r\n]/g, "");
        } catch (_) {
            return utf8Match[1].replace(/[\r\n]/g, "");
        }
    }
    const simpleMatch = contentDisposition.match(/filename="?([^";]+)"?/i);
    if (!simpleMatch || !simpleMatch[1]) return "";
    return simpleMatch[1].replace(/[\r\n]/g, "");
}

function formatBackupBytes(value) {
    const bytes = Math.max(0, Number(value) || 0);
    if (bytes < 1024) return `${bytes} B`;
    const units = ["KB", "MB", "GB"];
    let amount = bytes;
    let unit = "B";
    for (const candidate of units) {
        amount /= 1024;
        unit = candidate;
        if (amount < 1024) break;
    }
    return `${amount >= 10 ? amount.toFixed(0) : amount.toFixed(1)} ${unit}`;
}

function recoveryStateLabel(state) {
    return {
        healthy: "Healthy",
        stale: "Stale",
        never: "Never recorded",
        failed: "Failed",
        unavailable: "Unavailable"
    }[state] || "Unavailable";
}

function renderRecoveryOperation(prefix, operation) {
    const value = document.getElementById(`backup-recovery-${prefix}`);
    const detail = document.getElementById(`backup-recovery-${prefix}-detail`);
    if (!value || !detail) return;
    if (!operation || !operation.lastSuccessAt) {
        value.textContent = recoveryStateLabel(operation?.state || "unavailable");
        detail.textContent = operation?.message || "Recovery evidence is unavailable.";
        return;
    }
    const size = operation.sizeBytes === null ? "" : ` · ${formatBackupBytes(operation.sizeBytes)}`;
    value.textContent = `${formatSessionTime(operation.lastSuccessAt)}${size}`;
    if (operation.state === "failed" && operation.lastAttemptAt) {
        detail.textContent = `Latest attempt failed ${formatSessionTime(operation.lastAttemptAt)}. ${operation.message || ""}`.trim();
        return;
    }
    detail.textContent = operation.message || recoveryStateLabel(operation.state);
}

function renderBackupRecoveryHealth() {
    const health = adminPageView().backup.recoveryHealth;
    const state = health?.state || "unavailable";
    const badge = document.getElementById("backup-recovery-health-state");
    if (badge) {
        badge.textContent = recoveryStateLabel(state);
        const className = state === "healthy"
            ? "pill-success"
            : state === "stale"
                ? "pill-warning"
                : state === "failed"
                    ? "pill-danger"
                    : "pill-muted";
        badge.className = `pill ${className}`;
    }
    const message = document.getElementById("backup-recovery-health-message");
    if (message) message.textContent = health?.message || "Recovery evidence is unavailable.";
    renderRecoveryOperation("export", health?.export);
    renderRecoveryOperation("verification", health?.verification);

    const next = document.getElementById("backup-recovery-next");
    if (next) {
        next.textContent = health?.schedule?.scheduled && health.schedule.nextBackupAt
            ? formatSessionTime(health.schedule.nextBackupAt)
            : health?.schedule?.message || "No backup is scheduled.";
    }
    const threshold = document.getElementById("backup-recovery-threshold");
    if (threshold) {
        const hours = Number(health?.staleAfterHours) || 0;
        threshold.textContent = hours > 0 && hours % 24 === 0
            ? `${hours / 24} day${hours / 24 === 1 ? "" : "s"} (${hours} hours)`
            : hours > 0 ? `${hours} hours` : "Unavailable";
    }
    const checked = document.getElementById("backup-recovery-checked");
    if (checked) checked.textContent = health?.checkedAt ? `Checked ${formatSessionTime(health.checkedAt)}.` : "Evidence has not been checked.";
    const evidence = document.getElementById("backup-recovery-retention-evidence");
    if (evidence) evidence.textContent = health?.retention?.evidenceDescription || "Recovery evidence is unavailable.";
    const archive = document.getElementById("backup-recovery-retention-archive");
    if (archive) archive.textContent = health?.retention?.archiveDescription || "Exported archives are operator-managed.";
}

function renderBackupIssueList(containerID, issues) {
    const container = document.getElementById(containerID);
    const list = container?.querySelector("ul");
    if (!container || !list) return;
    const values = Array.isArray(issues) ? issues : [];
    container.hidden = values.length === 0;
    list.innerHTML = values.map(issue => `<li>${escapeHtml(issue?.message || "Unknown restore-readiness issue.")}</li>`).join("");
}

function renderBackupRestoreReview() {
    const view = adminPageView().backup;
    const review = view.review;
    const panel = document.getElementById("backup-restore-review");
    const empty = document.getElementById("backup-review-empty");
    if (!panel || !empty) return;
    panel.hidden = !review;
    empty.hidden = Boolean(review);
    if (!review) {
        empty.textContent = view.selectedFile
            ? "Review this exact archive and passphrase before replacement."
            : "Select an archive and review its restore impact before replacement.";
        renderAdminDestructiveControls();
        return;
    }

    const readiness = document.getElementById("backup-review-readiness");
    readiness.textContent = review.restoreReady ? "Ready for confirmation" : "Blocked";
    readiness.className = `pill ${review.restoreReady ? "pill-success" : "pill-danger"}`;
    document.getElementById("backup-review-archive").textContent = `${review.archive.format || "Unknown format"} · version ${review.archive.version || "unknown"}`;
    document.getElementById("backup-review-created").textContent = formatSessionTime(review.archive.createdAt);
    document.getElementById("backup-review-size").textContent = formatBackupBytes(review.archive.sizeBytes);
    document.getElementById("backup-review-restart").textContent = review.impact.restartRequired ? "Required" : "Not required";

    const resources = Array.isArray(review.resources) ? review.resources : [];
    document.getElementById("backup-review-resources").innerHTML = resources.map(resource => `
        <li>
            <strong>${escapeHtml(resource.name || "Unknown resource")}</strong>:
            ${resource.included ? `included (${escapeHtml(formatBackupBytes(resource.size_bytes))})` : resource.required ? "missing (required)" : "not included (optional)"}
        </li>
    `).join("") || "<li>No resource inventory was returned.</li>";

    const counts = review.safeCounts || {};
    document.getElementById("backup-review-counts").innerHTML = [
        ["Servers", counts.servers],
        ["Policies", counts.policies],
        ["Jobs", counts.jobs],
        ["Sessions", counts.sessions]
    ].map(([label, value]) => `<div><dt>${label}</dt><dd>${Number(value) || 0}</dd></div>`).join("");

    const impact = review.impact || {};
    document.getElementById("backup-review-impact").innerHTML = [
        impact.sessionsInvalidated ? "All active Admin sessions will be invalidated." : "Admin sessions are not expected to be invalidated.",
        impact.metricsAccessReplaced ? "The Metrics API credential will be replaced by archived state." : "Metrics API access is not expected to change.",
        impact.maintenanceRequired ? "Exclusive maintenance mode will be activated." : "Maintenance mode is not required.",
        impact.downtimeExpected ? "Requests will pause during replacement and runtime reload." : "No request downtime is expected.",
        impact.restartRequired ? "An application restart is required." : "No application restart is required."
    ].map(message => `<li>${escapeHtml(message)}</li>`).join("");

    renderBackupIssueList("backup-review-blockers", review.blockers);
    renderBackupIssueList("backup-review-warnings", review.warnings);
    renderAdminDestructiveControls();
}

async function fetchBackupStatus() {
    const status = document.getElementById("backup-status");
    if (!status) return;
    const requestID = beginAdminSnapshot("backup");
    try {
        const res = await fetch("/api/backup/status");
        if (!res.ok) {
            adminPageInteraction.dispatch({ type: "snapshotFailed", stream: "backup", requestID, error: "Backup status: unavailable" });
            status.textContent = "Backup status: unavailable";
            renderAdminWorkspace();
            return;
        }
        const data = await res.json().catch(() => ({}));
        adminPageInteraction.dispatch({ type: "backupSnapshotReceived", requestID, receivedAt: new Date().toISOString(), data });
        renderBackupRecoveryHealth();
        const knownHostsState = data.known_hosts_exists ? "present" : "missing";
        status.textContent = `Backup paths: DB=${data.db_path || "-"}, config=${data.config_path || "-"}, known_hosts=${data.known_hosts_path || "-"} (${knownHostsState})`;
        renderAdminWorkspace();
    } catch (err) {
        console.error("Failed to fetch backup status:", err);
        adminPageInteraction.dispatch({ type: "snapshotFailed", stream: "backup", requestID, error: "Backup status: request failed" });
        status.textContent = "Backup status: request failed";
        renderAdminWorkspace();
    }
}

async function exportBackup() {
    const exportPassInput = document.getElementById("backup-export-passphrase");
    const exportPassConfirmInput = document.getElementById("backup-export-passphrase-confirm");
    let plan;
    try {
        const pass = exportPassInput?.value || "";
        const confirmPass = exportPassConfirmInput?.value || "";
        const includeKnownHosts = !!document.getElementById("backup-include-known-hosts")?.checked;
        if (pass.length < 12) {
            window.notifyApp("Passphrase must be at least 12 characters.");
            return;
        }
        if (pass !== confirmPass) {
            window.notifyApp("Passphrase confirmation does not match.");
            return;
        }
        plan = beginAdminCommand("exportBackup", { passphraseValid: pass.length >= 12, passwordsMatch: pass === confirmPass, includeKnownHosts });
        if (!plan) {
            window.notifyApp(adminPageInteraction.planCommand("exportBackup").reason || "Backup is unavailable.");
            return;
        }
        const res = await fetch("/api/backup/export", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ passphrase: pass, include_known_hosts: includeKnownHosts })
        });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to export backup.");
            finishAdminCommand(plan, null, message, true);
            window.notifyApp(message);
            return;
        }
        const blob = await res.blob();
        const filename = deriveDownloadFilename(res.headers.get("Content-Disposition")) || `simplelinuxupdater-backup-${Date.now()}.slubkp`;
        const url = URL.createObjectURL(blob);
        const link = document.createElement("a");
        link.href = url;
        link.download = filename;
        document.body.appendChild(link);
        link.click();
        link.remove();
        URL.revokeObjectURL(url);
        finishAdminCommand(plan, null, "Backup exported.");
        window.notifyApp("Backup exported.");
    } catch (err) {
        console.error("Failed to export backup:", err);
        finishAdminCommand(plan, null, "Failed to export backup.", true);
        window.notifyApp("Failed to export backup.");
    } finally {
        if (exportPassInput) exportPassInput.value = "";
        if (exportPassConfirmInput) exportPassConfirmInput.value = "";
    }
}

async function restoreBackup() {
    const fileInput = document.getElementById("backup-restore-file");
    const restorePassInput = document.getElementById("backup-restore-passphrase");
    let plan;
    let clearCredential = false;
    try {
        const pass = restorePassInput?.value || "";
        const file = fileInput?.files?.[0];
        if (!file) {
            window.notifyApp("Choose a backup file first.");
            return;
        }
        if (pass.length < 12) {
            window.notifyApp("Passphrase must be at least 12 characters.");
            return;
        }
        const readiness = adminPageInteraction.planCommand("restoreBackup", { passphraseValid: pass.length >= 12 });
        if (!readiness.enabled) {
            window.notifyApp(readiness.reason || "Review restore readiness before replacement.");
            return;
        }
        const review = adminPageView().backup.review;
        const counts = review?.safeCounts || {};
        if (!(await window.confirmTypedAction("Type the confirmation text only after verifying this backup.", "RESTORE", {
            title: "Restore application backup",
            operation: "Replace application data from an encrypted backup",
            resources: `${file.name}; ${Number(counts.servers) || 0} servers, ${Number(counts.policies) || 0} policies, ${Number(counts.jobs) || 0} jobs, ${Number(counts.sessions) || 0} archived sessions`,
            consequences: "Current application data is replaced, all active Admin sessions are invalidated, the Metrics API credential is replaced, and requests pause during exclusive maintenance.",
            reversibility: "Not automatically reversible. Export a current backup first if rollback may be needed.",
            authentication: "The backup passphrase and typed confirmation are both required.",
            confirmLabel: "Restore Backup"
        }))) {
            return;
        }
        plan = beginAdminCommand("restoreBackup", { passphraseValid: pass.length >= 12 });
        if (!plan) {
            window.notifyApp(adminPageInteraction.planCommand("restoreBackup", { passphraseValid: pass.length >= 12 }).reason || "Restore review is no longer current.");
            return;
        }
        const form = new FormData();
        form.append("file", file);
        form.append("passphrase", pass);
        clearCredential = true;
        const res = await fetch("/api/backup/restore", {
            method: "POST",
            body: form
        });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to restore backup.");
            finishAdminCommand(plan, null, message, true);
            window.notifyApp(message);
            return;
        }
        const payload = await res.json().catch(() => ({}));
        finishAdminCommand(plan, payload, "Backup restored successfully.");
        window.notifyApp("Backup restored successfully.");
        if (fileInput) {
            fileInput.value = "";
            updateFileLabel(fileInput, "Choose backup file");
        }
        renderBackupRestoreReview();
        if (payload.sessions_invalidated) {
            window.location.assign("/login");
            return;
        }
        await fetchBackupStatus();
    } catch (err) {
        console.error("Failed to restore backup:", err);
        finishAdminCommand(plan, null, "Failed to restore backup.", true);
        window.notifyApp("Failed to restore backup.");
    } finally {
        if (clearCredential) {
            if (restorePassInput) restorePassInput.value = "";
            adminPageInteraction.dispatch({ type: "backupPassphraseChanged", valid: false });
            renderBackupRestoreReview();
        }
    }
}

async function verifyBackup() {
    const fileInput = document.getElementById("backup-restore-file");
    const restorePassInput = document.getElementById("backup-restore-passphrase");
    let plan;
    try {
        const pass = restorePassInput?.value || "";
        const file = fileInput?.files?.[0];
        if (!file) {
            window.notifyApp("Choose a backup file first.");
            return;
        }
        if (pass.length < 12) {
            window.notifyApp("Passphrase must be at least 12 characters.");
            return;
        }
        adminPageInteraction.dispatch({ type: "backupFileSelected", file });
        plan = beginAdminCommand("verifyBackup", { passphraseValid: pass.length >= 12 });
        if (!plan) return;
        const form = new FormData();
        form.append("file", file);
        form.append("passphrase", pass);
        const res = await fetch("/api/backup/verify", {
            method: "POST",
            body: form
        });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to verify backup.");
            finishAdminCommand(plan, null, message, true);
            window.notifyApp(message);
            return;
        }
        const payload = await res.json().catch(() => ({}));
        const message = payload.restore_ready
            ? "Restore readiness review completed."
            : "Restore readiness review completed with blockers.";
        finishAdminCommand(plan, payload, message);
        renderBackupRestoreReview();
        window.notifyApp(message);
    } catch (err) {
        console.error("Failed to verify backup:", err);
        finishAdminCommand(plan, null, "Failed to verify backup.", true);
        renderBackupRestoreReview();
        window.notifyApp("Failed to verify backup.");
    }
}

function weekdayOrder(token) {
    return weekdayOptions.findIndex((item) => item.value === token);
}

function normalizeWeekdayToken(raw) {
    switch (String(raw || "").trim().toLowerCase()) {
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
    return normalized.length ? normalized.map(formatWeekdayLabel).join(", ") : "No weekdays selected";
}

function humanizeExecutionMode(mode) {
    switch (String(mode || "").trim()) {
        case "scan_only":
            return "Scan only";
        case "approval_required":
            return "Approval required";
        case "auto_apply":
            return "Auto apply";
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
    const node = document.getElementById(kind === "global" ? "scheduled-global-blackouts-json-status" : "policy-blackouts-json-status");
    if (!node) return;
    node.textContent = String(message || "").trim();
    node.classList.toggle("form-feedback-error", !!message && isError);
    node.classList.toggle("form-feedback-success", !!message && !isError);
}

function syncBlackoutTextarea(kind) {
    const textarea = document.getElementById(kind === "global" ? "scheduled-global-blackouts-json" : "policy-blackouts-json");
    if (!textarea) return;
    textarea.value = JSON.stringify(scheduledPolicyRows(kind), null, 2);
}

function setBlackoutEditorRows(kind, rows) {
    scheduledPolicyAdministration.dispatch({ type: "blackoutRowsReceived", kind, rows });
    renderBlackoutEditor(kind);
}

function buildBlackoutWeekdayButtons(kind, row, index) {
    return weekdayOptions.map((day) => {
        const isActive = row.weekdays.includes(day.value);
        const active = isActive ? " active" : "";
        return `<button class="day-chip${active}" type="button" aria-pressed="${isActive ? "true" : "false"}" aria-label="${escapeHtml(day.fullLabel)}" data-blackout-kind="${escapeHtml(kind)}" data-blackout-action="toggle-day" data-index="${escapeHtml(String(index))}" data-day="${escapeHtml(day.value)}">${escapeHtml(day.label)}</button>`;
    }).join("");
}

function blackoutRowSummaryText(row) {
    const weekdays = normalizeWeekdays(Array.isArray(row?.weekdays) ? row.weekdays : []);
    const startTime = String(row?.start_time || "").trim() || "--:--";
    const endTime = String(row?.end_time || "").trim() || "--:--";
    return `${formatWeekdayList(weekdays)} · ${startTime} to ${endTime}`;
}

function updateBlackoutRowSummary(kind, index) {
    const row = scheduledPolicyRows(kind)[index];
    const rowsID = kind === "global" ? "global-blackout-rows" : "policy-blackout-rows";
    const summary = document.querySelector(`#${rowsID} [data-blackout-row-index="${String(index)}"] [data-blackout-summary]`);
    if (!row || !summary) return;
    summary.textContent = blackoutRowSummaryText(row);
}

function renderBlackoutEditor(kind) {
    const rows = scheduledPolicyRows(kind);
    const container = document.getElementById(kind === "global" ? "global-blackout-rows" : "policy-blackout-rows");
    if (!container) return;
    if (!rows.length) {
        container.innerHTML = '<div class="empty-editor-state subtle">No no-run windows yet.</div>';
        syncBlackoutTextarea(kind);
        if (kind === "policy") updatePolicySummary();
        else renderGlobalSettingsDraftState();
        return;
    }
    container.innerHTML = rows.map((row, index) => `
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
    `).join("");
    syncBlackoutTextarea(kind);
    if (kind === "policy") updatePolicySummary();
    else renderGlobalSettingsDraftState();
}

function addBlackoutRow(kind) {
    scheduledPolicyAdministration.dispatch({ type: "blackoutRowAdded", kind });
    setBlackoutJsonStatus(kind, "");
    renderBlackoutEditor(kind);
}

function setPolicyFeedback(status, error = "") {
    const statusNode = document.getElementById("update-policy-status");
    const errorNode = document.getElementById("update-policy-error");
    if (statusNode) statusNode.textContent = String(status || "").trim();
    if (errorNode) errorNode.textContent = String(error || "").trim();
}

function setScheduledSettingsFeedback(status, error = "") {
    const statusNode = document.getElementById("scheduled-settings-status");
    const errorNode = document.getElementById("scheduled-settings-error");
    if (statusNode) statusNode.textContent = String(status || "").trim();
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
}

function setPolicyWeekdays(weekdays) {
    scheduledPolicyAdministration.dispatch({ type: "editorChanged", patch: { weekdays, cadence_kind: document.getElementById("policy-cadence-kind")?.value || "daily" } });
    const selectedWeekdays = scheduledPolicyView().editor.draft.weekdays;
    document.querySelectorAll("#policy-weekdays-picker .day-chip").forEach((button) => {
        const day = button.dataset.weekday || "";
        const isActive = selectedWeekdays.includes(day);
        button.classList.toggle("active", isActive);
        button.setAttribute("aria-pressed", isActive ? "true" : "false");
    });
    updatePolicySummary();
    schedulePolicyPreview();
}

function togglePolicyWeekday(day) {
    scheduledPolicyAdministration.dispatch({ type: "policyWeekdayToggled", day });
    const weekdays = scheduledPolicyView().editor.draft.weekdays;
    document.querySelectorAll("#policy-weekdays-picker .day-chip").forEach((button) => {
        const active = weekdays.includes(button.dataset.weekday || "");
        button.classList.toggle("active", active);
        button.setAttribute("aria-pressed", active ? "true" : "false");
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
    const cadence = document.getElementById("policy-cadence-kind").value;
    const executionMode = document.getElementById("policy-execution-mode").value;
    const weekdaySection = document.getElementById("policy-weekday-section");
    const approvalWrap = document.getElementById("policy-approval-timeout-wrap");
    if (weekdaySection) {
        weekdaySection.classList.toggle("is-hidden", cadence !== "weekly");
    }
    if (approvalWrap) {
        approvalWrap.classList.toggle("is-hidden", executionMode !== "approval_required");
    }
    if (executionMode === "approval_required") {
        const timeoutInput = document.getElementById("policy-approval-timeout");
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
    const submitting = view.commands.inFlightPolicyIDs.includes(operationKey);
    const bar = document.getElementById("policy-draft-action-bar");
    const validation = document.getElementById("policy-draft-validation");
    const save = document.getElementById("policy-save-btn");
    const discard = document.getElementById("policy-discard-btn");
    if (bar) bar.hidden = !editor.dirty;
    if (validation) {
        validation.textContent = editor.valid
            ? "Ready to save this section."
            : (editor.validationMessage || "Complete required fields to save.");
    }
    if (save) {
        save.disabled = !editor.dirty || !editor.valid || submitting;
        save.textContent = editor.draft.id ? "Update Policy" : "Create Policy";
    }
    if (discard) discard.disabled = !editor.dirty || submitting;
}

function renderGlobalSettingsDraftState() {
    const view = scheduledPolicyView();
    const state = view.globalSettings;
    const submitting = view.commands.globalSettingsInFlight;
    const label = document.getElementById("scheduled-settings-draft-state");
    const save = document.getElementById("scheduled-settings-save");
    const discard = document.getElementById("scheduled-settings-discard");
    if (label) {
        label.textContent = state.dirty
            ? (state.valid ? "Unsaved global schedule changes" : state.message)
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

function renderPolicyPreviewList(items, emptyText, includeReason = false) {
    if (!Array.isArray(items) || !items.length) {
        return `<span class="subtle">${escapeHtml(emptyText)}</span>`;
    }
    return items.map((item) => {
        const name = escapeHtml(item?.name || "");
        const reason = includeReason && item?.reason ? ` · ${policyPreviewReasonLabel(item.reason)}` : "";
        const title = Array.isArray(item?.tags) && item.tags.length ? ` title="${escapeHtml(`Tags: ${item.tags.join(", ")}`)}"` : "";
        return `<span class="preview-chip${includeReason ? " preview-chip-muted" : ""}"${title}>${name}${escapeHtml(reason)}</span>`;
    }).join("");
}

function policyPreviewAdmissionLabel(outcome) {
    switch (String(outcome || "")) {
        case "admitted":
            return "Expected: admitted";
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
    const matchedServerCount = Number.isFinite(Number(occurrence?.matched_server_count))
        ? Number(occurrence.matched_server_count)
        : (Array.isArray(occurrence?.matched_servers) ? occurrence.matched_servers.length : 0);
    const windows = Array.isArray(occurrence?.applicable_no_run_windows) ? occurrence.applicable_no_run_windows : [];
    const clock = [
        occurrence?.abbreviation,
        occurrence?.offset,
        occurrence?.timezone
    ].filter(Boolean).join(" · ");
    const canonical = occurrence?.canonical_choice === "earlier_fallback_occurrence"
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
        .map((item) => `<div class="${className}">${escapeHtml(item?.message || item || "")}</div>`)
        .join("");
}

function renderPolicyConflictWindow(window) {
    const effective = Boolean(window?.effective);
    const suppressedByNoRun = window?.draft_admission_outcome === "blocked_no_run" ||
        window?.competing_admission_outcome === "blocked_no_run";
    const outcome = effective
        ? "Effective overlap"
        : (suppressedByNoRun ? "Suppressed by no-run" : "Inactive overlap");
    const admissions = [
        `Draft: ${policyPreviewAdmissionLabel(window?.draft_admission_outcome).replace("Expected: ", "")}`,
        `Competing: ${policyPreviewAdmissionLabel(window?.competing_admission_outcome).replace("Expected: ", "")}`
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
    const sharedServers = Array.isArray(conflict?.shared_servers) ? conflict.shared_servers : [];
    const windows = Array.isArray(conflict?.occurrence_windows) ? conflict.occurrence_windows : [];
    const overlapLabel = conflict?.overlap_kind === "full" ? "Full target overlap" : "Partial target overlap";
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
                <div class="preview-list">${renderPolicyPreviewList(sharedServers.map((name) => ({ name })), "None")}</div>
            </div>
            <ol class="preview-conflict-windows">
                ${windows.map(renderPolicyConflictWindow).join("")}
            </ol>
        </article>
    `;
}

function renderPolicyPreview(preview) {
    const matched = Array.isArray(preview?.matched_servers) ? preview.matched_servers : [];
    const excluded = Array.isArray(preview?.excluded_servers) ? preview.excluded_servers : [];
    const disabled = Array.isArray(preview?.disabled_by_override) ? preview.disabled_by_override : [];
    const occurrences = Array.isArray(preview?.upcoming_occurrences) ? preview.upcoming_occurrences : [];
    const conflicts = Array.isArray(preview?.schedule_conflicts) ? preview.schedule_conflicts : [];
    const validationErrors = Array.isArray(preview?.validation_errors) ? preview.validation_errors : [];
    const operationalWarnings = Array.isArray(preview?.operational_warnings)
        ? preview.operational_warnings
        : (Array.isArray(preview?.warnings) ? preview.warnings : []);
    const informationalFacts = Array.isArray(preview?.informational_facts) ? preview.informational_facts : [];
    const skipped = [...disabled.map((item) => ({ ...item, reason: "disabled_by_override" })), ...excluded];
    document.getElementById("policy-preview-summary").textContent = matched.length
        ? `${pluralize(matched.length, "server", "servers")} would match this policy.`
        : "No current server would match this policy.";
    document.getElementById("policy-preview-count").textContent = `${matched.length} matched`;
    document.getElementById("policy-preview-matched").innerHTML = renderPolicyPreviewList(matched, "None");
    document.getElementById("policy-preview-skipped").innerHTML = renderPolicyPreviewList(skipped, "None", true);
    document.getElementById("policy-preview-occurrence-count").textContent = `${occurrences.length} projected`;
    document.getElementById("policy-preview-occurrences").innerHTML = occurrences.length
        ? occurrences.map(renderPolicyPreviewOccurrence).join("")
        : '<li class="subtle">No canonical occurrence is available for this draft.</li>';
    document.getElementById("policy-preview-conflict-count").textContent = pluralize(conflicts.length, "policy", "policies");
    document.getElementById("policy-preview-conflicts").innerHTML = conflicts.length
        ? conflicts.map(renderPolicyConflict).join("")
        : '<span class="subtle">No enabled policy overlap is projected.</span>';
    renderPolicyPreviewDiagnostics("policy-preview-validation-errors", validationErrors, "preview-error");
    renderPolicyPreviewDiagnostics("policy-preview-warnings", operationalWarnings, "preview-warning");
    renderPolicyPreviewDiagnostics("policy-preview-facts", informationalFacts, "preview-fact");
}

function setPolicyPreviewMessage(message, countText = "0 matched") {
    document.getElementById("policy-preview-summary").textContent = message;
    document.getElementById("policy-preview-count").textContent = countText;
    document.getElementById("policy-preview-matched").innerHTML = '<span class="subtle">None</span>';
    document.getElementById("policy-preview-skipped").innerHTML = '<span class="subtle">None</span>';
    document.getElementById("policy-preview-occurrence-count").textContent = "0 projected";
    document.getElementById("policy-preview-occurrences").innerHTML = '<li class="subtle">Complete the policy fields to project its schedule.</li>';
    document.getElementById("policy-preview-conflict-count").textContent = "0 policies";
    document.getElementById("policy-preview-conflicts").innerHTML = '<span class="subtle">Complete the policy fields to project competing policy overlaps.</span>';
    renderPolicyPreviewDiagnostics("policy-preview-validation-errors", [], "preview-error");
    renderPolicyPreviewDiagnostics("policy-preview-warnings", [], "preview-warning");
    renderPolicyPreviewDiagnostics("policy-preview-facts", [], "preview-fact");
}

async function refreshPolicyPreview() {
    const effect = scheduledPolicyAdministration.dispatch({ type: "previewRequested" }).find((item) => item.type === "fetchPreview");
    if (!effect) {
        setPolicyPreviewMessage(scheduledPolicyView().editor.preview.message || "Complete policy fields to preview matching servers.");
        return;
    }
    setPolicyPreviewMessage("Refreshing target preview...", "...");
    try {
        const res = await fetch("/api/update-policies/preview", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(effect.payload)
        });
        if (!res.ok) {
            throw new Error(await parseErrorResponse(res, "Failed to preview scheduled policy."));
        }
        const data = await res.json().catch(() => ({}));
        scheduledPolicyAdministration.dispatch({ type: "previewReceived", requestId: effect.requestId, preview: data });
        renderPolicyPreview(scheduledPolicyView().editor.preview.data);
    } catch (err) {
        scheduledPolicyAdministration.dispatch({ type: "previewFailed", requestId: effect.requestId, error: err.message || "Failed to preview scheduled policy." });
        setPolicyPreviewMessage(scheduledPolicyView().editor.preview.message);
    }
}

function schedulePolicyPreview() {
    clearTimeout(scheduledPolicyPreviewTimer);
    scheduledPolicyPreviewTimer = window.setTimeout(refreshPolicyPreview, 250);
}

function formatCadence(policy) {
    const timeLocal = policy.time_local || "--:--";
    if (policy.cadence_kind === "weekly") {
        return `Every ${formatWeekdayList(policy.weekdays)} at ${timeLocal}`;
    }
    return `Daily at ${timeLocal}`;
}

function renderPolicyExecution(policy) {
	const mode = humanizeExecutionMode(policy.execution_mode);
	const scope = humanizePackageScope(policy.package_scope);
	const upgradeMode = humanizeUpgradeMode(policy.upgrade_mode || "standard");
	const timeout = policy.execution_mode === "approval_required"
		? ` · ${policy.approval_timeout_minutes || 720} minute approval window`
		: "";
	return `
		<div>${escapeHtml(mode)}</div>
		<div class="table-secondary">${escapeHtml(`${scope} · ${upgradeMode}${timeout}`)}</div>
	`;
}

function renderPolicySchedule(policy) {
    const noRunCount = Array.isArray(policy.policy_blackouts) ? policy.policy_blackouts.length : 0;
    const noRunText = noRunCount
        ? `${pluralize(noRunCount, "policy no-run window", "policy no-run windows")}`
        : "No policy no-run windows";
    const timezoneText = scheduledPolicyView().timezone
        ? `App timezone: ${scheduledPolicyView().timezone}`
        : "";
    const detailText = [noRunText, timezoneText].filter(Boolean).join(" · ");
    return `
        <div>${escapeHtml(formatCadence(policy))}</div>
        <div class="table-secondary">${escapeHtml(detailText)}</div>
    `;
}

function renderMatchedServers(policy) {
    const matchedServers = Array.isArray(policy.matched_servers) ? policy.matched_servers : [];
    if (!matchedServers.length) {
        const emptyMessage = policy && policy.enabled === false
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
    if (String(policy?.target_tag || "").trim()) bits.push(`tag ${policy.target_tag}`);
    if (Array.isArray(policy?.include_tags) && policy.include_tags.length) bits.push(`include ${policy.include_tags.join(", ")}`);
    if (Array.isArray(policy?.exclude_tags) && policy.exclude_tags.length) bits.push(`exclude ${policy.exclude_tags.join(", ")}`);
    if (Array.isArray(policy?.target_servers) && policy.target_servers.length) bits.push(`servers ${policy.target_servers.join(", ")}`);
    return bits.length ? bits.join(" / ") : "No targets";
}

function safeRunStatusClassToken(status) {
    const normalized = String(status || "unknown").toLowerCase().replace(/[^a-z0-9_-]/g, "-");
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
    "complete"
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
    return formatted.secondary ? `${formatted.primary} (${formatted.secondary})` : formatted.primary;
}

function prettyJobJSON(raw) {
    const text = String(raw || "").trim();
    if (!text) return "{}";
    try {
        return JSON.stringify(JSON.parse(text), null, 2);
    } catch (_) {
        return text;
    }
}

function renderJobPhaseTimeline(job) {
    const container = document.getElementById("job-detail-phases");
    if (!container) return;
    const currentPhase = String(job?.phase || "").trim();
    const phases = jobPhaseOrder.includes(currentPhase) ? jobPhaseOrder : [...jobPhaseOrder, currentPhase].filter(Boolean);
    const currentIndex = phases.indexOf(currentPhase);
    container.innerHTML = "";
    phases.forEach((phase, index) => {
        const item = document.createElement("span");
        item.className = "job-phase-step";
        if (currentIndex >= 0 && index < currentIndex) item.classList.add("is-complete");
        if (phase === currentPhase) item.classList.add("is-current");
        item.textContent = formatJobPhaseLabel(phase);
        container.appendChild(item);
    });
}

function renderJobDetail(job, reportURL) {
    scheduledPolicyAdministration.dispatch({ type: "jobReceived", job, data: job });
    document.getElementById("job-detail-title").textContent = `Job ${job.id || ""}`;
    document.getElementById("job-detail-status").innerHTML = `<span class="status-chip status-${safeRunStatusClassToken(job.status)}">${escapeHtml(job.status || "unknown")}</span>`;
    document.getElementById("job-detail-phase").textContent = formatJobPhaseLabel(job.phase);
    document.getElementById("job-detail-kind").textContent = job.kind || "-";
    document.getElementById("job-detail-server").textContent = job.server_name || "-";
    document.getElementById("job-detail-actor").textContent = job.actor || "-";
    document.getElementById("job-detail-client-ip").textContent = job.client_ip || "-";
    document.getElementById("job-detail-created").textContent = formatJobTimestamp(job.created_at);
    document.getElementById("job-detail-updated").textContent = formatJobTimestamp(job.updated_at);
    document.getElementById("job-detail-started").textContent = formatJobTimestamp(job.started_at);
    document.getElementById("job-detail-finished").textContent = formatJobTimestamp(job.finished_at);
    document.getElementById("job-detail-summary").textContent = job.summary || "-";
    document.getElementById("job-detail-retry").textContent = prettyJobJSON(job.retry_policy_json);
    document.getElementById("job-detail-meta").textContent = prettyJobJSON(job.meta_json);
    document.getElementById("job-detail-logs").textContent = job.logs_text || "";
    document.getElementById("job-detail-report").href = reportURL || `/api/reports/jobs/${encodeURIComponent(job.id || "")}`;
    renderJobPhaseTimeline(job);
}

function closeJobDetailModal() {
    const modal = document.getElementById("job-detail-modal");
    if (!modal) return;
    modal.classList.remove("active");
    scheduledPolicyAdministration.dispatch({ type: "jobClosed" });
}

async function openJobDetail(jobID) {
    const cleanID = String(jobID || "").trim();
    if (!cleanID) return;
    const request = scheduledPolicyAdministration.dispatch({ type: "jobSelected", jobID: cleanID })
        .find((effect) => effect.type === "fetchSnapshot");
    if (!request) return;
    try {
        const res = await fetch(`/api/jobs/${encodeURIComponent(cleanID)}`);
        if (!res.ok) {
            window.notifyApp(await parseErrorResponse(res, "Failed to load job details."));
            return;
        }
        const data = await res.json().catch(() => ({}));
        if (!data.job) {
            window.notifyApp("Job details were not returned.");
            return;
        }
        scheduledPolicyAdministration.dispatch({ type: "jobReceived", requestId: request.requestId, job: data.job, data: data.job });
        renderJobDetail(scheduledPolicyView().selectedJob, data.report_url);
        document.getElementById("job-detail-modal").classList.add("active");
        document.getElementById("job-detail-close").focus({ preventScroll: true });
    } catch (err) {
        scheduledPolicyAdministration.dispatch({ type: "snapshotFailed", stream: "job", requestId: request.requestId, error: err.message || "Failed to load job details." });
        console.error("Failed to load job details:", err);
        window.notifyApp("Failed to load job details.");
    }
}

async function copyJobDetailText(kind) {
    const job = scheduledPolicyView().selectedJob;
    if (!job) return;
    const text = kind === "logs"
        ? (job.logs_text || "")
        : `Job ${job.id}\nStatus: ${job.status || "unknown"}\nPhase: ${job.phase || "unknown"}\nSummary: ${job.summary || ""}`;
    if (!String(text || "").trim()) {
        window.notifyApp("Nothing to copy.");
        return;
    }
    try {
        await navigator.clipboard.writeText(text);
        window.notifyApp(kind === "logs" ? "Job logs copied." : "Job summary copied.");
    } catch (_) {
        window.notifyApp("Failed to copy. Select the text and copy it manually.");
    }
}

function resetPolicyForm(options = {}) {
    if (options.updateStore !== false) scheduledPolicyAdministration.dispatch({ type: "editorReset" });
    document.getElementById("policy-id").value = "";
    document.getElementById("policy-name").value = "";
    document.getElementById("policy-target-tag").value = "";
    document.getElementById("policy-include-tags").value = "";
    document.getElementById("policy-exclude-tags").value = "";
    document.getElementById("policy-target-servers").value = "";
    document.getElementById("policy-time-local").value = "02:00";
	document.getElementById("policy-execution-mode").value = "scan_only";
	document.getElementById("policy-package-scope").value = "security";
	document.getElementById("policy-upgrade-mode").value = "standard";
	document.getElementById("policy-cadence-kind").value = "daily";
    document.getElementById("policy-approval-timeout").value = "720";
    document.getElementById("policy-enabled").checked = true;
    clearPolicyFieldErrors();
    setPolicyFeedback("", "");
    setPolicyEditorModeLabel("Create new policy");
    setPolicyWeekdays([]);
    if (options.updateStore !== false) setBlackoutEditorRows("policy", []);
    else renderBlackoutEditor("policy");
    setBlackoutJsonStatus("policy", "");
    refreshPolicyFormVisibility();
    updatePolicySummary();
    schedulePolicyPreview();
    renderPolicyDraftState();
}

function applyPolicyToForm(policy, options = {}) {
    if (options.updateStore !== false) scheduledPolicyAdministration.dispatch({ type: "editorLoaded", policy });
    document.getElementById("policy-id").value = String(policy.id || "");
    document.getElementById("policy-name").value = policy.name || "";
    document.getElementById("policy-target-tag").value = policy.target_tag || "";
    document.getElementById("policy-include-tags").value = (policy.include_tags || []).join(", ");
    document.getElementById("policy-exclude-tags").value = (policy.exclude_tags || []).join(", ");
    document.getElementById("policy-target-servers").value = (policy.target_servers || []).join(", ");
    document.getElementById("policy-time-local").value = policy.time_local || "02:00";
	document.getElementById("policy-execution-mode").value = policy.execution_mode || "scan_only";
	document.getElementById("policy-package-scope").value = policy.package_scope || "security";
	document.getElementById("policy-upgrade-mode").value = policy.upgrade_mode || "standard";
	document.getElementById("policy-cadence-kind").value = policy.cadence_kind || "daily";
    document.getElementById("policy-approval-timeout").value = policy.approval_timeout_minutes || 720;
    document.getElementById("policy-enabled").checked = !!policy.enabled;
    clearPolicyFieldErrors();
    setPolicyFeedback("", "");
    setPolicyWeekdays(policy.weekdays || []);
    if (options.updateStore !== false) setBlackoutEditorRows("policy", policy.policy_blackouts || []);
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
        applyPolicyToForm({ ...draft, policy_blackouts: scheduledPolicyView().editor.policyBlackouts }, { updateStore: false });
    } else {
        resetPolicyForm({ updateStore: false });
    }
    document.getElementById("update-policy-form")?.scrollIntoView({ behavior: "smooth", block: "start" });
}

async function requestPolicyReplacement(replacement, trigger) {
    const effects = scheduledPolicyAdministration.dispatch({ type: "editorReplacementRequested", replacement });
    const confirmation = effects.find(effect => effect.type === "confirmEditorReplacement");
    if (confirmation) {
        trigger?.focus({ preventScroll: true });
        const editor = scheduledPolicyView().editor;
        const confirmed = await window.confirmAction("Review the unsaved browser draft before replacing it.", {
            danger: true,
            title: "Discard unsaved policy changes",
            operation: replacement?.type === "load" ? "Open another policy" : "Start a new policy draft",
            resources: editor.draft.name ? `Unsaved draft "${editor.draft.name}"` : "Current unsaved policy draft",
            consequences: "All unsaved policy fields and no-run window edits in this browser are discarded.",
            reversibility: "Not reversible unless the same values are entered again.",
            authentication: "No additional authentication is required because no accepted server data is changed.",
            confirmLabel: "Discard Changes"
        });
        if (!confirmed) return;
        scheduledPolicyAdministration.dispatch({ type: "editorReplacementConfirmed", replacement: confirmation.replacement });
    }
    applyPolicyReplacementToDOM(replacement);
}

function discardPolicyDraft() {
    scheduledPolicyAdministration.dispatch({ type: "editorDiscardRequested" });
    const editor = scheduledPolicyView().editor;
    const replacement = editor.draft.id
        ? { type: "load", policy: { ...editor.draft, policy_blackouts: editor.policyBlackouts } }
        : { type: "reset" };
    applyPolicyReplacementToDOM(replacement);
}

function discardGlobalSettingsDraft() {
    scheduledPolicyAdministration.dispatch({ type: "globalSettingsDiscardRequested" });
    renderBlackoutEditor("global");
    setScheduledSettingsFeedback("", "");
    renderGlobalSettingsDraftState();
}

function renderScheduledPolicies() {
    const tbody = document.querySelector("#scheduled-policy-table tbody");
    if (!tbody) return;
    tbody.innerHTML = "";
    const policies = scheduledPolicyView().policies;
    if (!policies.length) {
        const row = document.createElement("tr");
        row.innerHTML = '<td colspan="5" class="subtle">No scheduled update policies yet.</td>';
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
    const select = document.getElementById("maintenance-calendar-policy");
    if (!select) return;
    const current = scheduledPolicyView().selectedCalendarPolicyID || select.value;
    select.innerHTML = '<option value="">All policies</option>';
    scheduledPolicyView().policies.forEach((policy) => {
        const option = document.createElement("option");
        option.value = String(policy.id || "");
        option.textContent = policy.name || `Policy ${policy.id}`;
        select.appendChild(option);
    });
    if (current && Array.from(select.options).some((option) => option.value === current)) {
        select.value = current;
    }
}

function formatCalendarDate(day) {
    const date = String(day?.date || "");
    const weekday = formatWeekdayLabel(day?.weekday || "");
    return [weekday, date].filter(Boolean).join(" ");
}

function renderCalendarSlot(slot) {
    const serverCount = Array.isArray(slot?.matched_servers) ? slot.matched_servers.length : 0;
    const details = [
        slot?.timezone_offset || "",
        humanizeExecutionMode(slot?.execution_mode),
        humanizePackageScope(slot?.package_scope),
        pluralize(serverCount, "server", "servers")
    ].filter(Boolean).join(" · ");
    return `
        <span class="calendar-chip calendar-chip-allowed" title="${escapeHtml(details)}">
            ${escapeHtml(`Allowed ${slot?.time_local || "--:--"} ${slot?.timezone_offset || ""}`)}
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

function renderMaintenanceCalendar(calendar) {
    const container = document.getElementById("maintenance-calendar-list");
    const status = document.getElementById("maintenance-calendar-status");
    if (!container) return;
    const policies = Array.isArray(calendar?.policies) ? calendar.policies : [];
    if (status) {
        const range = calendar?.start_date && calendar?.end_date ? `${calendar.start_date} to ${calendar.end_date}` : "";
        const tz = calendar?.timezone || scheduledPolicyView().timezone || "UTC";
        status.textContent = range ? `${range} · ${tz}` : `Calendar timezone: ${tz}`;
    }
    if (!policies.length) {
        container.innerHTML = '<div class="empty-editor-state subtle">No scheduled policies to show.</div>';
        return;
    }
    container.innerHTML = policies.map((policy) => {
        const days = Array.isArray(policy.days) ? policy.days : [];
        const matchedCount = Array.isArray(policy.matched_servers) ? policy.matched_servers.length : 0;
        return `
            <div class="calendar-policy">
                <div class="calendar-policy-head">
                    <div>
                        <strong>${escapeHtml(policy.name || "")}</strong>
                        <div class="table-secondary">${escapeHtml(`${formatCadence(policy)} · ${pluralize(matchedCount, "matched server", "matched servers")}`)}</div>
                    </div>
                    <span class="pill ${policy.enabled ? "" : "pill-muted"}">${policy.enabled ? "Enabled" : "Disabled"}</span>
                </div>
                <div class="calendar-day-grid">
                    ${days.map((day) => {
                        const slots = Array.isArray(day.allowed_slots) ? day.allowed_slots : [];
                        const windows = Array.isArray(day.blocked_windows) ? day.blocked_windows : [];
                        const reasons = Array.isArray(day.blocked_reasons) ? day.blocked_reasons : [];
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
                                ${reasons.length ? `<div class="table-secondary">${escapeHtml(`Blocked: ${reasons.join(", ")}`)}</div>` : ""}
                            </div>
                        `;
                    }).join("")}
                </div>
            </div>
        `;
    }).join("");
}

function formatRunDuration(milliseconds) {
    const totalSeconds = Math.max(0, Math.round(Number(milliseconds) / 1000));
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
    const resolvedTimezone = window.getAppTimezoneResolved ? window.getAppTimezoneResolved() : "";
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
    const resultSummary = document.getElementById("scheduled-runs-result-summary");
    const pageInfo = document.getElementById("scheduled-runs-page-info");
    const previous = document.getElementById("scheduled-runs-prev");
    const next = document.getElementById("scheduled-runs-next");
    if (resultSummary) resultSummary.textContent = `${total} matching run${total === 1 ? "" : "s"}.`;
    if (pageInfo) pageInfo.textContent = totalPages ? `Page ${page} of ${totalPages}` : "Page 1 of 1";
    if (previous) previous.disabled = page <= 1;
    if (next) next.disabled = totalPages === 0 || page >= totalPages;
    tbody.innerHTML = "";
    if (!runs.length) {
        const row = document.createElement("tr");
        const hasFilters = [query.policy, query.server, query.outcome, query.from, query.to].some(Boolean);
        row.innerHTML = `<td colspan="6" class="subtle" data-label="Result">${hasFilters ? "No scheduled runs match these filters." : "No scheduled runs recorded yet."}</td>`;
        tbody.appendChild(row);
        renderAdminWorkspace();
        return;
    }
    runs.forEach((run) => {
        const row = document.createElement("tr");
        const outcome = run.terminal_outcome || run.status || "unknown";
        const statusToken = safeRunStatusClassToken(outcome);
        const scheduled = scheduledRunTimestamp(run, "scheduled_for_utc", "scheduled_for_display") || { primary: "-", secondary: "", title: "" };
        const started = scheduledRunTimestamp(run, "started_at", "started_at_display");
        const finished = scheduledRunTimestamp(run, "finished_at", "finished_at_display");
        const duration = run.duration_ms === undefined || run.duration_ms === null ? "Not finished" : formatRunDuration(run.duration_ms);
        const jobDetailURL = run.job_detail_url || (run.job_id ? `/api/jobs/${encodeURIComponent(run.job_id)}` : "");
        const reportURL = run.report_url || (run.job_id ? `/api/reports/jobs/${encodeURIComponent(run.job_id)}` : "");
        const auditURL = run.audit_url || "";
        row.innerHTML = `
            <td data-label="Scheduled" title="${escapeHtml(scheduled.title || "")}">
                <div>${escapeHtml(scheduled.primary || "")}</div>
                ${scheduled.secondary ? `<div class="table-secondary">${escapeHtml(scheduled.secondary)}</div>` : ""}
            </td>
            <td data-label="Policy &amp; server">
                <strong>${escapeHtml(run.policy_name || "-")}</strong>
                <div class="table-secondary">${escapeHtml(run.server_name || "-")}</div>
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
    const detailButton = event.target.closest("[data-action='job-detail']");
    if (!detailButton) return;
    openJobDetail(detailButton.dataset.jobId);
}

async function fetchScheduledPolicies(request) {
    request = request || scheduledPolicyAdministration.dispatch({ type: "snapshotRequested", stream: "policies" })
        .find((effect) => effect.type === "fetchSnapshot");
    if (!request) return;
    const res = await fetch("/api/update-policies");
    if (!res.ok) {
        throw new Error(await parseErrorResponse(res, "Failed to load scheduled policies."));
    }
    const data = await res.json().catch(() => ({}));
    const followUp = scheduledPolicyAdministration.dispatch({ type: "snapshotReceived", stream: "policies", requestId: request.requestId, receivedAt: new Date().toISOString(), data });
    if (data.timezone) {
        applyScheduledTimezone(data);
    }
    renderScheduledPolicies();
    renderMaintenanceCalendarFilter();
    await runScheduledEffects(followUp);
}

async function fetchScheduledSettings(request) {
    request = request || scheduledPolicyAdministration.dispatch({ type: "snapshotRequested", stream: "settings" })
        .find((effect) => effect.type === "fetchSnapshot");
    if (!request) return;
    const res = await fetch("/api/update-policies/settings");
    if (!res.ok) {
        throw new Error(await parseErrorResponse(res, "Failed to load scheduled update settings."));
    }
    const data = await res.json().catch(() => ({}));
    const followUp = scheduledPolicyAdministration.dispatch({ type: "snapshotReceived", stream: "settings", requestId: request.requestId, receivedAt: new Date().toISOString(), data });
    applyScheduledTimezone(data.timezone ? data : scheduledPolicyView().timezone || "UTC");
    setBlackoutEditorRows("global", data.global_blackouts || []);
    setBlackoutJsonStatus("global", "");
    await runScheduledEffects(followUp);
}

async function fetchScheduledRuns(request) {
    request = request || scheduledPolicyAdministration.dispatch({ type: "snapshotRequested", stream: "runs" })
        .find((effect) => effect.type === "fetchSnapshot");
    if (!request) return;
    const query = request.query || scheduledPolicyView().runHistory?.query || {};
    const params = new URLSearchParams({
        page: String(query.page || 1),
        page_size: String(query.pageSize || 25)
    });
    if (query.policy) params.set("policy", query.policy);
    if (query.server) params.set("server", query.server);
    if (query.outcome) params.set("outcome", query.outcome);
    if (query.from) params.set("from", query.from);
    if (query.to) params.set("to", query.to);
    const res = await fetch(`/api/update-policies/runs?${params.toString()}`);
    if (!res.ok) {
        throw new Error(await parseErrorResponse(res, "Failed to load scheduled runs."));
    }
    const data = await res.json().catch(() => ({}));
    const followUp = scheduledPolicyAdministration.dispatch({ type: "snapshotReceived", stream: "runs", requestId: request.requestId, receivedAt: new Date().toISOString(), data });
    const accepted = followUp.some(effect => effect.type === "render" && effect.area === "runs");
    if (!accepted) return;
    if (data.timezone) {
        applyScheduledTimezone(data);
    }
    renderScheduledRuns();
    await runScheduledEffects(followUp);
}

async function fetchMaintenanceCalendar(request) {
    const policyID = String(request?.policyID || scheduledPolicyView().selectedCalendarPolicyID || document.getElementById("maintenance-calendar-policy")?.value || "").trim();
    request = request || scheduledPolicyAdministration.dispatch({ type: "snapshotRequested", stream: "calendar", payload: { policyID } })
        .find((effect) => effect.type === "fetchSnapshot");
    if (!request) return;
    const params = new URLSearchParams({ days: "14" });
    if (policyID) params.set("policy_id", policyID);
    const res = await fetch(`/api/update-policies/calendar?${params.toString()}`);
    if (!res.ok) {
        throw new Error(await parseErrorResponse(res, "Failed to load maintenance window calendar."));
    }
    const data = await res.json().catch(() => ({}));
    const followUp = scheduledPolicyAdministration.dispatch({ type: "snapshotReceived", stream: "calendar", requestId: request.requestId, receivedAt: new Date().toISOString(), data });
    if (data.timezone) {
        applyScheduledTimezone(data);
    }
    renderMaintenanceCalendar(scheduledPolicyView().calendar);
    await runScheduledEffects(followUp);
}

async function reloadMaintenanceCalendar() {
    const status = document.getElementById("maintenance-calendar-status");
    try {
        if (status) status.textContent = "Loading calendar...";
        await fetchMaintenanceCalendar();
    } catch (err) {
        const requestId = scheduledPolicyView().snapshots.calendar.inFlight;
        if (requestId) scheduledPolicyAdministration.dispatch({ type: "snapshotFailed", stream: "calendar", requestId, error: err.message || "Failed to load maintenance window calendar." });
        console.error("Failed to load maintenance window calendar:", err);
        if (status) status.textContent = err.message || "Failed to load maintenance window calendar.";
    }
}

async function runScheduledEffects(effects) {
    let firstError = null;
    for (const effect of effects) {
        if (effect.type !== "fetchSnapshot") continue;
        try {
            if (effect.stream === "policies") await fetchScheduledPolicies(effect);
            if (effect.stream === "settings") await fetchScheduledSettings(effect);
            if (effect.stream === "runs") await fetchScheduledRuns(effect);
            if (effect.stream === "calendar") await fetchMaintenanceCalendar(effect);
        } catch (err) {
            const failureEffects = scheduledPolicyAdministration.dispatch({ type: "snapshotFailed", stream: effect.stream, requestId: effect.requestId, error: err.message || "Failed to refresh scheduled policy data." });
            if (!failureEffects.length) continue;
            renderAdminWorkspace();
            firstError = firstError || err;
        }
    }
    if (firstError) throw firstError;
}

async function refreshScheduledUpdateViews() {
    try {
        await fetchAppTimezoneSettings(true);
        await runScheduledEffects(["policies", "settings"].flatMap((stream) => (
            scheduledPolicyAdministration.dispatch({ type: "snapshotRequested", stream })
        )));
    } catch (err) {
        console.error("Failed to refresh scheduled update views:", err);
        setPolicyFeedback("", err.message || "Failed to load scheduled update views.");
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
            target_tag: document.getElementById("policy-target-tag").value,
            include_tags: document.getElementById("policy-include-tags").value,
            exclude_tags: document.getElementById("policy-exclude-tags").value,
            target_servers: document.getElementById("policy-target-servers").value,
            cadence_kind: document.getElementById("policy-cadence-kind").value,
            execution_mode: document.getElementById("policy-execution-mode").value,
            package_scope: document.getElementById("policy-package-scope").value,
            upgrade_mode: document.getElementById("policy-upgrade-mode").value,
            time_local: document.getElementById("policy-time-local").value,
            approval_timeout_minutes: document.getElementById("policy-approval-timeout").value
        }
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
    if (firstInvalidId) {
        if (!silent) document.getElementById(firstInvalidId)?.focus();
    }
    if (!result.ok) throw new Error(result.message || errors.blackouts || "Complete the scheduled policy fields.");
    return result.payload;
}

async function saveScheduledPolicy(event) {
    event.preventDefault();
    let plan;
    try {
        collectPolicyPayload();
        const command = scheduledPolicyAdministration.dispatch({ type: "commandRequested", command: "savePolicy" });
        const execution = command.find((effect) => effect.type === "executeCommand");
        if (!execution) throw new Error(command.find((effect) => effect.type === "commandRejected")?.message || "Scheduled policy action is unavailable.");
        plan = execution.plan;
        const url = plan.command === "updatePolicy" ? `/api/update-policies/${encodeURIComponent(plan.policyID)}` : "/api/update-policies";
        const method = plan.command === "updatePolicy" ? "PUT" : "POST";
        const saveBtn = document.getElementById("policy-save-btn");
        if (saveBtn) saveBtn.disabled = true;
        const res = await fetch(url, {
            method,
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(plan.payload)
        });
        if (!res.ok) {
            throw new Error(await parseErrorResponse(res, "Failed to save scheduled policy."));
        }
        const successMessage = plan.command === "updatePolicy" ? "Policy updated." : "Policy created.";
        resetPolicyForm();
        await runScheduledEffects(scheduledPolicyAdministration.dispatch({ type: "commandCompleted", plan, message: successMessage }));
        setPolicyFeedback(successMessage, "");
    } catch (err) {
        if (plan) scheduledPolicyAdministration.dispatch({ type: "commandFailed", plan, message: err.message || "Failed to save scheduled policy." });
        setPolicyFeedback("", err.message || "Failed to save scheduled policy.");
    } finally {
        renderPolicyDraftState();
    }
}

async function saveScheduledSettings() {
    let plan;
    try {
        setScheduledSettingsFeedback("", "");
        const command = scheduledPolicyAdministration.dispatch({ type: "commandRequested", command: "saveGlobalSettings" });
        const execution = command.find((effect) => effect.type === "executeCommand");
        if (!execution) throw new Error(command.find((effect) => effect.type === "commandRejected")?.message || "Global settings are unavailable.");
        plan = execution.plan;
        const button = document.getElementById("scheduled-settings-save");
        if (button) button.disabled = true;
        const res = await fetch("/api/update-policies/settings", {
            method: "PUT",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(plan.payload)
        });
        if (!res.ok) {
            throw new Error(await parseErrorResponse(res, "Failed to save global no-run windows."));
        }
        const data = await res.json().catch(() => ({}));
        applyScheduledTimezone(data.timezone ? data : scheduledPolicyView().timezone || "UTC");
        if (!String(data?.resolved_timezone ?? data?.resolvedTimezone ?? "").trim()) {
            try {
                await fetchScheduledRuns();
            } catch (refreshErr) {
                const requestId = scheduledPolicyView().snapshots.runs.inFlight;
                if (requestId) scheduledPolicyAdministration.dispatch({ type: "snapshotFailed", stream: "runs", requestId, error: refreshErr.message || "Failed to load scheduled runs." });
                console.error("Failed to refresh scheduled runs after settings save:", refreshErr);
            }
        }
        const successMessage = "Global no-run windows saved.";
        await runScheduledEffects(scheduledPolicyAdministration.dispatch({ type: "commandCompleted", plan, message: successMessage }));
        setScheduledSettingsFeedback(successMessage, "");
    } catch (err) {
        if (plan) scheduledPolicyAdministration.dispatch({ type: "commandFailed", plan, message: err.message || "Failed to save global no-run windows." });
        setScheduledSettingsFeedback("", err.message || "Failed to save global no-run windows.");
    } finally {
        renderGlobalSettingsDraftState();
    }
}

async function deleteScheduledPolicy(id) {
    const plan = scheduledPolicyAdministration.planCommand("deletePolicy", id);
    const policy = plan.policy;
    const required = policy?.name || String(id);
    if (!plan.enabled) {
        setPolicyFeedback("", plan.reason || "Scheduled policy action is unavailable.");
        return;
    }
    if (!(await window.confirmTypedAction("Type the policy name to confirm permanent deletion.", required, {
        title: "Delete scheduled update policy",
        operation: "Permanently delete one recurring update policy",
        resources: `${required} (policy ${id})`,
        consequences: "Future runs from this policy stop; existing job and audit records remain.",
        reversibility: "Not reversible. Recreating the policy requires entering its configuration again.",
        authentication: "Typed policy-name confirmation is required; the current Admin session authorizes the request.",
        confirmLabel: "Delete Policy"
    }))) {
        return;
    }
    let activePlan;
    try {
        const command = scheduledPolicyAdministration.dispatch({ type: "commandRequested", command: "deletePolicy", policyID: id });
        const execution = command.find((effect) => effect.type === "executeCommand");
        if (!execution) throw new Error(command.find((effect) => effect.type === "commandRejected")?.message || "Scheduled policy action is unavailable.");
        activePlan = execution.plan;
        renderAdminDestructiveControls();
        const res = await fetch(`/api/update-policies/${encodeURIComponent(activePlan.policyID)}`, { method: "DELETE" });
        if (!res.ok) {
            throw new Error(await parseErrorResponse(res, "Failed to delete scheduled policy."));
        }
        if (document.getElementById("policy-id").value === String(id)) {
            resetPolicyForm();
        }
        setPolicyFeedback("Policy deleted.", "");
        await runScheduledEffects(scheduledPolicyAdministration.dispatch({ type: "commandCompleted", plan: activePlan, message: "Policy deleted." }));
    } catch (err) {
        if (activePlan) scheduledPolicyAdministration.dispatch({ type: "commandFailed", plan: activePlan, message: err?.message || "Failed to delete scheduled policy." });
        setPolicyFeedback("", err?.message || "Failed to delete scheduled policy.");
    } finally {
        renderAdminDestructiveControls();
    }
}

function handleScheduledPolicyTableClick(event) {
    const button = event.target.closest("button[data-action]");
    if (!button) return;
    const id = String(button.dataset.id || "").trim();
    const policy = scheduledPolicyView().policies.find((item) => String(item.id) === id);
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
    scheduledPolicyAdministration.dispatch({ type: "blackoutRowChanged", kind, index, field, value });
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
            scheduledPolicyAdministration.dispatch({ type: "blackoutRowRemoved", kind, index });
            renderBlackoutEditor(kind);
        }
        return;
    }
    if (action === "toggle-day" && index >= 0) {
        scheduledPolicyAdministration.dispatch({ type: "blackoutWeekdayToggled", kind, index, day: button.dataset.day });
        const isActive = scheduledPolicyRows(kind)[index]?.weekdays.includes(normalizeWeekdayToken(button.dataset.day));
        button.classList.toggle("active", isActive);
        button.setAttribute("aria-pressed", isActive ? "true" : "false");
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
    const textarea = document.getElementById(kind === "global" ? "scheduled-global-blackouts-json" : "policy-blackouts-json");
    if (!textarea) return;
    const effect = scheduledPolicyAdministration.dispatch({ type: "blackoutJSONApplied", kind, label, raw: textarea.value })[0];
    if (effect?.type === "blackoutJSONAccepted") {
        renderBlackoutEditor(kind);
        setBlackoutJsonStatus(kind, effect.message, false);
        if (kind === "global") {
            setScheduledSettingsFeedback("", "");
            renderGlobalSettingsDraftState();
        }
    } else {
        setBlackoutJsonStatus(kind, effect?.message || `Failed to apply ${label.toLowerCase()}.`, true);
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
        "policy-approval-timeout"
    ];
    summaryFields.forEach((fieldId) => {
        document.getElementById(fieldId)?.addEventListener("input", () => {
            try { collectPolicyPayload({ silent: true }); } catch (_) {}
            if (fieldId === "policy-name") setPolicyFieldInvalid("policy-name", false);
            if (fieldId === "policy-target-tag") setPolicyFieldInvalid("policy-target-tag", false);
            refreshPolicyFormVisibility();
            updatePolicySummary();
            schedulePolicyPreview();
        });
        document.getElementById(fieldId)?.addEventListener("change", () => {
            try { collectPolicyPayload({ silent: true }); } catch (_) {}
            if (fieldId === "policy-name") setPolicyFieldInvalid("policy-name", false);
            if (fieldId === "policy-target-tag") setPolicyFieldInvalid("policy-target-tag", false);
            refreshPolicyFormVisibility();
            updatePolicySummary();
            schedulePolicyPreview();
        });
    });

    document.getElementById("policy-weekdays-picker")?.addEventListener("click", (event) => {
        const button = event.target.closest("[data-weekday]");
        if (!button) return;
        togglePolicyWeekday(button.dataset.weekday);
    });

    document.getElementById("policy-weekdays-clear")?.addEventListener("click", () => {
        setPolicyWeekdays([]);
    });
}

document.addEventListener("change", (event) => {
    if (event.target && event.target.id === "backup-restore-file") {
        adminPageInteraction.dispatch({ type: "backupFileSelected", file: event.target.files?.[0] || null });
        updateFileLabel(event.target, "Choose backup file");
        renderBackupRestoreReview();
    }
});

document.getElementById("logout-btn").addEventListener("click", () => window.logout());
document.getElementById("metrics-token-generate").addEventListener("click", () => rotateMetricsToken(false));
document.getElementById("metrics-token-rotate").addEventListener("click", () => rotateMetricsToken(true));
document.getElementById("metrics-token-disable").addEventListener("click", disableMetricsToken);
document.getElementById("metrics-token-copy").addEventListener("click", copyMetricsToken);
document.getElementById("backup-export-btn").addEventListener("click", exportBackup);
document.getElementById("backup-verify-btn").addEventListener("click", verifyBackup);
document.getElementById("backup-restore-btn").addEventListener("click", restoreBackup);
document.getElementById("backup-restore-passphrase").addEventListener("input", (event) => {
    adminPageInteraction.dispatch({ type: "backupPassphraseChanged", valid: event.target.value.length >= 12 });
    renderBackupRestoreReview();
});
document.getElementById("app-timezone-save").addEventListener("click", saveAppTimezoneSettings);
document.getElementById("app-timezone-discard").addEventListener("click", discardTimezoneDraft);
document.getElementById("app-timezone-input").addEventListener("input", (event) => {
    adminPageInteraction.dispatch({ type: "timezoneDraftChanged", timezone: event.target.value });
    setAppTimezoneFeedback("", "");
    renderTimezoneSaveState();
});
document.getElementById("notification-save").addEventListener("click", saveNotificationSettings);
document.getElementById("notification-discard").addEventListener("click", discardNotificationDraft);
document.getElementById("notification-test").addEventListener("click", sendNotificationTest);
document.getElementById("notification-webhook-url").addEventListener("input", syncNotificationDraftFromDOM);
document.getElementById("notification-enabled").addEventListener("change", syncNotificationDraftFromDOM);
document.querySelectorAll("[data-notification-event]").forEach(input => input.addEventListener("change", syncNotificationDraftFromDOM));
document.getElementById("auth-password-save").addEventListener("click", changeAdminPassword);
["auth-current-password", "auth-new-password", "auth-confirm-password"].forEach(id => {
    const input = document.getElementById(id);
    input.addEventListener("input", () => {
        setAuthPasswordFeedback("", "");
        renderAuthPasswordControls();
    });
    input.addEventListener("keydown", updatePasswordCapsLockWarning);
    input.addEventListener("keyup", updatePasswordCapsLockWarning);
});
document.querySelectorAll("[data-password-toggle]").forEach(button => button.addEventListener("click", togglePasswordVisibility));
document.getElementById("auth-password-invalidate-others").addEventListener("change", renderAuthPasswordControls);
document.getElementById("auth-sessions-clear").addEventListener("click", clearAuthSessions);
document.getElementById("auth-sessions-clear-others").addEventListener("click", clearOtherAuthSessions);
document.getElementById("auth-session-inventory").addEventListener("click", handleAuthSessionListClick);
document.getElementById("auth-sessions-show-all").addEventListener("click", () => {
    const expanded = !adminPageView().account.otherSessionsExpanded;
    adminPageInteraction.dispatch({ type: "sessionListExpandedChanged", expanded });
    renderAuthSessions();
});
document.getElementById("session-ip-reveal-form").addEventListener("submit", submitSessionIPReveal);
document.getElementById("session-ip-reveal-cancel").addEventListener("click", closeSessionIPRevealModal);
document.getElementById("session-ip-reveal-modal").addEventListener("click", (event) => {
    if (event.target?.id === "session-ip-reveal-modal") closeSessionIPRevealModal();
});
document.addEventListener("visibilitychange", () => {
    if (!document.hidden) return;
    hideTemporarySessionIPReveal();
    const modal = document.getElementById("session-ip-reveal-modal");
    if (modal?.classList.contains("active")) closeSessionIPRevealModal({ restoreFocus: false });
});
window.addEventListener("pagehide", hideTemporarySessionIPReveal);
document.getElementById("update-policy-form").addEventListener("submit", saveScheduledPolicy);
document.getElementById("policy-new-btn").addEventListener("click", event => requestPolicyReplacement({ type: "reset" }, event.currentTarget));
document.getElementById("policy-discard-btn").addEventListener("click", discardPolicyDraft);
document.getElementById("scheduled-settings-save").addEventListener("click", saveScheduledSettings);
document.getElementById("scheduled-settings-discard").addEventListener("click", discardGlobalSettingsDraft);
document.getElementById("maintenance-calendar-refresh").addEventListener("click", reloadMaintenanceCalendar);
document.getElementById("maintenance-calendar-policy").addEventListener("change", (event) => {
    scheduledPolicyAdministration.dispatch({ type: "calendarPolicySelected", policyID: event.target.value });
    reloadMaintenanceCalendar();
});
document.querySelector("#scheduled-policy-table tbody").addEventListener("click", handleScheduledPolicyTableClick);
document.querySelector("#scheduled-runs-table tbody").addEventListener("click", handleScheduledRunsTableClick);
document.getElementById("scheduled-runs-filter").addEventListener("submit", async (event) => {
    event.preventDefault();
    scheduledPolicyAdministration.dispatch({
        type: "runQueryChanged",
        patch: {
            policy: document.getElementById("scheduled-runs-policy-filter").value,
            server: document.getElementById("scheduled-runs-server-filter").value,
            outcome: document.getElementById("scheduled-runs-outcome-filter").value,
            from: document.getElementById("scheduled-runs-from-filter").value,
            to: document.getElementById("scheduled-runs-to-filter").value
        }
    });
    renderScheduledRuns();
    renderAdminWorkspace();
    await runScheduledEffects(scheduledPolicyAdministration.dispatch({ type: "runQueryApplied" }));
});
document.getElementById("scheduled-runs-reset").addEventListener("click", async () => {
    ["scheduled-runs-policy-filter", "scheduled-runs-server-filter", "scheduled-runs-from-filter", "scheduled-runs-to-filter"].forEach((id) => {
        document.getElementById(id).value = "";
    });
    document.getElementById("scheduled-runs-outcome-filter").value = "";
    renderAdminWorkspace();
    await runScheduledEffects(scheduledPolicyAdministration.dispatch({ type: "runQueryReset" }));
});
document.getElementById("scheduled-runs-prev").addEventListener("click", async () => {
    const page = scheduledPolicyView().runHistory?.page || 1;
    await runScheduledEffects(scheduledPolicyAdministration.dispatch({ type: "runPageRequested", page: page - 1 }));
});
document.getElementById("scheduled-runs-next").addEventListener("click", async () => {
    const page = scheduledPolicyView().runHistory?.page || 1;
    await runScheduledEffects(scheduledPolicyAdministration.dispatch({ type: "runPageRequested", page: page + 1 }));
});
document.getElementById("policy-blackout-add").addEventListener("click", () => addBlackoutRow("policy"));
document.getElementById("global-blackout-add").addEventListener("click", () => addBlackoutRow("global"));
document.getElementById("policy-blackouts-json-apply").addEventListener("click", () => applyBlackoutJson("policy", "Policy no-run windows"));
document.getElementById("scheduled-global-blackouts-json-apply").addEventListener("click", () => applyBlackoutJson("global", "Global no-run windows"));
document.getElementById("policy-blackout-rows").addEventListener("click", handleBlackoutEditorClick);
document.getElementById("global-blackout-rows").addEventListener("click", handleBlackoutEditorClick);
document.getElementById("policy-blackout-rows").addEventListener("input", handleBlackoutEditorInput);
document.getElementById("global-blackout-rows").addEventListener("input", handleBlackoutEditorInput);
document.getElementById("job-detail-close").addEventListener("click", closeJobDetailModal);
document.getElementById("job-detail-copy-summary").addEventListener("click", () => copyJobDetailText("summary"));
document.getElementById("job-detail-copy-logs").addEventListener("click", () => copyJobDetailText("logs"));
document.getElementById("job-detail-modal").addEventListener("click", (event) => {
    if (event.target && event.target.id === "job-detail-modal") {
        closeJobDetailModal();
    }
});
document.addEventListener("keydown", (event) => {
    handleSessionIPRevealModalKeydown(event);
    const modal = document.getElementById("job-detail-modal");
    if (event.key === "Escape" && modal && modal.classList.contains("active")) {
        closeJobDetailModal();
    }
});
window.addEventListener("beforeunload", (event) => {
    if (!adminPageView().hasMeaningfulDirty) return;
    event.preventDefault();
    event.returnValue = "";
});

bindPolicyFormInteractions();
initializeAdminWorkspace();
populateTimezonePicker();
resetPolicyForm();
fetchAuthSessionStatus();
refreshScheduledUpdateViews();
updateFileLabel(document.getElementById("backup-restore-file"), "Choose backup file");
renderBackupRestoreReview();
renderNotificationDraftState();
renderGlobalSettingsDraftState();
