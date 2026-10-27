// Browser effects for AdminAccountAdapter; accepted state and command plans stay in the injected interaction owner.
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
            adminPageInteraction,
            hideTemporarySessionIPReveal,
            renderAdminWorkspace,
            beginAdminSnapshot,
            escapeHtml,
            parseErrorResponse,
            beginAdminCommand,
            finishAdminCommand,
            encodeURIComponent,
            openSessionIPRevealModal,
        } = options;
        function setAuthPasswordFeedback(
            statusMessage,
            errorMessage,
            tone = "success",
        ) {
            const success = document.getElementById("auth-password-status");
            const error = document.getElementById("auth-password-error");
            if (success) {
                success.textContent = statusMessage || "";
                success.classList.toggle(
                    "form-feedback-success",
                    tone === "success",
                );
                success.classList.toggle(
                    "form-feedback-warning",
                    tone === "warning",
                );
            }
            if (error) error.textContent = errorMessage || "";
        }

        function passwordPolicyChecks(
            value,
            policy = adminPageView().account.passwordPolicy,
        ) {
            const password = String(value || "");
            const length = Array.from(password).length;
            return {
                length:
                    length >= policy.minLength && length <= policy.maxLength,
                letter: !policy.requiresLetter || /[A-Za-z]/.test(password),
                digit: !policy.requiresDigit || /[0-9]/.test(password),
            };
        }

        function passwordDraftFacts() {
            const currentPassword =
                document.getElementById("auth-current-password")?.value || "";
            const newPassword =
                document.getElementById("auth-new-password")?.value || "";
            const confirmPassword =
                document.getElementById("auth-confirm-password")?.value || "";
            const checks = passwordPolicyChecks(newPassword);
            return {
                hasCurrentPassword: currentPassword.length > 0,
                hasNewPassword: newPassword.length > 0,
                passwordsMatch: newPassword === confirmPassword,
                passwordValid: Object.values(checks).every(Boolean),
                invalidateOtherSessions: Boolean(
                    document.getElementById("auth-password-invalidate-others")
                        ?.checked,
                ),
                checks,
            };
        }

        function renderAuthPasswordControls() {
            const policy = adminPageView().account.passwordPolicy;
            const newInput = document.getElementById("auth-new-password");
            const confirmInput = document.getElementById(
                "auth-confirm-password",
            );
            const save = document.getElementById("auth-password-save");
            const facts = passwordDraftFacts();
            [newInput, confirmInput].forEach((input) => {
                if (!input) return;
                input.minLength = policy.minLength;
                input.maxLength = policy.maxLength;
            });
            const length = document.querySelector(
                '[data-password-requirement="length"]',
            );
            const letter = document.querySelector(
                '[data-password-requirement="letter"]',
            );
            const digit = document.querySelector(
                '[data-password-requirement="digit"]',
            );
            if (length)
                length.textContent = `${policy.minLength}–${policy.maxLength} characters`;
            if (letter) {
                letter.textContent = policy.requiresLetter
                    ? "At least one letter"
                    : "Letters are optional";
                letter.hidden = !policy.requiresLetter;
            }
            if (digit) {
                digit.textContent = policy.requiresDigit
                    ? "At least one digit"
                    : "Digits are optional";
                digit.hidden = !policy.requiresDigit;
            }
            const hasDraft = Boolean(newInput?.value);
            Object.entries(facts.checks).forEach(([name, met]) => {
                document
                    .querySelector(`[data-password-requirement="${name}"]`)
                    ?.classList.toggle("is-met", hasDraft && met);
            });
            const plan = adminPageInteraction.planCommand(
                "changePassword",
                facts,
            );
            if (save) {
                save.disabled = !plan.enabled;
                save.title = plan.enabled ? "" : plan.reason || "";
            }
        }

        function togglePasswordVisibility(event) {
            const button = event.currentTarget;
            const input = document.getElementById(
                button.dataset.passwordToggle || "",
            );
            if (!input) return;
            const showing = input.type === "text";
            input.type = showing ? "password" : "text";
            button.textContent = showing ? "Show" : "Hide";
            button.setAttribute("aria-pressed", String(!showing));
            const fieldName =
                input.id === "auth-current-password"
                    ? "current password"
                    : input.id === "auth-new-password"
                      ? "new password"
                      : "password confirmation";
            button.setAttribute(
                "aria-label",
                `${showing ? "Show" : "Hide"} ${fieldName}`,
            );
            input.focus();
        }

        function updatePasswordCapsLockWarning(event) {
            const warning = document.getElementById(
                "auth-password-caps-warning",
            );
            if (!warning || typeof event.getModifierState !== "function")
                return;
            warning.hidden = !event.getModifierState("CapsLock");
        }

        function resetPasswordEntryControls() {
            [
                "auth-current-password",
                "auth-new-password",
                "auth-confirm-password",
            ].forEach((id) => {
                const input = document.getElementById(id);
                const toggle = document.querySelector(
                    `[data-password-toggle="${id}"]`,
                );
                if (input) {
                    input.value = "";
                    input.type = "password";
                }
                if (toggle) {
                    toggle.textContent = "Show";
                    toggle.setAttribute("aria-pressed", "false");
                    const fieldName =
                        id === "auth-current-password"
                            ? "current password"
                            : id === "auth-new-password"
                              ? "new password"
                              : "password confirmation";
                    toggle.setAttribute("aria-label", `Show ${fieldName}`);
                }
            });
            const warning = document.getElementById(
                "auth-password-caps-warning",
            );
            if (warning) warning.hidden = true;
            renderAuthPasswordControls();
        }

        function formatSessionTime(value) {
            const raw = String(value || "").trim();
            if (!raw) return "Unavailable";
            const parsed = new Date(raw);
            if (Number.isNaN(parsed.getTime())) return raw;
            return parsed.toLocaleString([], {
                dateStyle: "medium",
                timeStyle: "short",
            });
        }

        function createAuthSessionCard(session) {
            const item = document.createElement("article");
            item.className = "session-item";
            const head = document.createElement("div");
            head.className = "session-item-head";
            const title = document.createElement("p");
            title.className = "session-item-title";
            title.textContent =
                session.clientLabel || "Unknown browser · Unknown OS";
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
                ["Expires", formatSessionTime(session.expiresAt)],
            ].forEach(([label, value, kind]) => {
                const group = document.createElement("div");
                const term = document.createElement("dt");
                const description = document.createElement("dd");
                term.textContent = label;
                description.textContent = value;
                if (kind === "ip") {
                    description.dataset.sessionIpId = session.id;
                    description.dataset.maskedIp =
                        session.clientIP || "Unavailable";
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
            revokeButton.textContent = session.current
                ? "Logout This Session"
                : "Revoke Session";
            actions.append(revealButton, visibility, revokeButton);
            item.appendChild(actions);
            return item;
        }

        function renderAuthSessions() {
            hideTemporarySessionIPReveal();
            const view = adminPageView();
            const status = document.getElementById("auth-session-status");
            const currentList = document.getElementById(
                "auth-current-session-list",
            );
            const otherList = document.getElementById(
                "auth-other-session-list",
            );
            const currentGroup = document.getElementById(
                "auth-current-session-group",
            );
            const otherGroup = document.getElementById(
                "auth-other-session-group",
            );
            const otherTitle = document.getElementById(
                "auth-other-session-title",
            );
            const showAll = document.getElementById("auth-sessions-show-all");
            const clearOthers = document.getElementById(
                "auth-sessions-clear-others",
            );
            if (status)
                status.textContent = `${view.account.sessionCount} active session${view.account.sessionCount === 1 ? "" : "s"}.`;
            if (clearOthers)
                clearOthers.disabled = view.account.otherSessions.length === 0;
            if (!currentList || !otherList) return;
            currentList.replaceChildren();
            otherList.replaceChildren();
            if (view.account.currentSession)
                currentList.appendChild(
                    createAuthSessionCard(view.account.currentSession),
                );
            view.account.otherSessions.forEach((session) =>
                otherList.appendChild(createAuthSessionCard(session)),
            );
            if (currentGroup)
                currentGroup.hidden = !view.account.currentSession;
            if (otherGroup)
                otherGroup.hidden = view.account.otherSessions.length === 0;
            if (otherTitle)
                otherTitle.textContent = `Other sessions (${view.account.otherSessions.length})`;
            if (showAll) {
                showAll.hidden = view.account.otherSessions.length <= 3;
                showAll.textContent = view.account.otherSessionsExpanded
                    ? "Collapse"
                    : "Show all";
            }
            otherList.classList.toggle(
                "is-expanded",
                view.account.otherSessionsExpanded,
            );
            renderAuthPasswordControls();
            renderAdminWorkspace();
        }

        async function fetchAuthSessionStatus() {
            const lifetime = lifecycle.capture();

            hideTemporarySessionIPReveal();
            const status = document.getElementById("auth-session-status");
            if (!status) return;
            const requestID = beginAdminSnapshot("account");
            try {
                const res = await fetch("/api/auth/sessions");
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    lifecycle.discardResponse(res);
                    adminPageInteraction.dispatch({
                        type: "snapshotFailed",
                        stream: "account",
                        requestID,
                        error: "Session status unavailable.",
                    });
                    status.textContent = "Session status unavailable.";
                    renderAdminWorkspace();
                    return;
                }
                const data = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                adminPageInteraction.dispatch({
                    type: "accountSnapshotReceived",
                    requestID,
                    receivedAt: new Date().toISOString(),
                    data,
                });
                renderAuthSessions();
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                console.error("Failed to fetch session status:", err);
                adminPageInteraction.dispatch({
                    type: "snapshotFailed",
                    stream: "account",
                    requestID,
                    error: "Session status request failed.",
                });
                status.textContent = "Session status request failed.";
                renderAdminWorkspace();
            }
        }

        const adminActivityLabels = Object.freeze({
            "auth.login": "Admin sign-in",
            "auth.session.ip_reveal": "Session IP revealed",
            "auth.password.change": "Admin password changed",
            "app_settings.timezone": "App timezone changed",
            "backup.restore": "Backup restored",
            "metrics.token.rotate": "Metrics token rotated",
            "metrics.token.clear": "Metrics token disabled",
            "notifications.settings": "Notification settings changed",
            "notifications.test": "Notification test sent",
            "update_policy.create": "Policy created",
            "update_policy.update": "Policy updated",
            "update_policy.delete": "Policy deleted",
            "update_policy.override": "Policy override changed",
            "update_policy.settings": "Policy settings changed",
        });

        function adminActivityLabel(action) {
            const normalized = String(action || "").trim();
            return (
                adminActivityLabels[normalized] ||
                normalized
                    .split(".")
                    .map((part) =>
                        part
                            ? `${part.charAt(0).toUpperCase()}${part.slice(1).replaceAll("_", " ")}`
                            : "",
                    )
                    .join(" · ")
            );
        }

        function renderRecentAdminActivity() {
            const view = adminPageView();
            const list = document.getElementById("admin-activity-list");
            const message = document.getElementById("admin-activity-message");
            if (!list || !message) return;
            const items = view.activity?.items || [];
            const stream = view.streams?.activity || {};
            list.innerHTML = items
                .map((item) => {
                    const outcome = String(
                        item.status || "unknown",
                    ).toLowerCase();
                    const outcomeClass = [
                        "success",
                        "succeeded",
                        "completed",
                    ].includes(outcome)
                        ? "is-success"
                        : ["failure", "failed", "error"].includes(outcome)
                          ? "is-failure"
                          : "";
                    const displayTime =
                        item.createdAtDisplay ||
                        item.createdAt ||
                        "Time unavailable";
                    return `<article class="admin-activity-item" role="listitem">
            <div class="admin-activity-primary">
                <span class="admin-activity-action">${escapeHtml(adminActivityLabel(item.action))}</span>
                <time class="admin-activity-time" datetime="${escapeHtml(item.createdAt)}">${escapeHtml(displayTime)}</time>
            </div>
            <div class="admin-activity-context">
                <span class="admin-activity-actor">Actor: ${escapeHtml(item.actor)}</span>
                <span class="muted">Application Time</span>
            </div>
            <span class="admin-activity-outcome ${outcomeClass}">${escapeHtml(outcome || "unknown")}</span>
            <a class="btn-ghost inline-btn" href="/api/reports/audit/${item.id}">View detail</a>
        </article>`;
                })
                .join("");

            if (stream.status === "loading" && items.length === 0) {
                message.textContent = "Loading recent activity...";
                message.hidden = false;
            } else if (stream.status === "failed") {
                message.textContent =
                    stream.error || "Recent activity is unavailable.";
                message.hidden = false;
            } else if (stream.status === "stale") {
                message.textContent = `${stream.error || "Recent activity could not be refreshed."} Showing the last successful results.`;
                message.hidden = false;
            } else if (stream.accepted && items.length === 0) {
                message.textContent = "No recent administrative activity.";
                message.hidden = false;
            } else {
                message.textContent = "";
                message.hidden = true;
            }
        }

        async function fetchRecentAdminActivity() {
            const lifetime = lifecycle.capture();

            const requestID = beginAdminSnapshot("activity");
            renderRecentAdminActivity();
            try {
                const res = await fetch(
                    "/api/audit-events?category=admin_activity&page=1&page_size=8",
                    { cache: "no-store" },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    const error = await parseErrorResponse(
                        res,
                        "Failed to load recent activity.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    adminPageInteraction.dispatch({
                        type: "snapshotFailed",
                        stream: "activity",
                        requestID,
                        error,
                    });
                    renderRecentAdminActivity();
                    renderAdminWorkspace();
                    return;
                }
                const data = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                adminPageInteraction.dispatch({
                    type: "activitySnapshotReceived",
                    requestID,
                    receivedAt: new Date().toISOString(),
                    data,
                });
                renderRecentAdminActivity();
                renderAdminWorkspace();
            } catch (error) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    error?.name === "AbortError"
                )
                    return;
                console.error("Failed to fetch recent Admin activity:", error);
                adminPageInteraction.dispatch({
                    type: "snapshotFailed",
                    stream: "activity",
                    requestID,
                    error: "Failed to load recent activity.",
                });
                renderRecentAdminActivity();
                renderAdminWorkspace();
            }
        }

        async function clearOtherAuthSessions() {
            const lifetime = lifecycle.capture();

            const count = adminPageView().account.otherSessions.length;
            if (
                !(await window.confirmAction(
                    "Review the sessions that will be invalidated before continuing.",
                    {
                        danger: true,
                        title: "Logout other Admin sessions",
                        operation: "Invalidate all other server-side sessions",
                        resources: `${count} other signed-in browser session${count === 1 ? "" : "s"}`,
                        consequences:
                            "Other browsers will be logged out immediately. This browser remains signed in.",
                        reversibility:
                            "Not reversible. Affected users must sign in again.",
                        authentication:
                            "Your current signed-in Admin session authorizes this operation.",
                        confirmLabel: "Logout Others",
                    },
                ))
            )
                return;
            if (!lifecycle.isCurrent(lifetime)) return;
            const plan = beginAdminCommand("clearOtherSessions");
            if (!plan) return;
            try {
                const res = await fetch("/api/auth/sessions/others", {
                    method: "DELETE",
                });
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    const message = await parseErrorResponse(
                        res,
                        "Failed to clear other sessions.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    finishAdminCommand(plan, null, message, true);
                    window.notifyApp(message);
                    return;
                }
                const data = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                finishAdminCommand(plan, data, "Other sessions cleared.");
                await fetchAuthSessionStatus();
                if (!lifecycle.isCurrent(lifetime)) return;
                window.notifyApp(
                    `${Number(data.deleted_sessions || 0)} other session(s) logged out.`,
                );
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                finishAdminCommand(
                    plan,
                    null,
                    err.message || "Failed to clear other sessions.",
                    true,
                );
                window.notifyApp(
                    err.message || "Failed to clear other sessions.",
                );
            }
        }

        async function revokeAuthSession(id, current) {
            const lifetime = lifecycle.capture();

            const session = adminPageView().account.sessions.find(
                (item) => item.id === id,
            );
            if (
                !(await window.confirmAction(
                    "Review the selected session before continuing.",
                    {
                        danger: true,
                        title: current
                            ? "Logout this Admin session"
                            : "Revoke server-side session",
                        operation: current
                            ? "Invalidate the current session"
                            : "Revoke one server-side session",
                        resources: `${session?.clientLabel || "Selected browser"} · ${session?.clientIP || "IP unavailable"}`,
                        consequences: current
                            ? "This browser will return to the sign-in page immediately."
                            : "The selected browser will be logged out immediately.",
                        reversibility:
                            "Not reversible. The browser must sign in again.",
                        authentication:
                            "Your current signed-in Admin session authorizes this operation.",
                        confirmLabel: current ? "Logout" : "Revoke Session",
                    },
                ))
            )
                return;
            if (!lifecycle.isCurrent(lifetime)) return;
            const plan = beginAdminCommand("revokeSession", { id });
            if (!plan) return;
            try {
                const res = await fetch(
                    `/api/auth/sessions/${encodeURIComponent(plan.payload.id)}`,
                    { method: "DELETE" },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    const message = await parseErrorResponse(
                        res,
                        "Failed to revoke session.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    finishAdminCommand(plan, null, message, true);
                    window.notifyApp(message);
                    return;
                }
                lifecycle.discardResponse(res);
                finishAdminCommand(plan, null, "Session revoked.");
                if (current) {
                    window.location.assign("/login");
                    return;
                }
                await fetchAuthSessionStatus();
                if (!lifecycle.isCurrent(lifetime)) return;
                window.notifyApp("Session revoked.");
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                finishAdminCommand(
                    plan,
                    null,
                    err.message || "Failed to revoke session.",
                    true,
                );
                window.notifyApp(err.message || "Failed to revoke session.");
            }
        }

        function handleAuthSessionListClick(event) {
            const hideButton = event.target.closest(
                "button[data-hide-session-ip]",
            );
            if (hideButton) {
                hideTemporarySessionIPReveal();
                hideButton.focus();
                return;
            }
            const revealButton = event.target.closest(
                "button[data-reveal-session-id]",
            );
            if (revealButton) {
                openSessionIPRevealModal(
                    revealButton.dataset.revealSessionId || "",
                    revealButton,
                );
                return;
            }
            const button = event.target.closest("button[data-session-id]");
            if (!button) return;
            revokeAuthSession(
                button.dataset.sessionId || "",
                button.dataset.currentSession === "true",
            );
        }

        async function changeAdminPassword() {
            const lifetime = lifecycle.capture();

            const currentInput = document.getElementById(
                "auth-current-password",
            );
            const newInput = document.getElementById("auth-new-password");
            const confirmInput = document.getElementById(
                "auth-confirm-password",
            );
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
                    const rejected = adminPageInteraction.planCommand(
                        "changePassword",
                        facts,
                    );
                    setAuthPasswordFeedback(
                        "",
                        rejected.reason || "Review the password fields.",
                    );
                    return;
                }
                const res = await fetch("/api/auth/password", {
                    method: "PUT",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({
                        current_password: currentPassword,
                        new_password: newPassword,
                        confirm_password: confirmPassword,
                        invalidate_other_sessions:
                            plan.payload.invalidateOtherSessions,
                    }),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    const message = await parseErrorResponse(
                        res,
                        "Failed to change password.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    finishAdminCommand(plan, null, message, true);
                    setAuthPasswordFeedback("", message);
                    return;
                }
                const data = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                const invalidated = Math.max(
                    0,
                    Number(data.invalidated_sessions) || 0,
                );
                const preserved = Math.max(
                    0,
                    Number(data.preserved_sessions) || 0,
                );
                const partial = data.outcome === "partial_failure";
                const message = partial
                    ? `Password changed, but other sessions could not be invalidated. ${preserved} active session${preserved === 1 ? "" : "s"} preserved.`
                    : data.invalidation_requested
                      ? `Password changed. ${invalidated} other session${invalidated === 1 ? "" : "s"} invalidated; ${preserved} active session${preserved === 1 ? "" : "s"} preserved.`
                      : `Password changed. 0 sessions invalidated; ${preserved} active session${preserved === 1 ? "" : "s"} preserved.`;
                finishAdminCommand(plan, data, message);
                resetPasswordEntryControls();
                setAuthPasswordFeedback(
                    message,
                    "",
                    partial ? "warning" : "success",
                );
                if (data.invalidation_requested) await fetchAuthSessionStatus();
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                finishAdminCommand(
                    plan,
                    null,
                    err.message || "Failed to change password.",
                    true,
                );
                setAuthPasswordFeedback(
                    "",
                    err.message || "Failed to change password.",
                );
            } finally {
                if (!lifecycle.isCurrent(lifetime)) return;
                renderAuthPasswordControls();
            }
        }

        async function clearAuthSessions() {
            const lifetime = lifecycle.capture();

            const count = adminPageView().account.sessionCount;
            if (
                !(await window.confirmTypedAction(
                    "Type the confirmation text to invalidate every session.",
                    "LOGOUT ALL",
                    {
                        title: "Logout every Admin session",
                        operation: "Invalidate all server-side sessions",
                        resources: `${count} signed-in browser session${count === 1 ? "" : "s"}`,
                        consequences:
                            "Every browser, including this one, will be logged out immediately.",
                        reversibility:
                            "Not reversible. Every user must sign in again.",
                        authentication:
                            "Typed confirmation is required; the current Admin session authorizes the request.",
                        confirmLabel: "Logout All Sessions",
                    },
                ))
            ) {
                return;
            }
            if (!lifecycle.isCurrent(lifetime)) return;
            const plan = beginAdminCommand("clearSessions");
            if (!plan) return;
            try {
                const res = await fetch("/api/auth/sessions", {
                    method: "DELETE",
                });
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    const message = await parseErrorResponse(
                        res,
                        "Failed to clear sessions.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    finishAdminCommand(plan, null, message, true);
                    window.notifyApp(message);
                    return;
                }
                lifecycle.discardResponse(res);
                finishAdminCommand(plan, { count: 0 }, "Sessions cleared.");
                adminPageInteraction.dispatch({ type: "metricsTokenHidden" });
                window.location.assign("/login");
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                finishAdminCommand(
                    plan,
                    null,
                    err.message || "Failed to clear sessions.",
                    true,
                );
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

        function formatMetricsCredentialTime(value, emptyLabel) {
            const parsed = new Date(String(value || ""));
            if (!value || Number.isNaN(parsed.getTime())) return emptyLabel;
            if (window.formatAppTimestamp) {
                return window.formatAppTimestamp(value, { titleUTC: true })
                    .primary;
            }
            return parsed.toISOString();
        }

        function renderMetricsCredentialLifecycle() {
            const view = adminPageView().metrics;
            const state = document.getElementById(
                "metrics-token-lifecycle-state",
            );
            const status = document.getElementById("metrics-token-status");
            const created = document.getElementById("metrics-token-created");
            const rotated = document.getElementById("metrics-token-rotated");
            const lastUsed = document.getElementById("metrics-token-last-used");
            const lastOrigin = document.getElementById(
                "metrics-token-last-origin",
            );
            const stalePolicy = document.getElementById(
                "metrics-token-stale-policy",
            );
            if (!status) return;

            const labels = {
                disabled: "Disabled",
                unknown: "Usage unknown",
                never_used: "Never used",
                current: "Current",
                stale: "Stale",
            };
            if (state) {
                state.dataset.state = view.lifecycleState;
                state.textContent =
                    labels[view.lifecycleState] || "Unavailable";
            }
            if (created)
                created.textContent = formatMetricsCredentialTime(
                    view.createdAt,
                    "Unavailable",
                );
            if (rotated)
                rotated.textContent = formatMetricsCredentialTime(
                    view.rotatedAt,
                    "Never",
                );
            if (lastUsed)
                lastUsed.textContent = formatMetricsCredentialTime(
                    view.lastUsedAt,
                    "Never",
                );
            if (lastOrigin)
                lastOrigin.textContent =
                    view.lastUsedOriginMasked || "Unavailable";
            if (stalePolicy) {
                stalePolicy.textContent = `A token is considered stale after ${view.staleAfterDays} days without successful use. Stale tokens are never disabled automatically.`;
            }

            if (!view.enabled) {
                status.textContent = "Metrics access is disabled.";
            } else if (view.lifecycleState === "unknown") {
                status.textContent =
                    "The token predates lifecycle tracking. Its usage is unknown until the next successful metrics request or rotation.";
            } else if (view.neverUsed) {
                status.textContent =
                    "The token is enabled but has never completed a successful metrics request.";
            } else if (view.stale) {
                status.textContent = `No successful metrics request has been recorded for at least ${view.staleAfterDays} days. Review scraper health; access remains enabled.`;
            } else {
                status.textContent = `The token has been used successfully within the last ${view.staleAfterDays} days.`;
            }
        }

        async function fetchMetricsTokenStatus(resetReveal = true) {
            const lifetime = lifecycle.capture();

            const status = document.getElementById("metrics-token-status");
            if (!status) return;
            if (resetReveal) {
                showMetricsTokenOnce("");
            }
            const requestID = beginAdminSnapshot("metrics");
            try {
                const res = await fetch("/api/metrics/token");
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    lifecycle.discardResponse(res);
                    adminPageInteraction.dispatch({
                        type: "snapshotFailed",
                        stream: "metrics",
                        requestID,
                        error: "Metrics token status: unknown",
                    });
                    status.textContent = "Metrics token status: unknown";
                    renderAdminWorkspace();
                    return;
                }
                const data = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                adminPageInteraction.dispatch({
                    type: "metricsSnapshotReceived",
                    requestID,
                    receivedAt: new Date().toISOString(),
                    data,
                });
                renderMetricsCredentialLifecycle();
                renderAdminWorkspace();
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                console.error("Failed to fetch metrics token status:", err);
                adminPageInteraction.dispatch({
                    type: "snapshotFailed",
                    stream: "metrics",
                    requestID,
                    error: "Metrics token status: request failed",
                });
                status.textContent = "Metrics token status: request failed";
                renderAdminWorkspace();
            }
        }

        async function rotateMetricsToken(askConfirm) {
            const lifetime = lifecycle.capture();

            if (
                askConfirm &&
                !(await window.confirmTypedAction(
                    "Type the confirmation text to replace the current Metrics API credential.",
                    "ROTATE TOKEN",
                    {
                        title: "Rotate Metrics API token",
                        operation: "Replace the active Metrics API token",
                        resources:
                            "The current /metrics credential and every scraper using it",
                        consequences:
                            "Existing scrapers will fail until they receive the new one-time token.",
                        reversibility: "The previous token cannot be restored.",
                        authentication:
                            "Typed confirmation is required; the new token is displayed once.",
                        confirmLabel: "Rotate Token",
                    },
                ))
            ) {
                return;
            }
            if (!lifecycle.isCurrent(lifetime)) return;
            const plan = beginAdminCommand("rotateMetricsToken");
            if (!plan) return;
            try {
                const res = await fetch("/api/metrics/token", {
                    method: "POST",
                });
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    const message = await parseErrorResponse(
                        res,
                        "Failed to rotate metrics token.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    finishAdminCommand(plan, null, message, true);
                    window.notifyApp(message);
                    return;
                }
                const data = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                const token =
                    data && typeof data.token === "string" ? data.token : "";
                if (!token) {
                    finishAdminCommand(
                        plan,
                        data,
                        "Token rotation succeeded but no token was returned.",
                        true,
                    );
                    window.notifyApp(
                        "Token rotation succeeded but no token was returned.",
                    );
                    return;
                }
                finishAdminCommand(plan, data, "Metrics token rotated.");
                showMetricsTokenOnce(token);
                fetchMetricsTokenStatus(false);
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                console.error("Failed to rotate metrics token:", err);
                finishAdminCommand(
                    plan,
                    null,
                    "Failed to rotate metrics token.",
                    true,
                );
                window.notifyApp("Failed to rotate metrics token.");
            }
        }

        async function disableMetricsToken() {
            const lifetime = lifecycle.capture();

            if (
                !(await window.confirmTypedAction(
                    "Type the confirmation text to disable authenticated Metrics API access.",
                    "DISABLE METRICS",
                    {
                        title: "Disable Metrics API token",
                        operation: "Disable the active Metrics API token",
                        resources:
                            "The /metrics endpoint and every configured scraper",
                        consequences:
                            "Metrics collection stops immediately for clients using this token.",
                        reversibility:
                            "Access can be restored only by generating a new token.",
                        authentication:
                            "Typed confirmation is required; the current Admin session authorizes the request.",
                        confirmLabel: "Disable Token",
                    },
                ))
            ) {
                return;
            }
            if (!lifecycle.isCurrent(lifetime)) return;
            const plan = beginAdminCommand("disableMetricsToken");
            if (!plan) return;
            try {
                const res = await fetch("/api/metrics/token", {
                    method: "DELETE",
                });
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    const message = await parseErrorResponse(
                        res,
                        "Failed to disable metrics token.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    finishAdminCommand(plan, null, message, true);
                    window.notifyApp(message);
                    return;
                }
                lifecycle.discardResponse(res);
                finishAdminCommand(
                    plan,
                    { enabled: false },
                    "Metrics token disabled.",
                );
                showMetricsTokenOnce("");
                fetchMetricsTokenStatus();
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                console.error("Failed to disable metrics token:", err);
                finishAdminCommand(
                    plan,
                    null,
                    "Failed to disable metrics token.",
                    true,
                );
                window.notifyApp("Failed to disable metrics token.");
            }
        }

        async function copyMetricsToken() {
            const lifetime = lifecycle.capture();

            const token = adminPageView().metrics.revealedToken;
            if (!token) {
                window.notifyApp("No token to copy.");
                return;
            }
            try {
                await navigator.clipboard.writeText(token);
                if (!lifecycle.isCurrent(lifetime)) return;
                window.notifyApp("Metrics token copied.");
            } catch (_) {
                if (!lifecycle.isCurrent(lifetime) || _?.name === "AbortError")
                    return;
                window.notifyApp(
                    "Failed to copy token. Copy it manually from the box.",
                );
            }
        }
        return Object.freeze({
            init: lifecycle.init,
            destroy: lifecycle.destroy,
            setAuthPasswordFeedback: (...args) =>
                lifecycle.active ? setAuthPasswordFeedback(...args) : undefined,
            renderAuthPasswordControls: (...args) =>
                lifecycle.active
                    ? renderAuthPasswordControls(...args)
                    : undefined,
            togglePasswordVisibility: (...args) =>
                lifecycle.active
                    ? togglePasswordVisibility(...args)
                    : undefined,
            updatePasswordCapsLockWarning: (...args) =>
                lifecycle.active
                    ? updatePasswordCapsLockWarning(...args)
                    : undefined,
            formatSessionTime: (...args) =>
                lifecycle.active ? formatSessionTime(...args) : undefined,
            renderAuthSessions: (...args) =>
                lifecycle.active ? renderAuthSessions(...args) : undefined,
            fetchAuthSessionStatus: (...args) =>
                lifecycle.active ? fetchAuthSessionStatus(...args) : undefined,
            fetchRecentAdminActivity: (...args) =>
                lifecycle.active
                    ? fetchRecentAdminActivity(...args)
                    : undefined,
            clearOtherAuthSessions: (...args) =>
                lifecycle.active ? clearOtherAuthSessions(...args) : undefined,
            handleAuthSessionListClick: (...args) =>
                lifecycle.active
                    ? handleAuthSessionListClick(...args)
                    : undefined,
            changeAdminPassword: (...args) =>
                lifecycle.active ? changeAdminPassword(...args) : undefined,
            clearAuthSessions: (...args) =>
                lifecycle.active ? clearAuthSessions(...args) : undefined,
            fetchMetricsTokenStatus: (...args) =>
                lifecycle.active ? fetchMetricsTokenStatus(...args) : undefined,
            rotateMetricsToken: (...args) =>
                lifecycle.active ? rotateMetricsToken(...args) : undefined,
            disableMetricsToken: (...args) =>
                lifecycle.active ? disableMetricsToken(...args) : undefined,
            copyMetricsToken: (...args) =>
                lifecycle.active ? copyMetricsToken(...args) : undefined,
        });
    }
    root.AdminAccountAdapter = Object.freeze({ create });
})(globalThis);
