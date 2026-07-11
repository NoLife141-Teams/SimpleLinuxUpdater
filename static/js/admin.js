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

function adminPageView() {
    return adminPageInteraction.getView();
}

function beginAdminCommand(command, payload = {}) {
    return adminPageInteraction.dispatch({ type: "commandRequested", command, payload })
        .find((item) => item.type === "executeCommand")?.plan || null;
}

function finishAdminCommand(plan, data, message, failed = false) {
    if (!plan) return [];
    return adminPageInteraction.dispatch({ type: failed ? "commandFailed" : "commandCompleted", plan, data, message });
}

function beginAdminSnapshot(stream) {
    return adminPageInteraction.dispatch({ type: "snapshotRequested", stream })
        .find((item) => item.type === "fetchSnapshot")?.requestID || null;
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

function timezoneOptionLabel(timezone) {
    if (timezone === "Local") return "Local system timezone";
    if (timezone === "UTC") return "UTC";
    if (/^[+-]\d{2}:\d{2}$/.test(timezone)) return `Fixed UTC offset ${timezone}`;
    return timezone.replace(/_/g, " ");
}

function ensureTimezoneSelectHasValue(value) {
    const select = document.getElementById("app-timezone-input");
    if (!select) return;
    const timezone = String(value || "").trim();
    if (!timezone) return;
    const exists = Array.from(select.options || []).some((option) => option.value === timezone);
    if (exists) return;
    const option = document.createElement("option");
    option.value = timezone;
    option.textContent = `${timezoneOptionLabel(timezone)} (saved)`;
    select.appendChild(option);
}

function populateTimezonePicker() {
    const select = document.getElementById("app-timezone-input");
    if (!select) return;
    const currentValue = select.value || adminPageView().timezone.draft || "";
    const combined = [
        "",
        "Local",
        "UTC",
        ...browserSupportedTimezones(),
        ...ianaTimezoneOptions,
        ...fixedOffsetTimezoneOptions
    ];
    const seen = new Set();
    const options = [];
    combined.forEach((timezone) => {
        const value = String(timezone || "").trim();
        if (seen.has(value)) return;
        seen.add(value);
        options.push(value);
    });
    const priority = new Map([
        ["Local", 0],
        ["UTC", 1],
        ["America/Toronto", 2],
        ["America/New_York", 3],
        ["America/Chicago", 4],
        ["America/Denver", 5],
        ["America/Los_Angeles", 6],
        ["Europe/London", 7],
        ["Europe/Paris", 8]
    ]);
    options.sort((a, b) => {
        if (a === "") return -1;
        if (b === "") return 1;
        const aRank = priority.has(a) ? priority.get(a) : 100;
        const bRank = priority.has(b) ? priority.get(b) : 100;
        if (aRank !== bRank) return aRank - bRank;
        return a.localeCompare(b);
    });
    select.innerHTML = options.map((timezone) => (
        `<option value="${escapeHtml(timezone)}">${escapeHtml(timezone === "" ? "System default timezone" : timezoneOptionLabel(timezone))}</option>`
    )).join("");
    ensureTimezoneSelectHasValue(currentValue);
    select.value = currentValue;
    const note = document.getElementById("app-timezone-picker-note");
    if (note) {
        note.innerHTML = `Pick from ${options.length} IANA/fixed timezone choices, including <code>America/Toronto</code>, <code>Europe/Paris</code>, <code>UTC</code>, or <code>Local</code>.`;
    }
}

function renderScheduledTimezone(payload) {
    const timezoneState = window.setAppTimezoneCache
        ? window.setAppTimezoneCache(payload)
        : { timezone: String(payload || "").trim() || scheduledPolicyView().timezone || "UTC" };
    const timezone = timezoneState.timezone || "UTC";
    scheduledPolicyAdministration.dispatch({ type: "timezoneReceived", timezone });
    const timezoneSelection = adminPageView().timezone.draft;
    const timezoneLabel = document.getElementById("scheduled-timezone");
    if (timezoneLabel) {
        timezoneLabel.textContent = timezone;
    }
    const timezoneInput = document.getElementById("app-timezone-input");
    if (timezoneInput && document.activeElement !== timezoneInput) {
        ensureTimezoneSelectHasValue(timezoneSelection);
        timezoneInput.value = timezoneSelection;
    }
    updatePolicySummary();
    renderScheduledPolicies();
    renderScheduledRuns(scheduledPolicyView().runs);
}

function applyScheduledTimezone(payload, requestID = null) {
    const effects = adminPageInteraction.dispatch({ type: "timezoneSnapshotReceived", requestID, data: payload });
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
}

function applyNotificationSettings(payload, requestID = null) {
    const effects = adminPageInteraction.dispatch({ type: "notificationSnapshotReceived", requestID, data: payload });
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
            return;
        }
        const data = await res.json().catch(() => ({}));
        applyNotificationSettings(data, requestID);
    } catch (err) {
        console.error("Failed to load notification settings:", err);
        adminPageInteraction.dispatch({ type: "snapshotFailed", stream: "notifications", requestID, error: "Failed to load notification settings." });
        setNotificationFeedback("", "Failed to load notification settings.");
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
        if (button) button.disabled = false;
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
        const timezone = input ? input.value.trim() : "";
        adminPageInteraction.dispatch({ type: "timezoneDraftChanged", timezone });
        plan = beginAdminCommand("saveTimezone");
        if (!plan) return;
        if (button) button.disabled = true;
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
        const button = document.getElementById("app-timezone-save");
        if (button) button.disabled = false;
    }
}

function setAuthPasswordFeedback(successMessage, errorMessage) {
    const success = document.getElementById("auth-password-status");
    const error = document.getElementById("auth-password-error");
    if (success) success.textContent = successMessage || "";
    if (error) error.textContent = errorMessage || "";
}

async function fetchAuthSessionStatus() {
    const status = document.getElementById("auth-session-status");
    if (!status) return;
    const requestID = beginAdminSnapshot("account");
    try {
        const res = await fetch("/api/auth/sessions");
        if (!res.ok) {
            adminPageInteraction.dispatch({ type: "snapshotFailed", stream: "account", requestID, error: "Session status unavailable." });
            status.textContent = "Session status unavailable.";
            return;
        }
        const data = await res.json().catch(() => ({}));
        adminPageInteraction.dispatch({ type: "accountSnapshotReceived", requestID, data: { count: data.session_count } });
        status.textContent = `${Number(data.session_count || 0)} server-side session(s) stored.`;
    } catch (err) {
        console.error("Failed to fetch session status:", err);
        adminPageInteraction.dispatch({ type: "snapshotFailed", stream: "account", requestID, error: "Session status request failed." });
        status.textContent = "Session status request failed.";
    }
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
        plan = beginAdminCommand("changePassword", {
            hasCurrentPassword: currentPassword.length > 0,
            hasNewPassword: newPassword.length > 0,
            passwordsMatch: newPassword === confirmPassword
        });
        if (!plan) {
            setAuthPasswordFeedback("", "Current password and matching new passwords are required.");
            return;
        }
        const res = await fetch("/api/auth/password", {
            method: "PUT",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
                current_password: currentPassword,
                new_password: newPassword,
                confirm_password: confirmPassword
            })
        });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to change password.");
            finishAdminCommand(plan, null, message, true);
            setAuthPasswordFeedback("", message);
            return;
        }
        finishAdminCommand(plan, null, "Password changed.");
        if (currentInput) currentInput.value = "";
        if (newInput) newInput.value = "";
        if (confirmInput) confirmInput.value = "";
        setAuthPasswordFeedback("Password changed.", "");
    } catch (err) {
        finishAdminCommand(plan, null, err.message || "Failed to change password.", true);
        setAuthPasswordFeedback("", err.message || "Failed to change password.");
    } finally {
        if (button) button.disabled = false;
    }
}

async function clearAuthSessions() {
    if (!(await window.confirmTypedAction("Logout every server-side session, including this browser?", "LOGOUT ALL"))) {
        return;
    }
    const plan = beginAdminCommand("clearSessions");
    if (!plan) return;
    try {
        const res = await fetch("/api/auth/sessions", { method: "DELETE" });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to clear sessions.");
            finishAdminCommand(plan, null, message, true);
            alert(message);
            return;
        }
        finishAdminCommand(plan, { count: 0 }, "Sessions cleared.");
        adminPageInteraction.dispatch({ type: "metricsTokenHidden" });
        window.location.assign("/login");
    } catch (err) {
        finishAdminCommand(plan, null, err.message || "Failed to clear sessions.", true);
        alert(err.message || "Failed to clear sessions.");
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
            return;
        }
        const data = await res.json().catch(() => ({}));
        adminPageInteraction.dispatch({ type: "metricsSnapshotReceived", requestID, data });
        status.textContent = data.enabled ? "Metrics API token: enabled" : "Metrics API token: disabled";
    } catch (err) {
        console.error("Failed to fetch metrics token status:", err);
        adminPageInteraction.dispatch({ type: "snapshotFailed", stream: "metrics", requestID, error: "Metrics token status: request failed" });
        status.textContent = "Metrics token status: request failed";
    }
}

async function rotateMetricsToken(askConfirm) {
    if (askConfirm && !(await window.confirmTypedAction("Rotate metrics token? Existing scrapers using the old token will fail until updated.", "ROTATE TOKEN"))) {
        return;
    }
    const plan = beginAdminCommand("rotateMetricsToken");
    if (!plan) return;
    try {
        const res = await fetch("/api/metrics/token", { method: "POST" });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to rotate metrics token.");
            finishAdminCommand(plan, null, message, true);
            alert(message);
            return;
        }
        const data = await res.json().catch(() => ({}));
        const token = (data && typeof data.token === "string") ? data.token : "";
        if (!token) {
            finishAdminCommand(plan, data, "Token rotation succeeded but no token was returned.", true);
            alert("Token rotation succeeded but no token was returned.");
            return;
        }
        finishAdminCommand(plan, data, "Metrics token rotated.");
        showMetricsTokenOnce(token);
        fetchMetricsTokenStatus(false);
    } catch (err) {
        console.error("Failed to rotate metrics token:", err);
        finishAdminCommand(plan, null, "Failed to rotate metrics token.", true);
        alert("Failed to rotate metrics token.");
    }
}

async function disableMetricsToken() {
    if (!(await window.confirmTypedAction("Disable metrics token and hide /metrics now?", "DISABLE METRICS"))) {
        return;
    }
    const plan = beginAdminCommand("disableMetricsToken");
    if (!plan) return;
    try {
        const res = await fetch("/api/metrics/token", { method: "DELETE" });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to disable metrics token.");
            finishAdminCommand(plan, null, message, true);
            alert(message);
            return;
        }
        finishAdminCommand(plan, { enabled: false }, "Metrics token disabled.");
        showMetricsTokenOnce("");
        fetchMetricsTokenStatus();
    } catch (err) {
        console.error("Failed to disable metrics token:", err);
        finishAdminCommand(plan, null, "Failed to disable metrics token.", true);
        alert("Failed to disable metrics token.");
    }
}

async function copyMetricsToken() {
    const token = adminPageView().metrics.revealedToken;
    if (!token) {
        alert("No token to copy.");
        return;
    }
    try {
        await navigator.clipboard.writeText(token);
        alert("Metrics token copied.");
    } catch (_) {
        alert("Failed to copy token. Copy it manually from the box.");
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

async function fetchBackupStatus() {
    const status = document.getElementById("backup-status");
    if (!status) return;
    const requestID = beginAdminSnapshot("backup");
    try {
        const res = await fetch("/api/backup/status");
        if (!res.ok) {
            adminPageInteraction.dispatch({ type: "snapshotFailed", stream: "backup", requestID, error: "Backup status: unavailable" });
            status.textContent = "Backup status: unavailable";
            return;
        }
        const data = await res.json().catch(() => ({}));
        adminPageInteraction.dispatch({ type: "backupSnapshotReceived", requestID, data });
        const knownHostsState = data.known_hosts_exists ? "present" : "missing";
        status.textContent = `Backup paths: DB=${data.db_path || "-"}, config=${data.config_path || "-"}, known_hosts=${data.known_hosts_path || "-"} (${knownHostsState})`;
    } catch (err) {
        console.error("Failed to fetch backup status:", err);
        adminPageInteraction.dispatch({ type: "snapshotFailed", stream: "backup", requestID, error: "Backup status: request failed" });
        status.textContent = "Backup status: request failed";
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
            alert("Passphrase must be at least 12 characters.");
            return;
        }
        if (pass !== confirmPass) {
            alert("Passphrase confirmation does not match.");
            return;
        }
        plan = beginAdminCommand("exportBackup", { passphraseValid: pass.length >= 12, passwordsMatch: pass === confirmPass, includeKnownHosts });
        if (!plan) {
            alert(adminPageInteraction.planCommand("exportBackup").reason || "Backup is unavailable.");
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
            alert(message);
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
        alert("Backup exported.");
    } catch (err) {
        console.error("Failed to export backup:", err);
        finishAdminCommand(plan, null, "Failed to export backup.", true);
        alert("Failed to export backup.");
    } finally {
        if (exportPassInput) exportPassInput.value = "";
        if (exportPassConfirmInput) exportPassConfirmInput.value = "";
    }
}

async function restoreBackup() {
    const fileInput = document.getElementById("backup-restore-file");
    const restorePassInput = document.getElementById("backup-restore-passphrase");
    let plan;
    try {
        const pass = restorePassInput?.value || "";
        const file = fileInput?.files?.[0];
        if (!file) {
            alert("Choose a backup file first.");
            return;
        }
        if (pass.length < 12) {
            alert("Passphrase must be at least 12 characters.");
            return;
        }
        if (!(await window.confirmTypedAction("Restore will replace the current DB and optional known_hosts. Local config.json stays in place.", "RESTORE"))) {
            return;
        }
        adminPageInteraction.dispatch({ type: "backupFileSelected", file });
        plan = beginAdminCommand("restoreBackup", { passphraseValid: pass.length >= 12 });
        if (!plan) return;
        const form = new FormData();
        form.append("file", file);
        form.append("passphrase", pass);
        const res = await fetch("/api/backup/restore", {
            method: "POST",
            body: form
        });
        if (!res.ok) {
            const message = await parseErrorResponse(res, "Failed to restore backup.");
            finishAdminCommand(plan, null, message, true);
            alert(message);
            return;
        }
        const payload = await res.json().catch(() => ({}));
        finishAdminCommand(plan, payload, "Backup restored successfully.");
        alert("Backup restored successfully.");
        if (fileInput) {
            fileInput.value = "";
            updateFileLabel(fileInput, "Choose backup file");
        }
        if (payload.sessions_invalidated) {
            window.location.assign("/login");
            return;
        }
        await fetchBackupStatus();
    } catch (err) {
        console.error("Failed to restore backup:", err);
        finishAdminCommand(plan, null, "Failed to restore backup.", true);
        alert("Failed to restore backup.");
    } finally {
        if (restorePassInput) restorePassInput.value = "";
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
            alert("Choose a backup file first.");
            return;
        }
        if (pass.length < 12) {
            alert("Passphrase must be at least 12 characters.");
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
            alert(message);
            return;
        }
        const payload = await res.json().catch(() => ({}));
        finishAdminCommand(plan, payload, "Backup verified.");
        const files = Number(payload.manifest_files || 0);
        const knownHosts = payload.known_hosts_included ? "includes known_hosts" : "no known_hosts";
        const created = payload.created_at ? ` Created ${payload.created_at}.` : "";
        document.getElementById("backup-status").textContent = `Backup verified: ${files} manifest file(s), ${knownHosts}.${created}`;
    } catch (err) {
        console.error("Failed to verify backup:", err);
        finishAdminCommand(plan, null, "Failed to verify backup.", true);
        alert("Failed to verify backup.");
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

function renderPolicyPreview(preview) {
    const matched = Array.isArray(preview?.matched_servers) ? preview.matched_servers : [];
    const excluded = Array.isArray(preview?.excluded_servers) ? preview.excluded_servers : [];
    const disabled = Array.isArray(preview?.disabled_by_override) ? preview.disabled_by_override : [];
    const warnings = Array.isArray(preview?.warnings) ? preview.warnings : [];
    const skipped = [...disabled.map((item) => ({ ...item, reason: "disabled_by_override" })), ...excluded];
    document.getElementById("policy-preview-summary").textContent = matched.length
        ? `${pluralize(matched.length, "server", "servers")} would match this policy.`
        : "No current server would match this policy.";
    document.getElementById("policy-preview-count").textContent = `${matched.length} matched`;
    document.getElementById("policy-preview-matched").innerHTML = renderPolicyPreviewList(matched, "None");
    document.getElementById("policy-preview-skipped").innerHTML = renderPolicyPreviewList(skipped, "None", true);
    document.getElementById("policy-preview-warnings").innerHTML = warnings
        .map((warning) => `<div class="preview-warning">${escapeHtml(warning)}</div>`)
        .join("");
}

function setPolicyPreviewMessage(message, countText = "0 matched") {
    document.getElementById("policy-preview-summary").textContent = message;
    document.getElementById("policy-preview-count").textContent = countText;
    document.getElementById("policy-preview-matched").innerHTML = '<span class="subtle">None</span>';
    document.getElementById("policy-preview-skipped").innerHTML = '<span class="subtle">None</span>';
    document.getElementById("policy-preview-warnings").innerHTML = "";
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
            alert(await parseErrorResponse(res, "Failed to load job details."));
            return;
        }
        const data = await res.json().catch(() => ({}));
        if (!data.job) {
            alert("Job details were not returned.");
            return;
        }
        scheduledPolicyAdministration.dispatch({ type: "jobReceived", requestId: request.requestId, job: data.job, data: data.job });
        renderJobDetail(scheduledPolicyView().selectedJob, data.report_url);
        document.getElementById("job-detail-modal").classList.add("active");
        document.getElementById("job-detail-close").focus({ preventScroll: true });
    } catch (err) {
        scheduledPolicyAdministration.dispatch({ type: "snapshotFailed", stream: "job", requestId: request.requestId, error: err.message || "Failed to load job details." });
        console.error("Failed to load job details:", err);
        alert("Failed to load job details.");
    }
}

async function copyJobDetailText(kind) {
    const job = scheduledPolicyView().selectedJob;
    if (!job) return;
    const text = kind === "logs"
        ? (job.logs_text || "")
        : `Job ${job.id}\nStatus: ${job.status || "unknown"}\nPhase: ${job.phase || "unknown"}\nSummary: ${job.summary || ""}`;
    if (!String(text || "").trim()) {
        alert("Nothing to copy.");
        return;
    }
    try {
        await navigator.clipboard.writeText(text);
        alert(kind === "logs" ? "Job logs copied." : "Job summary copied.");
    } catch (_) {
        alert("Failed to copy. Select the text and copy it manually.");
    }
}

function resetPolicyForm() {
    scheduledPolicyAdministration.dispatch({ type: "editorReset" });
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
    document.getElementById("policy-save-btn").textContent = "Create Policy";
    setPolicyWeekdays([]);
    setBlackoutEditorRows("policy", []);
    setBlackoutJsonStatus("policy", "");
    refreshPolicyFormVisibility();
    updatePolicySummary();
    schedulePolicyPreview();
}

function applyPolicyToForm(policy) {
    scheduledPolicyAdministration.dispatch({ type: "editorLoaded", policy });
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
    setBlackoutEditorRows("policy", policy.policy_blackouts || []);
    setBlackoutJsonStatus("policy", "");
    setPolicyEditorModeLabel(`Editing #${policy.id}`);
    document.getElementById("policy-save-btn").textContent = "Update Policy";
    refreshPolicyFormVisibility();
    updatePolicySummary();
    schedulePolicyPreview();
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
                    <button class="btn-danger" type="button" data-action="delete-policy" data-id="${escapeHtml(String(policy.id))}">Delete</button>
                </div>
            </td>
        `;
        tbody.appendChild(row);
    });
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

function renderScheduledRuns(items) {
    const tbody = document.querySelector("#scheduled-runs-table tbody");
    if (!tbody) return;
    const runs = Array.isArray(items) ? items : [];
    tbody.innerHTML = "";
    if (!runs.length) {
        const row = document.createElement("tr");
        row.innerHTML = '<td colspan="7" class="subtle">No scheduled runs recorded yet.</td>';
        tbody.appendChild(row);
        return;
    }
    runs.forEach((run) => {
        const row = document.createElement("tr");
        const jobValue = run.job_id ? `<code>${escapeHtml(run.job_id)}</code>` : '<span class="subtle">-</span>';
        const statusToken = safeRunStatusClassToken(run.status);
        const resolvedTimezone = window.getAppTimezoneResolved ? window.getAppTimezoneResolved() : "";
        const scheduledOptions = { includeUTC: true };
        if (!resolvedTimezone && String(run.scheduled_for_display || "").trim()) {
            scheduledOptions.preformattedPrimary = run.scheduled_for_display;
        }
        const scheduled = window.formatAppTimestamp
            ? window.formatAppTimestamp(run.scheduled_for_utc, scheduledOptions)
            : { primary: run.scheduled_for_utc || "", secondary: "", title: run.scheduled_for_utc || "" };
        row.innerHTML = `
            <td title="${escapeHtml(scheduled.title || "")}">
                <div>${escapeHtml(scheduled.primary || "")}</div>
                ${scheduled.secondary ? `<div class="table-secondary">${escapeHtml(scheduled.secondary)}</div>` : ""}
            </td>
            <td>${escapeHtml(run.policy_name || "")}</td>
            <td>${escapeHtml(run.server_name || "")}</td>
            <td><span class="status-chip status-${statusToken}">${escapeHtml(run.status || "unknown")}</span></td>
            <td>${escapeHtml(run.summary || run.reason || "")}</td>
            <td>${jobValue}</td>
            <td>
                ${run.job_id ? `
                    <div class="table-actions">
                        <button class="inline-btn btn-ghost" type="button" data-action="job-detail" data-job-id="${escapeHtml(String(run.job_id))}">Details</button>
                        <a class="inline-btn btn-ghost" href="/api/reports/jobs/${encodeURIComponent(run.job_id)}">Report</a>
                    </div>
                ` : '<span class="subtle">-</span>'}
            </td>
        `;
        tbody.appendChild(row);
    });
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
    const followUp = scheduledPolicyAdministration.dispatch({ type: "snapshotReceived", stream: "policies", requestId: request.requestId, data });
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
    const followUp = scheduledPolicyAdministration.dispatch({ type: "snapshotReceived", stream: "settings", requestId: request.requestId, data });
    applyScheduledTimezone(data.timezone ? data : scheduledPolicyView().timezone || "UTC");
    setBlackoutEditorRows("global", data.global_blackouts || []);
    setBlackoutJsonStatus("global", "");
    await runScheduledEffects(followUp);
}

async function fetchScheduledRuns(request) {
    request = request || scheduledPolicyAdministration.dispatch({ type: "snapshotRequested", stream: "runs" })
        .find((effect) => effect.type === "fetchSnapshot");
    if (!request) return;
    const res = await fetch("/api/update-policies/runs?limit=50");
    if (!res.ok) {
        throw new Error(await parseErrorResponse(res, "Failed to load scheduled runs."));
    }
    const data = await res.json().catch(() => ({}));
    const followUp = scheduledPolicyAdministration.dispatch({ type: "snapshotReceived", stream: "runs", requestId: request.requestId, data });
    if (data.timezone) {
        applyScheduledTimezone(data);
    }
    renderScheduledRuns(scheduledPolicyView().runs);
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
    const followUp = scheduledPolicyAdministration.dispatch({ type: "snapshotReceived", stream: "calendar", requestId: request.requestId, data });
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
    for (const effect of effects) {
        if (effect.type !== "fetchSnapshot") continue;
        try {
            if (effect.stream === "policies") await fetchScheduledPolicies(effect);
            if (effect.stream === "settings") await fetchScheduledSettings(effect);
            if (effect.stream === "runs") await fetchScheduledRuns(effect);
            if (effect.stream === "calendar") await fetchMaintenanceCalendar(effect);
        } catch (err) {
            scheduledPolicyAdministration.dispatch({ type: "snapshotFailed", stream: effect.stream, requestId: effect.requestId, error: err.message || "Failed to refresh scheduled policy data." });
            throw err;
        }
    }
}

async function refreshScheduledUpdateViews() {
    try {
        await fetchAppTimezoneSettings(true);
        await runScheduledEffects(["policies", "settings", "runs"].flatMap((stream) => (
            scheduledPolicyAdministration.dispatch({ type: "snapshotRequested", stream })
        )));
        await runScheduledEffects(scheduledPolicyAdministration.dispatch({ type: "snapshotRequested", stream: "calendar" }));
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
        const saveBtn = document.getElementById("policy-save-btn");
        if (saveBtn) saveBtn.disabled = false;
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
        const button = document.getElementById("scheduled-settings-save");
        if (button) button.disabled = false;
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
    if (!(await window.confirmTypedAction(`Delete scheduled update policy "${required}"?`, required))) {
        return;
    }
    let activePlan;
    try {
        const command = scheduledPolicyAdministration.dispatch({ type: "commandRequested", command: "deletePolicy", policyID: id });
        const execution = command.find((effect) => effect.type === "executeCommand");
        if (!execution) throw new Error(command.find((effect) => effect.type === "commandRejected")?.message || "Scheduled policy action is unavailable.");
        activePlan = execution.plan;
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
    }
}

function handleScheduledPolicyTableClick(event) {
    const button = event.target.closest("button[data-action]");
    if (!button) return;
    const id = String(button.dataset.id || "").trim();
    const policy = scheduledPolicyView().policies.find((item) => String(item.id) === id);
    if (!policy) return;
    if (button.dataset.action === "edit-policy") {
        applyPolicyToForm(policy);
        document.getElementById("update-policy-form")?.scrollIntoView({ behavior: "smooth", block: "start" });
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
document.getElementById("app-timezone-save").addEventListener("click", saveAppTimezoneSettings);
document.getElementById("app-timezone-input").addEventListener("input", (event) => {
    adminPageInteraction.dispatch({ type: "timezoneDraftChanged", timezone: event.target.value });
    setAppTimezoneFeedback("", "");
});
document.getElementById("notification-save").addEventListener("click", saveNotificationSettings);
document.getElementById("notification-test").addEventListener("click", sendNotificationTest);
document.getElementById("notification-webhook-url").addEventListener("input", () => {
    adminPageInteraction.dispatch({ type: "notificationDraftChanged", patch: {
        enabled: Boolean(document.getElementById("notification-enabled")?.checked),
        webhookURL: document.getElementById("notification-webhook-url")?.value?.trim() || "",
        eventTypes: selectedNotificationEvents()
    } });
    setNotificationFeedback("", "");
});
document.getElementById("auth-password-save").addEventListener("click", changeAdminPassword);
document.getElementById("auth-sessions-clear").addEventListener("click", clearAuthSessions);
document.getElementById("update-policy-form").addEventListener("submit", saveScheduledPolicy);
document.getElementById("policy-reset-btn").addEventListener("click", resetPolicyForm);
document.getElementById("scheduled-settings-save").addEventListener("click", saveScheduledSettings);
document.getElementById("maintenance-calendar-refresh").addEventListener("click", reloadMaintenanceCalendar);
document.getElementById("maintenance-calendar-policy").addEventListener("change", (event) => {
    scheduledPolicyAdministration.dispatch({ type: "calendarPolicySelected", policyID: event.target.value });
    reloadMaintenanceCalendar();
});
document.querySelector("#scheduled-policy-table tbody").addEventListener("click", handleScheduledPolicyTableClick);
document.querySelector("#scheduled-runs-table tbody").addEventListener("click", handleScheduledRunsTableClick);
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
    const modal = document.getElementById("job-detail-modal");
    if (event.key === "Escape" && modal && modal.classList.contains("active")) {
        closeJobDetailModal();
    }
});

bindPolicyFormInteractions();
populateTimezonePicker();
resetPolicyForm();
fetchMetricsTokenStatus();
fetchAuthSessionStatus();
fetchBackupStatus();
fetchNotificationSettings();
refreshScheduledUpdateViews();
updateFileLabel(document.getElementById("backup-restore-file"), "Choose backup file");
