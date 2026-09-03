(function initAdminSessionIPReveal(root, factory) {
    const api = factory(root);
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.AdminSessionIPReveal = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function adminSessionIPRevealFactory(root) {
    "use strict";

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

    function normalizeVisibleSeconds(value) {
        return Math.min(30, Math.max(1, Number(value) || 30));
    }

    function remainingVisibilitySeconds(expiresAt, now = Date.now()) {
        return Math.max(0, Math.ceil((expiresAt - now) / 1000));
    }

    function lockSessionIPRevealBackground(modal) {
        sessionIPReveal.background = Array.from(root.document.body.children)
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
        root.document.body.classList.add("modal-open");
    }

    function unlockSessionIPRevealBackground() {
        sessionIPReveal.background.forEach(({ element, inert, ariaHidden }) => {
            element.inert = inert;
            if (ariaHidden === null) element.removeAttribute("aria-hidden");
            else element.setAttribute("aria-hidden", ariaHidden);
        });
        sessionIPReveal.background = [];
        root.document.body.classList.remove("modal-open");
    }

    function closeSessionIPRevealModal(options = {}) {
        const modal = root.document.getElementById("session-ip-reveal-modal");
        const password = root.document.getElementById("session-ip-reveal-password");
        const error = root.document.getElementById("session-ip-reveal-error");
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
        const modal = root.document.getElementById("session-ip-reveal-modal");
        const password = root.document.getElementById("session-ip-reveal-password");
        if (!modal || !password || !sessionID) return;
        hideTemporarySessionIPReveal();
        modal.dataset.sessionId = sessionID;
        sessionIPReveal.trigger = trigger || null;
        lockSessionIPRevealBackground(modal);
        modal.classList.add("active");
        root.requestAnimationFrame(() => password.focus());
    }

    function hideTemporarySessionIPReveal() {
        root.clearInterval(sessionIPReveal.intervalID);
        root.clearTimeout(sessionIPReveal.timeoutID);
        const sessionID = sessionIPReveal.sessionID;
        if (sessionID) {
            const escapedSessionID = root.CSS.escape(sessionID);
            const ipNode = root.document.querySelector(`[data-session-ip-id="${escapedSessionID}"]`);
            const visibility = root.document.querySelector(`[data-session-ip-visibility="${escapedSessionID}"]`);
            const button = root.document.querySelector(`button[data-hide-session-ip="${escapedSessionID}"]`);
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
        const remaining = remainingVisibilitySeconds(sessionIPReveal.expiresAt);
        if (remaining === 0) {
            hideTemporarySessionIPReveal();
            return;
        }
        const visibility = root.document.querySelector(
            `[data-session-ip-visibility="${root.CSS.escape(sessionIPReveal.sessionID)}"]`
        );
        if (visibility) visibility.textContent = `Full IP visible for ${remaining} second${remaining === 1 ? "" : "s"}`;
    }

    function showTemporarySessionIPReveal(sessionID, fullIP, requestedSeconds) {
        hideTemporarySessionIPReveal();
        const escapedSessionID = root.CSS.escape(sessionID);
        const ipNode = root.document.querySelector(`[data-session-ip-id="${escapedSessionID}"]`);
        const visibility = root.document.querySelector(`[data-session-ip-visibility="${escapedSessionID}"]`);
        const button = root.document.querySelector(`button[data-reveal-session-id="${escapedSessionID}"]`);
        if (!ipNode || !visibility || !button || !fullIP) return false;
        const seconds = normalizeVisibleSeconds(requestedSeconds);
        sessionIPReveal.sessionID = sessionID;
        sessionIPReveal.expiresAt = Date.now() + (seconds * 1000);
        ipNode.textContent = fullIP;
        visibility.hidden = false;
        delete button.dataset.revealSessionId;
        button.dataset.hideSessionIp = sessionID;
        button.textContent = "Hide now";
        button.disabled = false;
        updateTemporarySessionIPReveal();
        sessionIPReveal.intervalID = root.setInterval(updateTemporarySessionIPReveal, 250);
        sessionIPReveal.timeoutID = root.setTimeout(hideTemporarySessionIPReveal, seconds * 1000);
        return true;
    }

    function handleSessionIPRevealModalKeydown(event) {
        const modal = root.document.getElementById("session-ip-reveal-modal");
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
        if (event.shiftKey && root.document.activeElement === first) {
            event.preventDefault();
            last.focus();
        } else if (!event.shiftKey && root.document.activeElement === last) {
            event.preventDefault();
            first.focus();
        }
    }

    async function submitSessionIPReveal(event) {
        event.preventDefault();
        const modal = root.document.getElementById("session-ip-reveal-modal");
        const password = root.document.getElementById("session-ip-reveal-password");
        const error = root.document.getElementById("session-ip-reveal-error");
        const submit = root.document.getElementById("session-ip-reveal-submit");
        const sessionID = modal?.dataset.sessionId || "";
        if (!sessionID || !password?.value) {
            if (error) error.textContent = "Current password is required.";
            return;
        }
        if (submit) submit.disabled = true;
        const requestID = ++sessionIPReveal.requestID;
        const requestController = new root.AbortController();
        sessionIPReveal.requestController?.abort();
        sessionIPReveal.requestController = requestController;
        try {
            const request = typeof root.__nativeFetch === "function" ? root.__nativeFetch : root.fetch;
            const res = await request(`/api/auth/sessions/${encodeURIComponent(sessionID)}/reveal-ip`, {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ current_password: password.value }),
                cache: "no-store",
                signal: requestController.signal
            });
            if (requestID !== sessionIPReveal.requestID || !modal?.classList.contains("active")) return;
            if (!res.ok) {
                if (error) error.textContent = await root.parseErrorResponse(res, "Failed to reveal session IP.");
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

    return Object.freeze({
        closeSessionIPRevealModal,
        handleSessionIPRevealModalKeydown,
        hideTemporarySessionIPReveal,
        normalizeVisibleSeconds,
        openSessionIPRevealModal,
        remainingVisibilitySeconds,
        submitSessionIPReveal
    });
}));
