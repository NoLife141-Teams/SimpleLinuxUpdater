// Browser effects for AdminNotificationsAdapter; accepted state and command plans stay in the injected interaction owner.
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
            adminPageView,
            formatAdminRefreshTime,
            renderAdminWorkspace,
            adminPageInteraction,
            beginAdminSnapshot,
            parseErrorResponse,
            beginAdminCommand,
            finishAdminCommand,
            encodeURIComponent,
        } = options;
        function setNotificationFeedback(successMessage, errorMessage) {
            const success = document.getElementById("notification-status");
            const error = document.getElementById("notification-error");
            if (success) success.textContent = successMessage || "";
            if (error) error.textContent = errorMessage || "";
        }

        function selectedNotificationEvents() {
            return Array.from(
                document.querySelectorAll("[data-notification-event]"),
            )
                .filter((input) => input.checked)
                .map((input) => input.dataset.notificationEvent)
                .filter(Boolean);
        }

        function formatNotificationDuration(durationMS) {
            const milliseconds = Math.max(0, Number(durationMS) || 0);
            if (milliseconds < 1000) return `${milliseconds} ms`;
            return `${(milliseconds / 1000).toFixed(milliseconds < 10000 ? 2 : 1)} s`;
        }

        function renderNotificationDiagnostics() {
            const view = adminPageView();
            const diagnostics = view.notificationDiagnostics;
            const stream = view.streams.notificationDiagnostics;
            const status = diagnostics.lastAttempt;
            const message = document.getElementById(
                "notification-diagnostics-message",
            );
            const facts = document.getElementById(
                "notification-diagnostics-facts",
            );
            const refresh = document.getElementById(
                "notification-diagnostics-retry",
            );
            const outcome = document.getElementById(
                "notification-diagnostics-outcome",
            );
            const attempt = document.getElementById(
                "notification-diagnostics-attempt",
            );
            const event = document.getElementById(
                "notification-diagnostics-event",
            );
            const http = document.getElementById(
                "notification-diagnostics-http",
            );
            const duration = document.getElementById(
                "notification-diagnostics-duration",
            );
            const failures = document.getElementById(
                "notification-diagnostics-failures",
            );
            const retryRow = document.getElementById(
                "notification-diagnostics-retry-row",
            );
            const retryAt = document.getElementById(
                "notification-diagnostics-retry-at",
            );
            const errorRow = document.getElementById(
                "notification-diagnostics-error-row",
            );
            const error = document.getElementById(
                "notification-diagnostics-error",
            );
            if (refresh) refresh.disabled = stream.status === "loading";
            const pauses = document.getElementById("notification-diagnostics-rate-limits");
            if (pauses) {
                const labels = { webhook: "Webhook", discord: "Discord", telegram: "Telegram" };
                const entries = (diagnostics.rateLimitedDestinations || []).map(({destination, until}) => {
                    const row = document.createElement("div");
                    const label = document.createElement("dt");
                    label.textContent = `${labels[destination]} queue paused`;
                    const deadline = document.createElement("dd");
                    deadline.textContent = `HTTP 429 · Next eligible at ${formatAdminRefreshTime(until, "medium")}`;
                    row.append(label, deadline);
                    return row;
                });
                pauses.replaceChildren(...entries);
                pauses.hidden = entries.length === 0;
            }
            if (!status) {
                if (facts) facts.hidden = true;
                if (message) {
                    message.textContent = stream.error
                        ? `Delivery diagnostics unavailable: ${stream.error}`
                        : stream.status === "loading"
                          ? "Loading delivery diagnostics..."
                          : "No delivery attempts recorded.";
                }
                return;
            }
            if (facts) facts.hidden = false;
            const outcomeLabels = {
                succeeded: "Succeeded",
                retrying: "Retry scheduled",
                failed: "Failed",
            };
            if (outcome) {
                outcome.textContent =
                    outcomeLabels[status.outcome] || "Unknown";
                outcome.dataset.outcome = status.outcome || "unknown";
            }
            if (attempt)
                attempt.textContent =
                    formatAdminRefreshTime(status.attemptedAt) || "Unavailable";
            if (event) {
                const eventName =
                    status.eventType || status.action || "Notification";
                event.textContent = status.destination
                    ? `${status.destination} · ${eventName}`
                    : eventName;
            }
            if (http)
                http.textContent = status.statusCode
                    ? `HTTP ${status.statusCode}`
                    : "Not applicable";
            if (duration)
                duration.textContent = formatNotificationDuration(
                    status.durationMS,
                );
            if (failures)
                failures.textContent = String(status.consecutiveFailures || 0);
            const hasRetry =
                status.outcome === "retrying" && Boolean(status.nextRetryAt);
            if (retryRow) retryRow.hidden = !hasRetry;
            if (retryAt)
                retryAt.textContent = hasRetry
                    ? formatAdminRefreshTime(status.nextRetryAt, "medium") ||
                      status.nextRetryAt
                    : "";
            if (errorRow) errorRow.hidden = !status.error;
            if (error) error.textContent = status.error || "";
            if (message) {
                message.textContent = stream.error
                    ? `Showing the last successful diagnostics. Refresh failed: ${stream.error}`
                    : status.outcome === "retrying"
                      ? "The latest attempt failed and an automatic retry is scheduled."
                      : status.outcome === "succeeded"
                        ? "The latest delivery attempt succeeded."
                        : "The latest delivery attempt failed and no retry is scheduled.";
            }
        }

        function renderNotificationSettings() {
            const view = adminPageView().notifications;
            const enabled = document.getElementById("notification-enabled");
            const webhookURL = document.getElementById(
                "notification-webhook-url",
            );
            const masked = document.getElementById(
                "notification-webhook-masked",
            );
            const editor = document.getElementById(
                "notification-webhook-editor",
            );
            const replace = document.getElementById(
                "notification-webhook-replace",
            );
            const clear = document.getElementById("notification-webhook-clear");
            const eventTypes = view.eventTypes;
            if (enabled) enabled.checked = view.enabled;
            const reminders = document.getElementById(
                "notification-reminders-enabled",
            );
            const reminderInterval = document.getElementById(
                "notification-reminder-interval",
            );
            if (reminders) reminders.checked = view.remindersEnabled;
            if (reminderInterval)
                reminderInterval.value = view.reminderIntervalMinutes;
            if (masked) {
                masked.textContent =
                    view.webhookURLIntent === "clear"
                        ? "Will be cleared when saved"
                        : view.webhookConfigured
                          ? view.webhookURLMasked ||
                            "Configured endpoint (masked)"
                          : "Not configured";
            }
            if (editor) editor.hidden = view.webhookURLIntent !== "replace";
            if (replace) replace.disabled = view.webhookURLIntent === "replace";
            if (clear)
                clear.disabled =
                    !view.webhookConfigured ||
                    view.webhookURLIntent === "clear";
            if (webhookURL && view.webhookURLIntent !== "replace")
                webhookURL.value = "";
            const discordEnabled = document.getElementById(
                "notification-discord-enabled",
            );
            const discordMasked = document.getElementById(
                "notification-discord-masked",
            );
            const discordEditor = document.getElementById(
                "notification-discord-editor",
            );
            const discordReplace = document.getElementById(
                "notification-discord-replace",
            );
            const discordClear = document.getElementById(
                "notification-discord-clear",
            );
            const discordURL = document.getElementById(
                "notification-discord-url",
            );
            if (discordEnabled) discordEnabled.checked = view.discordEnabled;
            if (discordMasked) {
                discordMasked.textContent =
                    view.discordWebhookURLIntent === "clear"
                        ? "Will be cleared when saved"
                        : view.discordConfigured
                          ? view.discordWebhookURLMasked ||
                            "Configured Discord webhook (masked)"
                          : "Not configured";
            }
            if (discordEditor)
                discordEditor.hidden =
                    view.discordWebhookURLIntent !== "replace";
            if (discordReplace)
                discordReplace.disabled =
                    view.discordWebhookURLIntent === "replace";
            if (discordClear)
                discordClear.disabled =
                    !view.discordConfigured ||
                    view.discordWebhookURLIntent === "clear";
            if (discordURL && view.discordWebhookURLIntent !== "replace")
                discordURL.value = "";
            const telegramEnabled = document.getElementById(
                "notification-telegram-enabled",
            );
            const telegramMasked = document.getElementById(
                "notification-telegram-masked",
            );
            const telegramEditor = document.getElementById(
                "notification-telegram-editor",
            );
            const telegramReplace = document.getElementById(
                "notification-telegram-replace",
            );
            const telegramClear = document.getElementById(
                "notification-telegram-clear",
            );
            const telegramToken = document.getElementById(
                "notification-telegram-token",
            );
            const telegramChatID = document.getElementById(
                "notification-telegram-chat-id",
            );
            if (telegramEnabled) telegramEnabled.checked = view.telegramEnabled;
            if (telegramMasked) {
                telegramMasked.textContent =
                    view.telegramCredentialsIntent === "clear"
                        ? "Will be cleared when saved"
                        : view.telegramConfigured
                          ? `${view.telegramBotTokenMasked || "Bot configured"} · ${view.telegramChatIDMasked || "chat configured"}`
                          : "Not configured";
            }
            if (telegramEditor)
                telegramEditor.hidden =
                    view.telegramCredentialsIntent !== "replace";
            if (telegramReplace)
                telegramReplace.disabled =
                    view.telegramCredentialsIntent === "replace";
            if (telegramClear)
                telegramClear.disabled =
                    !view.telegramConfigured ||
                    view.telegramCredentialsIntent === "clear";
            if (telegramToken && view.telegramCredentialsIntent !== "replace")
                telegramToken.value = "";
            if (telegramChatID && view.telegramCredentialsIntent !== "replace")
                telegramChatID.value = "";
            document
                .querySelectorAll("[data-notification-test]")
                .forEach((button) => {
                    const destination = button.dataset.notificationTest;
                    button.disabled =
                        destination === "webhook"
                            ? !view.webhookConfigured
                            : destination === "discord"
                              ? !view.discordConfigured
                              : !view.telegramConfigured;
                });
            document
                .querySelectorAll("[data-notification-event]")
                .forEach((input) => {
                    input.checked = eventTypes.includes(
                        input.dataset.notificationEvent,
                    );
                });
            renderNotificationDraftState();
            renderAdminWorkspace();
        }

        function renderNotificationDraftState() {
            const view = adminPageView();
            const notification = view.notifications;
            const inFlight =
                view.commands.inFlight.includes("saveNotifications");
            const save = document.getElementById("notification-save");
            const discard = document.getElementById("notification-discard");
            const state = document.getElementById("notification-draft-state");
            if (save) {
                save.disabled =
                    !notification.dirty || !notification.valid || inFlight;
                save.title = notification.valid ? "" : notification.message;
            }
            if (discard) discard.disabled = !notification.dirty || inFlight;
            if (state) {
                const intentLabels = {
                    replace: "Webhook URL will be replaced",
                    clear: "Webhook URL will be cleared",
                };
                state.textContent = notification.dirty
                    ? notification.valid
                        ? intentLabels[notification.webhookURLIntent] ||
                          "Unsaved notification changes"
                        : notification.message
                    : "No unsaved changes";
                state.classList.toggle("is-dirty", notification.dirty);
            }
        }

        function notificationReplacementPolicyValid(raw) {
            try {
                const parsed = new URL(String(raw || "").trim());
                if (parsed.username || parsed.password) return false;
                if (parsed.protocol === "https:") return true;
                if (parsed.protocol !== "http:") return false;
                const host = parsed.hostname.toLowerCase().replace(/\.$/, "");
                if (
                    host === "localhost" ||
                    host.endsWith(".localhost") ||
                    host.endsWith(".local") ||
                    host.endsWith(".internal")
                )
                    return true;
                if (
                    host === "::1" ||
                    /^127\./.test(host) ||
                    /^10\./.test(host) ||
                    /^192\.168\./.test(host)
                )
                    return true;
                const private172 = host.match(/^172\.(\d{1,3})\./);
                return Boolean(
                    private172 &&
                    Number(private172[1]) >= 16 &&
                    Number(private172[1]) <= 31,
                );
            } catch (_) {
                if (!lifecycle.active || _?.name === "AbortError") return;
                return false;
            }
        }

        function discordReplacementPolicyValid(raw) {
            try {
                const parsed = new URL(String(raw || "").trim());
                const host = parsed.hostname.toLowerCase();
                const officialHost =
                    host === "discord.com" ||
                    host.endsWith(".discord.com") ||
                    host === "discordapp.com" ||
                    host.endsWith(".discordapp.com");
                return (
                    parsed.protocol === "https:" &&
                    !parsed.username &&
                    !parsed.password &&
                    officialHost &&
                    /\/webhooks\/[^/]+\/[^/]+/.test(parsed.pathname)
                );
            } catch {
                if (!lifecycle.active) return;
                return false;
            }
        }

        function telegramReplacementPolicyValid(token, chatID) {
            const normalizedToken = String(token || "").trim();
            const normalizedChatID = String(chatID || "").trim();
            return (
                /^[0-9]+:[A-Za-z0-9_-]{20,}$/.test(normalizedToken) &&
                (/^-?[0-9]+$/.test(normalizedChatID) ||
                    /^@[A-Za-z][A-Za-z0-9_]{4,31}$/.test(normalizedChatID))
            );
        }

        function syncNotificationDraftFromDOM() {
            const current = adminPageView().notifications;
            const replacement =
                document
                    .getElementById("notification-webhook-url")
                    ?.value?.trim() || "";
            const discordReplacement =
                document
                    .getElementById("notification-discord-url")
                    ?.value?.trim() || "";
            const telegramToken =
                document
                    .getElementById("notification-telegram-token")
                    ?.value?.trim() || "";
            const telegramChatID =
                document
                    .getElementById("notification-telegram-chat-id")
                    ?.value?.trim() || "";
            adminPageInteraction.dispatch({
                type: "notificationDraftChanged",
                patch: {
                    enabled: Boolean(
                        document.getElementById("notification-enabled")
                            ?.checked,
                    ),
                    webhookURLIntent: current.webhookURLIntent,
                    replacementProvided:
                        current.webhookURLIntent === "replace" &&
                        replacement !== "",
                    replacementValid:
                        current.webhookURLIntent !== "replace" ||
                        notificationReplacementPolicyValid(replacement),
                    discordEnabled: Boolean(
                        document.getElementById("notification-discord-enabled")
                            ?.checked,
                    ),
                    discordWebhookURLIntent: current.discordWebhookURLIntent,
                    discordReplacementProvided:
                        current.discordWebhookURLIntent === "replace" &&
                        discordReplacement !== "",
                    discordReplacementValid:
                        current.discordWebhookURLIntent !== "replace" ||
                        discordReplacementPolicyValid(discordReplacement),
                    telegramEnabled: Boolean(
                        document.getElementById("notification-telegram-enabled")
                            ?.checked,
                    ),
                    telegramCredentialsIntent:
                        current.telegramCredentialsIntent,
                    telegramReplacementProvided:
                        current.telegramCredentialsIntent === "replace" &&
                        telegramToken !== "" &&
                        telegramChatID !== "",
                    telegramReplacementValid:
                        current.telegramCredentialsIntent !== "replace" ||
                        telegramReplacementPolicyValid(
                            telegramToken,
                            telegramChatID,
                        ),
                    remindersEnabled: Boolean(
                        document.getElementById(
                            "notification-reminders-enabled",
                        )?.checked,
                    ),
                    reminderIntervalMinutes: Number(
                        document.getElementById(
                            "notification-reminder-interval",
                        )?.value,
                    ),
                    eventTypes: selectedNotificationEvents(),
                },
            });
            setNotificationFeedback("", "");
            renderNotificationDraftState();
        }

        function setDiscordWebhookIntent(intent) {
            const input = document.getElementById("notification-discord-url");
            if (input) input.value = "";
            adminPageInteraction.dispatch({
                type: "notificationDraftChanged",
                patch: {
                    discordWebhookURLIntent: intent,
                    discordReplacementProvided: false,
                    discordReplacementValid: true,
                },
            });
            setNotificationFeedback("", "");
            renderNotificationSettings();
            if (intent === "replace") input?.focus();
        }

        function setTelegramCredentialsIntent(intent) {
            const token = document.getElementById(
                "notification-telegram-token",
            );
            const chatID = document.getElementById(
                "notification-telegram-chat-id",
            );
            if (token) token.value = "";
            if (chatID) chatID.value = "";
            adminPageInteraction.dispatch({
                type: "notificationDraftChanged",
                patch: {
                    telegramCredentialsIntent: intent,
                    telegramReplacementProvided: false,
                    telegramReplacementValid: true,
                },
            });
            setNotificationFeedback("", "");
            renderNotificationSettings();
            if (intent === "replace") token?.focus();
        }

        function setNotificationWebhookIntent(intent) {
            const input = document.getElementById("notification-webhook-url");
            if (input) input.value = "";
            adminPageInteraction.dispatch({
                type: "notificationDraftChanged",
                patch: {
                    webhookURLIntent: intent,
                    replacementProvided: false,
                    replacementValid: true,
                },
            });
            setNotificationFeedback("", "");
            renderNotificationSettings();
            if (intent === "replace") input?.focus();
        }

        function discardNotificationDraft() {
            adminPageInteraction.dispatch({
                type: "notificationDiscardRequested",
            });
            const webhookURL = document.getElementById(
                "notification-webhook-url",
            );
            if (webhookURL) webhookURL.value = "";
            const discordURL = document.getElementById(
                "notification-discord-url",
            );
            const telegramToken = document.getElementById(
                "notification-telegram-token",
            );
            const telegramChatID = document.getElementById(
                "notification-telegram-chat-id",
            );
            if (discordURL) discordURL.value = "";
            if (telegramToken) telegramToken.value = "";
            if (telegramChatID) telegramChatID.value = "";
            renderNotificationSettings();
            setNotificationFeedback("", "");
        }

        function applyNotificationSettings(payload, requestID = null) {
            const effects = adminPageInteraction.dispatch({
                type: "notificationSnapshotReceived",
                requestID,
                receivedAt: new Date().toISOString(),
                data: payload,
            });
            if (effects.length === 0) return false;
            renderNotificationSettings();
            return true;
        }

        async function fetchNotificationSettings() {
            const lifetime = lifecycle.capture();

            const requestID = beginAdminSnapshot("notifications");
            try {
                const res = await fetch("/api/notifications/settings", {
                    cache: "no-store",
                });
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    const message = await parseErrorResponse(
                        res,
                        "Failed to load notification settings.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    adminPageInteraction.dispatch({
                        type: "snapshotFailed",
                        stream: "notifications",
                        requestID,
                        error: message,
                    });
                    setNotificationFeedback("", message);
                    renderAdminWorkspace();
                    return;
                }
                const data = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                applyNotificationSettings(data, requestID);
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                console.error("Failed to load notification settings:", err);
                adminPageInteraction.dispatch({
                    type: "snapshotFailed",
                    stream: "notifications",
                    requestID,
                    error: "Failed to load notification settings.",
                });
                setNotificationFeedback(
                    "",
                    "Failed to load notification settings.",
                );
                renderAdminWorkspace();
            }
        }

        function applyNotificationDeliveryDiagnostics(
            payload,
            requestID = null,
        ) {
            const effects = adminPageInteraction.dispatch({
                type: "notificationDiagnosticsReceived",
                requestID,
                receivedAt: new Date().toISOString(),
                data: payload,
            });
            if (effects.length === 0) return false;
            renderNotificationDiagnostics();
            return true;
        }

        async function fetchNotificationDeliveryDiagnostics() {
            const lifetime = lifecycle.capture();

            const requestID = beginAdminSnapshot("notificationDiagnostics");
            renderNotificationDiagnostics();
            try {
                const res = await fetch(
                    "/api/notifications/delivery-diagnostics",
                    { cache: "no-store" },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    const message = await parseErrorResponse(
                        res,
                        "Failed to load delivery diagnostics.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    adminPageInteraction.dispatch({
                        type: "snapshotFailed",
                        stream: "notificationDiagnostics",
                        requestID,
                        error: message,
                    });
                    renderNotificationDiagnostics();
                    return;
                }
                const data = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                applyNotificationDeliveryDiagnostics(data, requestID);
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                console.error("Failed to load delivery diagnostics:", err);
                adminPageInteraction.dispatch({
                    type: "snapshotFailed",
                    stream: "notificationDiagnostics",
                    requestID,
                    error: "Failed to load delivery diagnostics.",
                });
                renderNotificationDiagnostics();
            }
        }

        function initNotificationHistory() {
            const panel = document.getElementById("notification-history");
            if (!panel) return;
            const filters = document.getElementById("notification-history-filters");
            const status = document.getElementById("notification-history-status");
            const results = document.getElementById("notification-history-items");
            const retention = document.getElementById("notification-history-retention");
            const pages = document.getElementById("notification-history-pages");
            const newer = document.getElementById("notification-history-newer");
            const older = document.getElementById("notification-history-older");
            const retry = document.getElementById("notification-history-retry");
            const destinations = { webhook: "Webhook", discord: "Discord", telegram: "Telegram" };
            const states = {
                pending: "Pending", claimed: "Sending", retrying: "Retry scheduled",
                succeeded: "Delivered", failed: "Failed", skipped: "Skipped",
            };
            const events = {
                "approval.published": "Published update approval",
                "approval.expired": "Expired update approval",
                "apt.reconciliation.required": "APT reconciliation required",
                "update.complete": "Completed update",
                "schedule.run.failed": "Failed scheduled run",
                "schedule.run.skipped": "Skipped scheduled run",
                "notification.reminder": "Grouped reminder",
            };
            let navigation = { filters: {}, cursors: [""] };
            let retryQuery = null;

            function addText(parent, tag, text, className = "") {
                const element = document.createElement(tag);
                if (className) element.className = className;
                element.textContent = text;
                parent.append(element);
                return element;
            }

            function deliveryRow(item) {
                const row = document.createElement("li");
                row.className = "notification-history-item";
                const event = document.createElement("div");
                const delivery = document.createElement("div");
                const times = document.createElement("div");
                addText(event, "strong", destinations[item.destination] || "Notification");
                addText(event, "span", events[item.event_type] || item.event_type);
                if (item.job_id) addText(event, "code", item.job_id, "muted");
                if (item.approval_generation) addText(event, "small", `Plan generation ${item.approval_generation}`, "muted");
                if (item.job_id && !item.job_report_url) addText(event, "small", "Job report unavailable", "muted");
                if (item.job_report_url || item.approval_plan_url) {
                    const links = document.createElement("div");
                    links.className = "notification-history-links";
                    if (item.job_report_url) addText(links, "a", "Job report", "btn-ghost inline-btn").href = item.job_report_url;
                    if (item.approval_plan_url) addText(links, "a", "Review approval plan", "btn-ghost inline-btn").href = item.approval_plan_url;
                    event.append(links);
                }

                addText(delivery, "strong", states[item.state] || "Unknown").dataset.state = item.state;
                addText(delivery, "small", `${item.attempts} ${item.attempts === 1 ? "attempt" : "attempts"}`, "muted");
                if (item.reason) addText(delivery, "small", item.reason);
                for (const [label, timestamp] of [
                    ["Queued", item.created_at], ["Last attempt", item.attempted_at],
                    ["Completed", item.completed_at], ["Next retry", item.next_attempt_at],
                ]) {
                    const display = timestamp && formatAdminRefreshTime(timestamp);
                    if (display) addText(times, "time", `${label} · ${display}`, "muted").dateTime = timestamp;
                }
                row.append(event, delivery, times);
                return row;
            }

            function render() {
                const { notificationHistory: history, streams } = adminPageView();
                const stream = streams.notificationHistory;
                const loading = stream.status === "loading";
                const current = stream.status === "current";
                status.textContent = loading ? "Loading delivery history…"
                    : stream.error || (current && history.items.length === 0 ? "No matching deliveries." : "");
                retry.hidden = !stream.error;
                retention.hidden = !history.retentionSeconds;
                if (history.retentionSeconds) {
                    const inDays = history.retentionSeconds >= 86400;
                    const duration = inDays ? history.retentionSeconds / 86400 : history.retentionSeconds / 3600;
                    const unit = inDays ? "day" : "hour";
                    const count = new Intl.NumberFormat("en", { maximumFractionDigits: 1 }).format(duration);
                    retention.textContent = `Completed deliveries are kept for ${count} ${unit}${duration === 1 ? "" : "s"}.`;
                }
                results.hidden = !current || history.items.length === 0;
                results.replaceChildren(...(current ? history.items.map(deliveryRow) : []));
                newer.disabled = !current || navigation.cursors.length < 2;
                older.disabled = !current || !history.nextCursor;
                pages.hidden = newer.disabled && older.disabled;
            }

            async function load(query, focus = false) {
                const lifetime = lifecycle.capture();
                const requestID = beginAdminSnapshot("notificationHistory");
                render();
                const params = new URLSearchParams(query.filters);
                const before = query.cursors.at(-1);
                if (before) params.set("before", before);
                const fail = message => {
                    const effects = adminPageInteraction.dispatch({
                        type: "snapshotFailed", stream: "notificationHistory", requestID, error: message,
                    });
                    if (effects.length === 0) return;
                    retryQuery = query;
                    render();
                    if (focus) status.focus();
                };
                try {
                    const res = await fetch(`/api/notifications/delivery-history?${params}`, { cache: "no-store" });
                    if (!lifecycle.isCurrent(lifetime)) return;
                    if (!res.ok) {
                        const message = await parseErrorResponse(res, "Delivery history is temporarily unavailable.");
                        if (lifecycle.isCurrent(lifetime)) fail(message);
                        return;
                    }
                    const data = await res.json();
                    if (!lifecycle.isCurrent(lifetime)) return;
                    if (!Array.isArray(data?.items) || data.items.some(item => !item || typeof item !== "object")) {
                        fail("Delivery history is temporarily unavailable.");
                        return;
                    }
                    const effects = adminPageInteraction.dispatch({
                        type: "notificationHistoryReceived", requestID, receivedAt: new Date().toISOString(), data,
                    });
                    if (effects.length === 0) return;
                    navigation = query;
                    retryQuery = null;
                    render();
                    if (focus) (data.items.length ? results : status).focus();
                } catch (error) {
                    if (lifecycle.isCurrent(lifetime) && error?.name !== "AbortError") {
                        fail("Delivery history is temporarily unavailable.");
                    }
                }
            }

            const loadOnOpen = () => {
                const stream = adminPageView().streams.notificationHistory;
                if (panel.open && stream.status !== "loading") void load(retryQuery || navigation);
            };
            lifecycle.listen(panel, "toggle", loadOnOpen);
            lifecycle.listen(filters, "submit", event => {
                event.preventDefault();
                const applied = Object.fromEntries(Array.from(new FormData(filters), ([key, value]) => [key, value.trim()]).filter(([, value]) => value));
                void load({ filters: applied, cursors: [""] });
            });
            lifecycle.listen(newer, "click", () => void load({ ...navigation, cursors: navigation.cursors.slice(0, -1) }, true));
            lifecycle.listen(older, "click", () => void load({
                ...navigation, cursors: [...navigation.cursors, adminPageView().notificationHistory.nextCursor],
            }, true));
            lifecycle.listen(retry, "click", () => { if (retryQuery) void load(retryQuery, true); });
            loadOnOpen();
        }

        function init() {
            if (lifecycle.active) return;
            lifecycle.init();
            initNotificationHistory();
        }

        async function saveNotificationSettings() {
            const lifetime = lifecycle.capture();

            const button = document.getElementById("notification-save");
            let plan;
            try {
                setNotificationFeedback("", "");
                if (button) button.disabled = true;
                syncNotificationDraftFromDOM();
                plan = beginAdminCommand("saveNotifications");
                if (!plan) return;
                const payload = { ...plan.payload };
                if (payload.webhook_url_intent === "replace") {
                    payload.webhook_url =
                        document
                            .getElementById("notification-webhook-url")
                            ?.value?.trim() || "";
                }
                if (payload.discord?.webhook_url_intent === "replace") {
                    payload.discord.webhook_url =
                        document
                            .getElementById("notification-discord-url")
                            ?.value?.trim() || "";
                }
                if (payload.telegram?.credentials_intent === "replace") {
                    payload.telegram.bot_token =
                        document
                            .getElementById("notification-telegram-token")
                            ?.value?.trim() || "";
                    payload.telegram.chat_id =
                        document
                            .getElementById("notification-telegram-chat-id")
                            ?.value?.trim() || "";
                }
                const res = await fetch("/api/notifications/settings", {
                    method: "PUT",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify(payload),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    const message = await parseErrorResponse(
                        res,
                        "Failed to save notification settings.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    finishAdminCommand(plan, null, message, true);
                    setNotificationFeedback("", message);
                    return;
                }
                const data = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                finishAdminCommand(plan, data, "Notification settings saved.");
                renderNotificationSettings();
                const outcomes = {
                    replace: "Webhook URL replaced.",
                    clear: "Webhook URL cleared.",
                    preserve:
                        "Notification settings saved; the configured URL was preserved.",
                };
                setNotificationFeedback(
                    outcomes[data.webhook_url_intent] ||
                        "Notification settings saved.",
                    "",
                );
                void fetchNotificationDeliveryDiagnostics();
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                console.error("Failed to save notification settings:", err);
                finishAdminCommand(
                    plan,
                    null,
                    "Failed to save notification settings.",
                    true,
                );
                setNotificationFeedback(
                    "",
                    "Failed to save notification settings.",
                );
            } finally {
                if (!lifecycle.isCurrent(lifetime)) return;
                renderNotificationDraftState();
            }
        }

        async function sendNotificationTest(destination = "webhook") {
            const lifetime = lifecycle.capture();

            const button = document.querySelector(
                `[data-notification-test="${destination}"]`,
            );
            let plan;
            try {
                setNotificationFeedback("", "");
                if (button) button.disabled = true;
                plan = beginAdminCommand("testNotification", { destination });
                if (!plan) return;
                const res = await fetch(
                    `/api/notifications/test?destination=${encodeURIComponent(plan.payload.destination)}`,
                    { method: "POST" },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                const payload = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                const refreshInterrupted = adminPageView().streams.notificationDiagnostics.status === "loading";
                if (!res.ok) {
                    finishAdminCommand(
                        plan,
                        payload,
                        "Notification test failed.",
                        true,
                    );
                    renderNotificationDiagnostics();
                    if (refreshInterrupted) void fetchNotificationDeliveryDiagnostics();
                    const message = await parseErrorResponse(
                        res,
                        "Notification test failed.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    setNotificationFeedback("", message);
                    return;
                }
                finishAdminCommand(
                    plan,
                    payload,
                    "Notification test delivered.",
                );
                renderNotificationDiagnostics();
                if (refreshInterrupted) void fetchNotificationDeliveryDiagnostics();
                const destinationLabel =
                    destination.charAt(0).toUpperCase() + destination.slice(1);
                setNotificationFeedback(
                    `${destinationLabel} test delivered.`,
                    "",
                );
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                console.error("Failed to send notification test:", err);
                finishAdminCommand(
                    plan,
                    null,
                    "Notification test failed.",
                    true,
                );
                setNotificationFeedback("", "Notification test failed.");
            } finally {
                if (!lifecycle.isCurrent(lifetime)) return;
                if (button) button.disabled = false;
            }
        }
        return Object.freeze({
            init,
            destroy: lifecycle.destroy,
            renderNotificationDraftState: (...args) =>
                lifecycle.active
                    ? renderNotificationDraftState(...args)
                    : undefined,
            syncNotificationDraftFromDOM: (...args) =>
                lifecycle.active
                    ? syncNotificationDraftFromDOM(...args)
                    : undefined,
            setDiscordWebhookIntent: (...args) =>
                lifecycle.active ? setDiscordWebhookIntent(...args) : undefined,
            setTelegramCredentialsIntent: (...args) =>
                lifecycle.active
                    ? setTelegramCredentialsIntent(...args)
                    : undefined,
            setNotificationWebhookIntent: (...args) =>
                lifecycle.active
                    ? setNotificationWebhookIntent(...args)
                    : undefined,
            discardNotificationDraft: (...args) =>
                lifecycle.active
                    ? discardNotificationDraft(...args)
                    : undefined,
            fetchNotificationSettings: (...args) =>
                lifecycle.active
                    ? fetchNotificationSettings(...args)
                    : undefined,
            fetchNotificationDeliveryDiagnostics: (...args) =>
                lifecycle.active
                    ? fetchNotificationDeliveryDiagnostics(...args)
                    : undefined,
            saveNotificationSettings: (...args) =>
                lifecycle.active
                    ? saveNotificationSettings(...args)
                    : undefined,
            sendNotificationTest: (...args) =>
                lifecycle.active ? sendNotificationTest(...args) : undefined,
        });
    }
    root.AdminNotificationsAdapter = Object.freeze({ create });
})(globalThis);
