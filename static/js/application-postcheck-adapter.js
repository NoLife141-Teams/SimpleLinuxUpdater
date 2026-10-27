(function (root) {
    "use strict";
    function mount({ store, onChange = () => {} }) {
        const area = document.getElementById(
            "admin-section-application-checks",
        );
        const lifecycle = new AbortController();
        const requests = new Set();
        const el = (id) => document.getElementById(`postcheck-${id}`);
        const view = () => store.getView().postchecks;
        const escape = (value) =>
            String(value ?? "").replace(
                /[&<>"']/g,
                (c) =>
                    ({
                        "&": "&amp;",
                        "<": "&lt;",
                        ">": "&gt;",
                        '"': "&quot;",
                        "'": "&#39;",
                    })[c],
            );
        function dispatch(type, data = {}, redraw = true) {
            store.dispatch({ type: `postcheck:${type}`, ...data });
            if (redraw) render();
            onChange();
        }
        function render() {
            const state = view();
            el("profile-select").innerHTML = state.draft.profiles
                .map(
                    (p) =>
                        `<option value="${escape(p.id)}">${escape(p.name)} (revision ${Number(p.revision)})</option>`,
                )
                .join("");
            el("profile-select").value = state.selectedID;
            const p = state.draft.profiles.find(
                (item) => item.id === state.selectedID,
            );
            el("editor").hidden = !p;
            el("editor").disabled = state.busy || !p;
            el("name").value = p?.name || "";
            el("servers").value = (p?.target_servers || []).join(", ");
            el("tags").value = (p?.target_tags || []).join(", ");
            el("revision").textContent = p
                ? `Profile ${p.id} · saved revision ${p.revision}`
                : "";
            el("rows").innerHTML = (p?.checks || [])
                .map((c, i) => {
                    const prefix = `postcheck-${i}`;
                    const field = (name, label, type = "text", extra = "") =>
                        `<label for="${prefix}-${name}">${label}<input id="${prefix}-${name}" data-check-index="${i}" data-check-field="${name}" type="${type}" value="${escape(c[name])}" ${extra}></label>`;
                    return `<fieldset class="postcheck-row"><legend>Check ${i + 1} · ${escape(c.id)}</legend><div class="postcheck-fields">${field("name", "Check name", "text", "maxlength=100 required")}
                    <label for="${prefix}-kind">Check type<select id="${prefix}-kind" data-check-index="${i}" data-check-field="kind"><option value="http_status" ${c.kind === "http_status" ? "selected" : ""}>HTTP GET status</option><option value="systemd_active" ${c.kind === "systemd_active" ? "selected" : ""}>Systemd unit active</option></select></label>
                    ${c.kind === "systemd_active" ? field("unit", "Systemd unit", "text", "required") : field("url", "HTTP(S) health URL", "url", "required") + field("expected_status", "Expected HTTP status", "number", "min=100 max=599 required")}
                    ${field("timeout_seconds", "Recovery timeout (seconds)", "number", "min=1 max=300 required")}${field("interval_seconds", "Retry interval (seconds)", "number", "min=1 max=30 required")}
                    <label for="${prefix}-mode">Failure mode<select id="${prefix}-mode" data-check-index="${i}" data-check-field="mode"><option value="blocking" ${c.mode === "blocking" ? "selected" : ""}>Blocking</option><option value="warning" ${c.mode === "warning" ? "selected" : ""}>Warning</option></select></label></div><button type="button" class="btn-ghost" data-remove-check="${i}">Remove check</button></fieldset>`;
                })
                .join("");
            for (const id of [
                "new",
                "delete",
                "discard",
                "reload",
                "profile-select",
            ])
                el(id).disabled =
                    state.busy ||
                    (id === "delete" && !p) ||
                    (id === "reload" && state.dirty);
            el("save").disabled = state.busy || !state.dirty;
            el("test").disabled = state.busy || state.dirty;
            el("test-server").disabled = state.busy;
            el("test-server").value = state.testServer;
            el("feedback").textContent = state.busy
                ? "Running…"
                : state.message ||
                  (state.dirty
                      ? "Unsaved profile changes."
                      : "No unsaved changes.");
            el("feedback").classList.toggle("error", state.error);
            el("test-results").textContent = state.testResults
                ? `Server ${state.testResults.server_name || state.testServer} · ${state.testResults.profile.name} · revision ${state.testResults.profile.revision}\n` +
                  state.testResults.results
                      .map(
                          (r) =>
                              `${r.passed ? "PASS" : r.mode === "blocking" ? "FAIL" : "WARN"} ${r.name}: ${r.details} (${r.attempts} attempts, ${r.duration_ms} ms)`,
                      )
                      .join("\n")
                : "";
        }
        async function request(type, url, options = {}) {
            if (lifecycle.signal.aborted || view().busy) return;
            dispatch("request");
            const requestID = view().requestID;
            const controller = new AbortController();
            requests.add(controller);
            try {
                const response = await fetch(url, {
                    ...options,
                    cache: "no-store",
                    signal: controller.signal,
                });
                const data = await response.json();
                if (lifecycle.signal.aborted) return;
                if (!response.ok)
                    throw new Error(data.error || "Request failed.");
                dispatch(type, { requestID, data });
            } catch (error) {
                if (!lifecycle.signal.aborted)
                    dispatch("failed", { requestID, message: error.message });
            } finally {
                requests.delete(controller);
            }
        }
        async function load() {
            if (view().dirty || view().busy) return;
            await request("loaded", "/api/postcheck-profiles");
        }
        area.addEventListener(
            "input",
            (event) => {
                const target = event.target;
                if (target.id === "postcheck-test-server") {
                    dispatch("testServer", { name: target.value }, false);
                    el("test-results").textContent = "";
                }
                if (target.dataset.profileField) {
                    const key = target.dataset.profileField;
                    const value =
                        key === "name"
                            ? target.value
                            : target.value
                                  .split(",")
                                  .map((s) => s.trim())
                                  .filter(Boolean);
                    dispatch("profile", { patch: { [key]: value } }, false);
                }
                if (target.dataset.checkField && target.tagName !== "SELECT") {
                    const key = target.dataset.checkField;
                    dispatch(
                        "check",
                        {
                            index: Number(target.dataset.checkIndex),
                            patch: {
                                [key]:
                                    target.type === "number"
                                        ? Number(target.value)
                                        : target.value,
                            },
                        },
                        false,
                    );
                }
                el("save").disabled = view().busy || !view().dirty;
                el("test").disabled = view().busy || view().dirty;
                el("reload").disabled = view().busy || view().dirty;
            },
            { signal: lifecycle.signal },
        );
        area.addEventListener(
            "change",
            (event) => {
                const target = event.target;
                if (target.dataset.checkField && target.tagName === "SELECT")
                    dispatch("check", {
                        index: Number(target.dataset.checkIndex),
                        patch: { [target.dataset.checkField]: target.value },
                    });
                if (target.id === "postcheck-profile-select")
                    dispatch("select", { id: target.value });
            },
            { signal: lifecycle.signal },
        );
        area.addEventListener(
            "click",
            (event) => {
                const target = event.target.closest("button");
                if (!target) return;
                const actions = {
                    "postcheck-new": "new",
                    "postcheck-delete": "delete",
                    "postcheck-add": "addCheck",
                    "postcheck-discard": "discard",
                };
                if (actions[target.id]) dispatch(actions[target.id]);
                if (target.dataset.removeCheck !== undefined)
                    dispatch("removeCheck", {
                        index: Number(target.dataset.removeCheck),
                    });
                if (target.id === "postcheck-reload") void load();
                if (target.id === "postcheck-test") {
                    const name = view().testServer.trim();
                    if (!name) {
                        el("feedback").textContent =
                            "Enter the exact Server name to test.";
                        return;
                    }
                    void request(
                        "tested",
                        `/api/servers/${encodeURIComponent(name)}/postchecks/test`,
                        { method: "POST" },
                    );
                }
            },
            { signal: lifecycle.signal },
        );
        el("profile-form").addEventListener(
            "submit",
            (event) => {
                event.preventDefault();
                if (!view().dirty) return;
                void request("saved", "/api/postcheck-profiles", {
                    method: "PUT",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify(view().draft),
                });
            },
            { signal: lifecycle.signal },
        );
        const destroy = () => {
            lifecycle.abort();
            for (const controller of requests) controller.abort();
            requests.clear();
        };
        window.addEventListener("pagehide", destroy, {
            once: true,
            signal: lifecycle.signal,
        });
        render();
        return Object.freeze({ load, destroy });
    }
    root.ApplicationPostcheckAdapter = Object.freeze({ mount });
})(globalThis);
