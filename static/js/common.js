(function () {
    if (window.__commonHelpersReady) return;
    window.__commonHelpersReady = true;
    const appTimezoneState = window.__appTimezoneState || {
        value: "UTC",
        resolved: "UTC",
        editable: "",
        loaded: false,
        loadingPromise: null
    };
    window.__appTimezoneState = appTimezoneState;

    function normalizeAppTimezone(value) {
        const raw = String(value ?? "").trim();
        return raw || "UTC";
    }

    function normalizeResolvedTimezone(value) {
        return String(value ?? "").trim();
    }

    function normalizeEditableTimezone(value) {
        return String(value ?? "").trim();
    }

    function defaultEditableTimezone(timezone) {
        const value = normalizeAppTimezone(timezone);
        if (/^[+-]\d{2}:\d{2}$/.test(value)) {
            return "Local";
        }
        return value || "UTC";
    }

    function timezoneForIntl(timezone) {
        return normalizeResolvedTimezone(timezone);
    }

    function buildTimestampFormatter(timeZone) {
        return new Intl.DateTimeFormat(undefined, {
            timeZone,
            year: "numeric",
            month: "short",
            day: "2-digit",
            hour: "2-digit",
            minute: "2-digit",
            hourCycle: "h23",
            timeZoneName: "short"
        });
    }

    function parseTimestamp(value) {
        const raw = String(value ?? "").trim();
        if (!raw) return null;
        const parsed = new Date(raw);
        return Number.isNaN(parsed.getTime()) ? null : parsed;
    }

    window.escapeHtml = window.escapeHtml || function escapeHtml(value) {
        return String(value ?? "")
            .replace(/&/g, "&amp;")
            .replace(/</g, "&lt;")
            .replace(/>/g, "&gt;")
            .replace(/\"/g, "&quot;")
            .replace(/'/g, "&#39;");
    };

    window.parseErrorResponse = window.parseErrorResponse || async function parseErrorResponse(res, fallbackMessage) {
        const data = await res.json().catch(() => ({}));
        return data.error || fallbackMessage;
    };

    window.updateFileLabel = window.updateFileLabel || function updateFileLabel(input, emptyLabel = "Choose file") {
        if (!input) return;
        const label = document.querySelector(`label[for="${input.id}"]`);
        if (!label) return;
        const file = input.files && input.files[0];
        label.textContent = file ? file.name : emptyLabel;
    };

    function ensureFeedbackRegion() {
        let region = document.getElementById("app-feedback-region");
        if (region) return region;
        region = document.createElement("div");
        region.id = "app-feedback-region";
        region.className = "app-feedback-region";
        region.setAttribute("role", "status");
        region.setAttribute("aria-live", "polite");
        region.setAttribute("aria-atomic", "true");
        document.body.appendChild(region);
        return region;
    }

    window.notifyApp = window.notifyApp || function notifyApp(message, options = {}) {
        const region = ensureFeedbackRegion();
        const item = document.createElement("div");
        item.className = `app-feedback ${options.tone === "success" ? "success" : options.tone === "warning" ? "warning" : options.tone === "info" ? "info" : "danger"}`;
        item.textContent = String(message || "");
        region.replaceChildren(item);
        window.setTimeout(() => {
            if (item.parentNode === region) item.remove();
        }, Number(options.duration || 6000));
    };

    function ensureActionConfirmModal() {
        let backdrop = document.getElementById("action-confirm-modal");
        if (backdrop) return backdrop;
        backdrop = document.createElement("div");
        backdrop.className = "modal-backdrop action-confirm-backdrop";
        backdrop.id = "action-confirm-modal";
        backdrop.innerHTML = `
            <div class="modal action-confirm-modal" role="dialog" aria-modal="true" aria-labelledby="action-confirm-title" aria-describedby="action-confirm-message">
                <div class="eyebrow">Review operation</div>
                <h2 id="action-confirm-title">Confirm action</h2>
                <p class="muted action-confirm-message" id="action-confirm-message"></p>
                <div class="modal-actions">
                    <button class="btn-ghost inline-btn" id="action-confirm-cancel" type="button">Cancel</button>
                    <button class="primary-action inline-btn" id="action-confirm-submit" type="button">Confirm</button>
                </div>
            </div>`;
        document.body.appendChild(backdrop);
        return backdrop;
    }

    window.confirmAction = window.confirmAction || function confirmAction(message, options = {}) {
        return new Promise((resolve) => {
            const backdrop = ensureActionConfirmModal();
            const dialog = backdrop.querySelector(".action-confirm-modal");
            const submit = backdrop.querySelector("#action-confirm-submit");
            const cancel = backdrop.querySelector("#action-confirm-cancel");
            const messageNode = backdrop.querySelector("#action-confirm-message");
            const previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
            const focusable = () => Array.from(dialog.querySelectorAll("button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])"));
            const close = (confirmed) => {
                backdrop.classList.remove("active");
                document.removeEventListener("keydown", onKeydown);
                submit.removeEventListener("click", onSubmit);
                cancel.removeEventListener("click", onCancel);
                backdrop.removeEventListener("click", onBackdrop);
                if (previousFocus && typeof previousFocus.focus === "function") previousFocus.focus();
                resolve(confirmed);
            };
            const onSubmit = () => close(true);
            const onCancel = () => close(false);
            const onBackdrop = (event) => { if (event.target === backdrop) close(false); };
            const onKeydown = (event) => {
                if (event.key === "Escape") {
                    event.preventDefault();
                    close(false);
                    return;
                }
                if (event.key !== "Tab") return;
                const items = focusable();
                if (!items.length) return;
                const first = items[0];
                const last = items[items.length - 1];
                if (event.shiftKey && document.activeElement === first) {
                    event.preventDefault();
                    last.focus();
                } else if (!event.shiftKey && document.activeElement === last) {
                    event.preventDefault();
                    first.focus();
                }
            };
            messageNode.textContent = String(message || "");
            submit.textContent = String(options.confirmLabel || "Confirm");
            submit.className = `${options.danger ? "btn-danger" : "primary-action"} inline-btn`;
            submit.addEventListener("click", onSubmit);
            cancel.addEventListener("click", onCancel);
            backdrop.addEventListener("click", onBackdrop);
            document.addEventListener("keydown", onKeydown);
            backdrop.classList.add("active");
            window.requestAnimationFrame(() => cancel.focus());
        });
    };

    function ensureTypedConfirmModal() {
        let backdrop = document.getElementById("typed-confirm-modal");
        if (backdrop) return backdrop;
        backdrop = document.createElement("div");
        backdrop.className = "modal-backdrop typed-confirm-backdrop";
        backdrop.id = "typed-confirm-modal";
        backdrop.innerHTML = `
            <div class="modal typed-confirm-modal" role="dialog" aria-modal="true" aria-labelledby="typed-confirm-title">
                <div class="eyebrow">Confirmation required</div>
                <h2 id="typed-confirm-title">Confirm Action</h2>
                <p class="muted" id="typed-confirm-message"></p>
                <label class="field full-width" for="typed-confirm-input">
                    <span id="typed-confirm-label">Type the confirmation text</span>
                    <input type="text" id="typed-confirm-input" autocomplete="off" spellcheck="false">
                </label>
                <div class="modal-actions">
                    <button class="btn-ghost inline-btn" id="typed-confirm-cancel" type="button">Cancel</button>
                    <button class="btn-danger inline-btn" id="typed-confirm-submit" type="button" disabled>Confirm</button>
                </div>
            </div>
        `;
        document.body.appendChild(backdrop);
        return backdrop;
    }

    window.confirmTypedAction = window.confirmTypedAction || function confirmTypedAction(message, requiredText) {
        const required = String(requiredText || "").trim();
        if (!required) {
            return window.confirmAction(message);
        }
        return new Promise((resolve) => {
            const backdrop = ensureTypedConfirmModal();
            const input = backdrop.querySelector("#typed-confirm-input");
            const submit = backdrop.querySelector("#typed-confirm-submit");
            const cancel = backdrop.querySelector("#typed-confirm-cancel");
            const messageNode = backdrop.querySelector("#typed-confirm-message");
            const label = backdrop.querySelector("#typed-confirm-label");
            const previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
            const close = (confirmed) => {
                backdrop.classList.remove("active");
                input.value = "";
                submit.disabled = true;
                input.removeEventListener("input", onInput);
                input.removeEventListener("keydown", onKeydown);
                submit.removeEventListener("click", onSubmit);
                cancel.removeEventListener("click", onCancel);
                backdrop.removeEventListener("click", onBackdropClick);
                document.removeEventListener("keydown", onDocumentKeydown);
                if (previousFocus && typeof previousFocus.focus === "function") {
                    previousFocus.focus();
                }
                resolve(confirmed);
            };
            const onInput = () => {
                submit.disabled = input.value !== required;
            };
            const onSubmit = () => {
                close(input.value === required);
            };
            const onCancel = () => close(false);
            const onBackdropClick = (event) => {
                if (event.target === backdrop) close(false);
            };
            const onKeydown = (event) => {
                if (event.key === "Enter" && input.value === required) {
                    event.preventDefault();
                    close(true);
                }
            };
            const onDocumentKeydown = (event) => {
                if (event.key === "Escape" && backdrop.classList.contains("active")) {
                    event.preventDefault();
                    close(false);
                }
            };
            messageNode.textContent = String(message || "");
            label.textContent = `Type "${required}" to confirm.`;
            input.value = "";
            submit.disabled = true;
            input.addEventListener("input", onInput);
            input.addEventListener("keydown", onKeydown);
            submit.addEventListener("click", onSubmit);
            cancel.addEventListener("click", onCancel);
            backdrop.addEventListener("click", onBackdropClick);
            document.addEventListener("keydown", onDocumentKeydown);
            backdrop.classList.add("active");
            window.requestAnimationFrame(() => input.focus());
        });
    };

    window.setAppTimezoneCache = window.setAppTimezoneCache || function setAppTimezoneCache(payload) {
        if (payload && typeof payload === "object" && !Array.isArray(payload)) {
            appTimezoneState.value = normalizeAppTimezone(payload.timezone);
            if (Object.prototype.hasOwnProperty.call(payload, "resolved_timezone")) {
                appTimezoneState.resolved = normalizeResolvedTimezone(payload.resolved_timezone);
            } else if (Object.prototype.hasOwnProperty.call(payload, "resolvedTimezone")) {
                appTimezoneState.resolved = normalizeResolvedTimezone(payload.resolvedTimezone);
            } else {
                appTimezoneState.resolved = normalizeResolvedTimezone(payload.timezone);
            }
            if (Object.prototype.hasOwnProperty.call(payload, "editable_timezone")) {
                appTimezoneState.editable = normalizeEditableTimezone(payload.editable_timezone);
            } else if (Object.prototype.hasOwnProperty.call(payload, "editableTimezone")) {
                appTimezoneState.editable = normalizeEditableTimezone(payload.editableTimezone);
            } else if (!appTimezoneState.loaded && !normalizeEditableTimezone(appTimezoneState.editable)) {
                appTimezoneState.editable =
                    normalizeEditableTimezone(payload.timezone || payload.resolved_timezone || payload.resolvedTimezone) ||
                    defaultEditableTimezone(appTimezoneState.value);
            }
        } else {
            const timezone = normalizeAppTimezone(payload);
            appTimezoneState.value = timezone;
            appTimezoneState.resolved = normalizeResolvedTimezone(timezone);
            appTimezoneState.editable = defaultEditableTimezone(timezone);
        }
        appTimezoneState.loaded = true;
        return {
            timezone: appTimezoneState.value,
            resolved_timezone: appTimezoneState.resolved,
            resolvedTimezone: appTimezoneState.resolved,
            editable_timezone: appTimezoneState.editable,
            editableTimezone: appTimezoneState.editable
        };
    };

    window.getAppTimezoneLabel = window.getAppTimezoneLabel || function getAppTimezoneLabel() {
        return normalizeAppTimezone(appTimezoneState.value);
    };

    window.getAppTimezoneResolved = window.getAppTimezoneResolved || function getAppTimezoneResolved() {
        return normalizeResolvedTimezone(appTimezoneState.resolved);
    };

    window.ensureAppTimezoneLoaded = window.ensureAppTimezoneLoaded || async function ensureAppTimezoneLoaded(force = false) {
        if (appTimezoneState.loaded && !force) {
            return {
                timezone: appTimezoneState.value,
                resolved_timezone: appTimezoneState.resolved,
                resolvedTimezone: appTimezoneState.resolved,
                editable_timezone: appTimezoneState.editable,
                editableTimezone: appTimezoneState.editable
            };
        }
        if (appTimezoneState.loadingPromise && !force) {
            return appTimezoneState.loadingPromise;
        }
        appTimezoneState.loadingPromise = (async () => {
            try {
                const res = await fetch("/api/app-settings/timezone", { cache: "no-store" });
                if (!res.ok) {
                    throw new Error(`HTTP ${res.status}`);
                }
                const data = await res.json().catch(() => ({}));
                return window.setAppTimezoneCache(data);
            } catch (err) {
                console.error("Failed to load app timezone:", err);
                if (!appTimezoneState.loaded) {
                    appTimezoneState.value = normalizeAppTimezone(appTimezoneState.value || "UTC");
                    appTimezoneState.resolved = normalizeResolvedTimezone(appTimezoneState.resolved || appTimezoneState.value);
                    appTimezoneState.editable = normalizeEditableTimezone(appTimezoneState.editable) || defaultEditableTimezone(appTimezoneState.value);
                    appTimezoneState.loaded = true;
                }
                return {
                    timezone: appTimezoneState.value,
                    resolved_timezone: appTimezoneState.resolved,
                    resolvedTimezone: appTimezoneState.resolved,
                    editable_timezone: appTimezoneState.editable,
                    editableTimezone: appTimezoneState.editable
                };
            } finally {
                appTimezoneState.loadingPromise = null;
            }
        })();
        return appTimezoneState.loadingPromise;
    };

    window.formatAppTimestamp = window.formatAppTimestamp || function formatAppTimestamp(value, options = {}) {
        const fallback = String(value ?? "").trim();
        const parsed = parseTimestamp(value);
        const timezone = window.getAppTimezoneLabel();
        const resolvedTimezone = window.getAppTimezoneResolved ? window.getAppTimezoneResolved() : timezone;
        if (!parsed) {
            return {
                primary: options.preformattedPrimary || fallback || "-",
                secondary: "",
                title: options.preformattedTitle || fallback || "",
                timezone
            };
        }

        let primary = options.preformattedPrimary || fallback || parsed.toISOString();
        let utcValue = parsed.toISOString();
        if (!options.preformattedPrimary && resolvedTimezone) {
            try {
                primary = buildTimestampFormatter(timezoneForIntl(resolvedTimezone)).format(parsed);
            } catch (err) {
                console.error("Failed to format timestamp in app timezone:", err);
            }
        }
        try {
            utcValue = buildTimestampFormatter("UTC").format(parsed);
        } catch (_) {
            utcValue = parsed.toISOString();
        }
        return {
            primary,
            secondary: options.preformattedSecondary || (options.includeUTC ? `UTC: ${utcValue}` : ""),
            title: options.preformattedTitle || (options.includeUTC || options.titleUTC ? `UTC: ${utcValue}` : utcValue),
            timezone
        };
    };
}());
