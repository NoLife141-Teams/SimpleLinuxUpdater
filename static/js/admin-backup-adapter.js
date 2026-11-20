// Browser effects for AdminBackupAdapter; accepted state and command plans stay in the injected interaction owner.
(function (root) {
    "use strict";
    function create(options) {
        const lifecycle = root.BrowserEffects.create();
        const fetch = lifecycle.fetch;
        let reauthentication;
        const aptSelection = new Map();
        const aptResults = new Map();
        const aptBatchLimit = 12;
        let aptRun = null;
        const IntersectionObserver = root.IntersectionObserver
            ? lifecycle.observer(root.IntersectionObserver)
            : undefined;
        const ResizeObserver = root.ResizeObserver
            ? lifecycle.observer(root.ResizeObserver)
            : undefined;
        const {
            decodeURIComponent,
            formatSessionTime,
            adminPageView,
            escapeHtml,
            renderAdminDestructiveControls,
            adminPageInteraction,
            beginAdminCommand: admitAdminCommand,
            parseErrorResponse,
            finishAdminCommand: settleAdminCommand,
            beginAdminSnapshot,
            renderAdminWorkspace,
            updateFileLabel,
            refreshInstallation,
            describeRestoreAPTState,
        } = options;
        const commandPlans = new Set();
        function beginAdminCommand(...args) {
            const plan = admitAdminCommand(...args);
            if (plan) commandPlans.add(plan);
            return plan;
        }
        function finishAdminCommand(plan, ...args) {
            if (!commandPlans.delete(plan)) return;
            settleAdminCommand(plan, ...args);
        }
        function clearBackupCredentials() {
            for (const id of ["backup-restore-passphrase", "backup-export-passphrase", "backup-export-passphrase-confirm"]) {
                const input = document.getElementById(id);
                if (input) input.value = "";
            }
            adminPageInteraction.dispatch({ type: "backupPassphraseChanged", valid: false });
            for (const plan of commandPlans) {
                finishAdminCommand(plan, null, "Backup action cancelled when leaving this page.", true);
            }
        }
        function deriveDownloadFilename(contentDisposition) {
            if (!contentDisposition) return "";
            const utf8Match = contentDisposition.match(
                /filename\*=UTF-8''([^;]+)/i,
            );
            if (utf8Match && utf8Match[1]) {
                try {
                    return decodeURIComponent(utf8Match[1]).replace(
                        /[\r\n]/g,
                        "",
                    );
                } catch (_) {
                    if (!lifecycle.active || _?.name === "AbortError") return;
                    return utf8Match[1].replace(/[\r\n]/g, "");
                }
            }
            const simpleMatch = contentDisposition.match(
                /filename="?([^";]+)"?/i,
            );
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
            return (
                {
                    healthy: "Healthy",
                    stale: "Stale",
                    never: "Never recorded",
                    failed: "Failed",
                    unavailable: "Unavailable",
                }[state] || "Unavailable"
            );
        }

        function renderRecoveryOperation(prefix, operation) {
            const value = document.getElementById(`backup-recovery-${prefix}`);
            const detail = document.getElementById(
                `backup-recovery-${prefix}-detail`,
            );
            if (!value || !detail) return;
            if (!operation || !operation.lastSuccessAt) {
                value.textContent = recoveryStateLabel(
                    operation?.state || "unavailable",
                );
                detail.textContent =
                    operation?.message || "Recovery evidence is unavailable.";
                return;
            }
            const size =
                operation.sizeBytes === null
                    ? ""
                    : ` · ${formatBackupBytes(operation.sizeBytes)}`;
            value.textContent = `${formatSessionTime(operation.lastSuccessAt)}${size}`;
            if (operation.state === "failed" && operation.lastAttemptAt) {
                detail.textContent =
                    `Latest attempt failed ${formatSessionTime(operation.lastAttemptAt)}. ${operation.message || ""}`.trim();
                return;
            }
            detail.textContent =
                operation.message || recoveryStateLabel(operation.state);
        }

        function renderBackupRecoveryHealth() {
            const health = adminPageView().backup.recoveryHealth;
            const state = health?.state || "unavailable";
            const badge = document.getElementById(
                "backup-recovery-health-state",
            );
            if (badge) {
                badge.textContent = recoveryStateLabel(state);
                const className =
                    state === "healthy"
                        ? "pill-success"
                        : state === "stale"
                          ? "pill-warning"
                          : state === "failed"
                            ? "pill-danger"
                            : "pill-muted";
                badge.className = `pill ${className}`;
            }
            const message = document.getElementById(
                "backup-recovery-health-message",
            );
            if (message)
                message.textContent =
                    health?.message || "Recovery evidence is unavailable.";
            renderRecoveryOperation("export", health?.export);
            renderRecoveryOperation("verification", health?.verification);

            const next = document.getElementById("backup-recovery-next");
            if (next) {
                next.textContent =
                    health?.schedule?.scheduled && health.schedule.nextBackupAt
                        ? formatSessionTime(health.schedule.nextBackupAt)
                        : health?.schedule?.message ||
                          "No backup is scheduled.";
            }
            const threshold = document.getElementById(
                "backup-recovery-threshold",
            );
            if (threshold) {
                const hours = Number(health?.staleAfterHours) || 0;
                threshold.textContent =
                    hours > 0 && hours % 24 === 0
                        ? `${hours / 24} day${hours / 24 === 1 ? "" : "s"} (${hours} hours)`
                        : hours > 0
                          ? `${hours} hours`
                          : "Unavailable";
            }
            const checked = document.getElementById("backup-recovery-checked");
            if (checked)
                checked.textContent = health?.checkedAt
                    ? `Checked ${formatSessionTime(health.checkedAt)}.`
                    : "Evidence has not been checked.";
            const evidence = document.getElementById(
                "backup-recovery-retention-evidence",
            );
            if (evidence)
                evidence.textContent =
                    health?.retention?.evidenceDescription ||
                    "Recovery evidence is unavailable.";
            const archive = document.getElementById(
                "backup-recovery-retention-archive",
            );
            if (archive)
                archive.textContent =
                    health?.retention?.archiveDescription ||
                    "Exported archives are operator-managed.";
        }

        function renderBackupIssueList(containerID, issues) {
            const container = document.getElementById(containerID);
            const list = container?.querySelector("ul");
            if (!container || !list) return;
            const values = Array.isArray(issues) ? issues : [];
            container.hidden = values.length === 0;
            list.innerHTML = values
                .map(
                    (issue) =>
                        `<li>${escapeHtml(issue?.message || "Unknown restore-readiness issue.")}</li>`,
                )
                .join("");
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

            const readiness = document.getElementById(
                "backup-review-readiness",
            );
            const installation = review.installationReview;
            const ready = review.restoreReady && (!installation || installation.state === "ready");
            readiness.textContent = ready ? "Ready for confirmation"
                : installation?.state === "unavailable" ? "Check unavailable" : "Blocked";
            readiness.className = `pill ${ready ? "pill-success" : "pill-danger"}`;
            const installationSection = document.getElementById("backup-review-installation");
            installationSection.hidden = !installation;
            if (installation) {
                document.getElementById("backup-review-archive-compatibility").textContent = "Archive compatible";
                document.getElementById("backup-review-installation-message").textContent = installation.message;
                document.getElementById("backup-review-installation-conflicts").innerHTML = installation.conflicts.map(conflict => {
                    const report = String(conflict?.report_url || "");
                    const link = /^\/api\/reports\/jobs\/(?:[a-f0-9]{32}|job-[0-9]{1,19})$/.test(report)
                        ? ` <a href="${escapeHtml(report)}">Job report</a>` : "";
                    return `<li><strong>${escapeHtml(conflict?.server || "Server")}</strong> · ${escapeHtml(conflict?.message || "")}${link}</li>`;
                }).join("");
            }
            document.getElementById("backup-review-archive").textContent =
                `${review.archive.format || "Unknown format"} · version ${review.archive.version || "unknown"}`;
            document.getElementById("backup-review-created").textContent =
                formatSessionTime(review.archive.createdAt);
            document.getElementById("backup-review-size").textContent =
                formatBackupBytes(review.archive.sizeBytes);
            document.getElementById("backup-review-restart").textContent =
                review.impact.restartRequired ? "Required" : "Not required";

            const resources = Array.isArray(review.resources)
                ? review.resources
                : [];
            document.getElementById("backup-review-resources").innerHTML =
                resources
                    .map(
                        (resource) => `
        <li>
            <strong>${escapeHtml(resource.name || "Unknown resource")}</strong>:
            ${resource.included ? `included (${escapeHtml(formatBackupBytes(resource.size_bytes))})` : resource.required ? "missing (required)" : "not included (optional)"}
        </li>
    `,
                    )
                    .join("") || "<li>No resource inventory was returned.</li>";

            const counts = review.safeCounts || {};
            document.getElementById("backup-review-counts").innerHTML = [
                ["Servers", counts.servers],
                ["Policies", counts.policies],
                ["Jobs", counts.jobs],
                ["Sessions", counts.sessions],
            ]
                .map(
                    ([label, value]) =>
                        `<div><dt>${label}</dt><dd>${Number(value) || 0}</dd></div>`,
                )
                .join("");

            const impact = review.impact || {};
            document.getElementById("backup-review-impact").innerHTML = [
                impact.sessionsInvalidated
                    ? "All active Admin sessions will be invalidated."
                    : "Admin sessions are not expected to be invalidated.",
                impact.metricsAccessReplaced
                    ? "The Metrics API credential will be replaced by archived state."
                    : "Metrics API access is not expected to change.",
                impact.maintenanceRequired
                    ? "Exclusive maintenance mode will be activated."
                    : "Maintenance mode is not required.",
                impact.downtimeExpected
                    ? "Requests will pause during replacement and runtime reload."
                    : "No request downtime is expected.",
                impact.restartRequired
                    ? "An application restart is required."
                    : "No application restart is required.",
            ]
                .map((message) => `<li>${escapeHtml(message)}</li>`)
                .join("");

            const storagePanel = document.getElementById("backup-review-storage");
            const storageVolumes = document.getElementById("backup-review-storage-volumes");
            if (storagePanel && storageVolumes) {
                const roles = {
                    preparation: "Archive and SQLite preparation",
                    sqlite_replacement: "SQLite replacement",
                    ssh_trust_replacement: "SSH host-key replacement",
                    rollback_snapshots: "Rollback snapshots",
                };
                storagePanel.hidden = !review.storage.length;
                storageVolumes.innerHTML = review.storage.map(volume => {
                    const labels = volume.roles.map(role => roles[role]).filter(Boolean);
                    const available = volume.availableBytes === null ? "Available capacity unknown"
                        : formatBackupBytes(volume.availableBytes) + " available";
                    const state = volume.state === "insufficient" ? " · Insufficient space"
                        : volume.state === "unknown" && volume.availableBytes !== null ? " · Estimate incomplete" : "";
                    return `<li><strong>${escapeHtml(labels.join(", "))}</strong> · About ${escapeHtml(formatBackupBytes(volume.requiredBytes))} required · ${escapeHtml(available + state)}</li>`;
                }).join("");
            }

            renderBackupIssueList("backup-review-blockers", review.blockers);
            renderBackupIssueList("backup-review-warnings", review.warnings);
            renderAdminDestructiveControls();
        }

        function renderBackupAutomationControls() {
            const inFlight = adminPageView().commands?.inFlight || [];
            const busy = ["saveBackupSchedule", "runBackupSchedule"].some(
                (command) => inFlight.includes(command),
            );
            const save = document.getElementById("backup-automation-save");
            const run = document.getElementById("backup-automation-run");
            if (save) save.disabled = busy;
            if (run)
                run.disabled =
                    busy ||
                    !adminPageInteraction.planCommand("runBackupSchedule")
                        .enabled;
        }

        function init() {
            if (lifecycle.active) return;
            lifecycle.init();
            reauthentication = root.Reauthentication.create(lifecycle);
            lifecycle.own(clearBackupCredentials);
            lifecycle.own(() => {
                aptSelection.clear();
                aptResults.clear();
                aptRun = null;
                const feedback = document.getElementById("restore-apt-feedback");
                if (feedback) feedback.textContent = "";
            });
            renderBackupAutomationControls();
            lifecycle.listen(document.getElementById("backup-archive-catalog"), "click", event => {
                const button = event.target.closest("button[data-backup-archive]");
                if (button) void operateBackupArchive(button.dataset.backupArchive, button.dataset.backupOperation);
            });
            const resume = document.getElementById("restore-isolation-resume");
            lifecycle.listen(resume, "click", () => void resumeExternalWork(resume));
            const aptPanel = document.getElementById("restore-apt-validation");
            lifecycle.listen(aptPanel, "click", event => {
                const button = event.target.closest("button[data-restore-apt-name]");
                if (button && !button.disabled) {
                    const host = adminPageView().installation.restoreAPTHosts.find(item => item.identity === button.dataset.restoreAptIdentity);
                    if (host) void checkRestoredAPT([host], true);
                } else if (event.target.closest("#restore-apt-check-selected")) {
                    const hosts = adminPageView().installation.restoreAPTHosts;
                    void checkRestoredAPT(hosts.filter(host => aptSelection.has(host.identity)), false);
                }
            });
            lifecycle.listen(aptPanel, "change", event => {
                const input = event.target.closest("input[data-restore-apt-select]");
                if (!input || input.disabled || aptRun) return;
                const host = adminPageView().installation?.restoreAPTHosts.find(item => item.identity === input.dataset.restoreAptSelect);
                if (!host) return;
                if (input.checked) aptSelection.set(host.identity, {name: host.name, targetRevision: host.targetRevision});
                else aptSelection.delete(host.identity);
                renderRestoreAPTControls();
            });
            renderRestoreAPTControls();
        }

        function renderRestoreAPTControls() {
            const panel = document.getElementById("restore-apt-validation");
            if (!panel) return;
            const view = adminPageView();
            const freshness = view.streams?.installation?.freshness;
            const hosts = new Map((view.installation?.restoreAPTHosts || []).map(host => [host.identity, host]));
            for (const [identity, selected] of aptSelection) {
                const host = hosts.get(identity);
                if (!host || host.disabled || host.name !== selected.name || host.targetRevision !== selected.targetRevision) {
                    aptSelection.delete(identity);
                }
            }
            for (const [identity, result] of aptResults) {
                if (hosts.get(identity)?.name !== result.name) aptResults.delete(identity);
            }
            let finished = 0;
            for (const row of panel.querySelectorAll("tbody tr")) {
                const checkbox = row.querySelector("[data-restore-apt-select]");
                const host = hosts.get(checkbox.dataset.restoreAptSelect);
                if (!host) continue;
                checkbox.checked = aptSelection.has(host.identity);
                checkbox.disabled = Boolean(aptRun || host.disabled || (!checkbox.checked && aptSelection.size >= aptBatchLimit));
                row.querySelector("[data-restore-apt-name]").disabled = Boolean(aptRun || host.disabled);
                const initial = aptRun?.hosts.get(host.identity);
                const result = aptResults.get(host.identity);
                const completed = initial && host.checkedAt && host.checkedAt !== initial.checkedAt && !host.checking;
                const resultTime = Date.parse(result?.checked_at) || 0;
                const snapshotTime = Date.parse(host.checkedAt) || 0;
                const showResult = result?.state && result.target_revision === (host.targetRevision || undefined) &&
                    (resultTime > snapshotTime ||
                        (freshness !== "fresh" && resultTime === snapshotTime && result.state !== host.state));
                if (completed) finished++;
                row.querySelector("[data-restore-apt-progress]").textContent =
                    result?.error ? result.error :
                    showResult ? `Last submitted check · ${describeRestoreAPTState(result.state)}` :
                    initial && host.checking ? "Checking…" :
                    completed ? "Check finished" :
                    initial ? "Waiting to check…" : "";
            }
            const selected = panel.querySelector("#restore-apt-check-selected");
            selected.disabled = Boolean(aptRun || !aptSelection.size);
            selected.textContent = aptSelection.size ? `Check selected (${aptSelection.size})` : "Check selected";
            if (aptRun) {
                const feedback = document.getElementById("restore-apt-feedback");
                const progress = aptRun.single ? `Checking ${aptRun.name}…` : `${finished} of ${aptRun.hosts.size} checks finished.`;
                if (feedback.textContent !== progress) feedback.textContent = progress;
            }
        }

        async function checkRestoredAPT(hosts, single) {
            if (aptRun || !hosts.length || hosts.length > aptBatchLimit) return;
            const lifetime = lifecycle.capture();
            const feedback = document.getElementById("restore-apt-feedback");
            const targets = hosts.map(({ name, identity, targetRevision }) => ({ name, identity, target_revision: targetRevision || undefined }));
            const run = { single, name: targets[0].name, hosts: new Map(hosts.map(host => [host.identity, host])) };
            aptRun = run;
            for (const { identity } of targets) aptResults.delete(identity);
            renderRestoreAPTControls();
            let timer, polling = !single;
            const stopPolling = () => {
                polling = false;
                lifecycle.clearTimeout(timer);
            };
            const poll = () => {
                timer = lifecycle.setTimeout(async () => {
                    await refreshInstallation();
                    if (polling && lifecycle.isCurrent(lifetime)) poll();
                }, 1500);
            };
            if (polling) poll();
            const release = () => {
                stopPolling();
                aptRun = null;
                renderRestoreAPTControls();
            };
            const unregister = lifecycle.own(stopPolling);
            try {
                const response = await reauthentication.sensitive(
                    "/api/backup/restore-isolation/apt-check",
                    { method: "POST", headers: { "Content-Type": "application/json" },
                        body: JSON.stringify(single ? targets[0] : { targets }) },
                    "Check APT after restore",
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                stopPolling();
                let needsAttention = 0;
                if (!single) {
                    const results = new Map((response.results || []).map(result => [result.identity, result]));
                    for (const { name, identity } of targets) {
                        const result = results.get(identity);
                        aptResults.set(identity, result?.name === name ? result : { name, error: "No check result received. Try again." });
                        if (result?.name === name && ["healthy", "approved"].includes(result.state) && !result.error) {
                            aptSelection.delete(identity);
                        } else {
                            needsAttention++;
                        }
                    }
                }
                const subject = targets.length === 1 ? "1 server" : `${targets.length} servers`;
                const attention = needsAttention === 1 ? " 1 needs attention." : needsAttention ? ` ${needsAttention} need attention.` : "";
                feedback.textContent = single ? `Check finished for ${run.name}.` : `Checks finished for ${subject}.${attention}`;
                void refreshInstallation().then(() => {
                    if (lifecycle.isCurrent(lifetime)) renderRestoreAPTControls();
                });
            } catch (error) {
                if (lifecycle.isCurrent(lifetime)) feedback.textContent = error.name === "AbortError"
                    ? "APT checks cancelled." : error.message || "Could not check APT.";
            } finally {
                if (lifecycle.isCurrent(lifetime)) {
                    unregister();
                    release();
                }
            }
        }

        async function resumeExternalWork(button) {
            if (button.disabled) return;
            const lifetime = lifecycle.capture();
            const feedback = document.getElementById("restore-isolation-feedback");
            button.disabled = true;
            feedback.textContent = "";
            try {
                const confirmed = await window.confirmTypedAction(
                    "Review the restored settings before resuming external work.",
                    "RESUME",
                    {
                        title: "Resume external work",
                        operation: "Lift restore isolation",
                        consequences: "SSH and queued notifications may resume. Package maintenance remains blocked on servers that require an APT check.",
                        authentication: "Recent administrator reauthentication is required.",
                        confirmLabel: "Resume External Work",
                    },
                );
                if (!confirmed || !lifecycle.isCurrent(lifetime)) return;
                await reauthentication.sensitive(
                    "/api/backup/restore-isolation/resume",
                    {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({confirmation: "RESUME"})},
                    "Resume external work",
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                window.notifyApp("Restore isolation lifted.");
                await refreshInstallation();
            } catch (error) {
                if (lifecycle.isCurrent(lifetime)) feedback.textContent = error.message || "Could not resume external work.";
            } finally {
                if (lifecycle.isCurrent(lifetime)) button.disabled = false;
            }
        }

        function renderBackupAutomation(updateForm = false) {
            const view = adminPageView().backup.automation;
            const status = document.getElementById("backup-automation-status");
            if (!status || !view) return;
            status.textContent = `${view.state}: ${view.message}`;
            document.getElementById("backup-automation-next").textContent =
                view.nextAt ? formatSessionTime(view.nextAt) : "Not scheduled";
            document.getElementById("backup-automation-attempt").textContent =
                view.lastAttemptAt
                    ? formatSessionTime(view.lastAttemptAt)
                    : "Never";
            document.getElementById("backup-automation-success").textContent =
                view.lastSuccessAt
                    ? formatSessionTime(view.lastSuccessAt)
                    : "Never";
            renderBackupAutomationControls();
            renderBackupCatalog();
            if (updateForm) {
                const config = view.config;
                for (const name of ["cadence", "time", "weekday", "retain"])
                    document.getElementById(`backup-automation-${name}`).value =
                        config[name];
                document.getElementById("backup-automation-enabled").checked =
                    config.enabled;
                document.getElementById(
                    "backup-automation-known-hosts",
                ).checked = config.include_known_hosts;
            }
        }

        function renderBackupCatalog() {
            const container = document.getElementById("backup-archive-catalog");
            if (!container) return;
            const view = adminPageView().backup;
            const catalog = view.automation.catalog;
            if (view.catalogFeedback && !adminPageView().commands.inFlight.some(command => ["downloadBackupArchive", "reverifyBackupArchive"].includes(command))) {
                document.getElementById("backup-catalog-result").textContent = view.catalogFeedback.message;
            }
            const focused = document.activeElement?.dataset;
            const focusedID = focused?.backupArchive, focusedOperation = focused?.backupOperation;
            container.innerHTML = catalog.archives.length ? catalog.archives.map(archive => {
                const integrity = { matched: "SHA-256 matched", mismatch: "SHA-256 mismatch", unavailable: "Check unavailable" };
                const integrityState = integrity[archive.integrity.state] || "Unknown";
                const checkedAt = archive.integrity.checkedAt ? formatSessionTime(archive.integrity.checkedAt) : "";
                const download = adminPageInteraction.planCommand("downloadBackupArchive", { id: archive.id });
                const verify = adminPageInteraction.planCommand("reverifyBackupArchive", { id: archive.id });
                return `<article class="backup-block" data-archive-id="${escapeHtml(archive.id)}">
                    <h4>Published ${escapeHtml(archive.publishedAt ? formatSessionTime(archive.publishedAt) : "Unknown")}</h4>
                    <p>Size: ${escapeHtml(archive.sizeBytes === null ? "Unknown" : formatBackupBytes(archive.sizeBytes))} · known_hosts: ${archive.knownHostsIncluded === null ? "Unknown" : archive.knownHostsIncluded ? "Included" : "Not included"}</p>
                    <p>Local availability: <strong>${escapeHtml(archive.availability)}</strong> · Latest review: ${escapeHtml(archive.verification)}</p>
                    <p>Last successful verification: ${escapeHtml(archive.lastVerifiedAt ? formatSessionTime(archive.lastVerifiedAt) : "Unknown")}</p>
                    <p>Content integrity: <strong>${escapeHtml(integrityState)}</strong>${checkedAt ? " · Last completed check " + escapeHtml(checkedAt) : ""}</p>
                    <p>Data captured: ${escapeHtml(archive.capturedAt ? formatSessionTime(archive.capturedAt) : "Unknown")}</p>
                    <details><summary>Recorded SHA-256</summary><code>${escapeHtml(archive.sha256 || "Unknown")}</code></details>
                    <button type="button" class="btn-ghost" data-backup-archive="${escapeHtml(archive.id)}" data-backup-operation="download" ${download.enabled ? "" : "disabled"}>Download encrypted</button>
                    <button type="button" class="btn-ghost" data-backup-archive="${escapeHtml(archive.id)}" data-backup-operation="verify" ${verify.enabled ? "" : "disabled"}>Reverify</button>
                    ${archive.jobID ? `<a href="/api/reports/jobs/${encodeURIComponent(archive.jobID)}">View publication job</a>` : ""}
                </article>`;
            }).join("") : '<p class="muted">No registered automated archives.</p>';
            if (catalog.truncated) container.insertAdjacentHTML("beforeend", `<p class="muted">Only the latest ${catalog.archives.length} entries are shown.</p>`);
            if (focusedID) {
                const target = [...container.querySelectorAll("button[data-backup-archive]")].find(button => button.dataset.backupArchive === focusedID && button.dataset.backupOperation === focusedOperation);
                if (target && !target.disabled) target.focus({ preventScroll: true });
            }
        }

        async function operateBackupArchive(id, operation) {
            const lifetime = lifecycle.capture();
            const command = operation === "download" ? "downloadBackupArchive" : "reverifyBackupArchive";
            const plan = beginAdminCommand(command, { id });
            if (!plan) return;
            const output = document.getElementById("backup-catalog-result");
            output.textContent = operation === "download" ? "Validating encrypted archive…" : "Reviewing restore readiness without replacing active data…";
            renderBackupCatalog();
            try {
                const result = await reauthentication.sensitive(`/api/backup/archives/${encodeURIComponent(plan.payload.id)}/${operation}`, {
                    method: "POST", ...(operation === "download" ? { responseType: "blob" } : {})
                }, operation === "download" ? "Download registered encrypted backup" : "Reverify registered backup");
                if (!lifecycle.isCurrent(lifetime)) return;
                let message;
                if (operation === "download") {
                    const url = URL.createObjectURL(result);
                    try {
                        const link = document.createElement("a");
                        link.href = url; link.download = "simplelinuxupdater-automated-backup.slubkp";
                        document.body.appendChild(link); link.click(); link.remove();
                    } finally { URL.revokeObjectURL(url); }
                    message = "Encrypted backup downloaded. Keep its passphrase separately.";
                } else {
                    message = result.restore_ready ? "Reverification succeeded. Active data was not replaced." : "Reverification completed with restore blockers.";
                    const counts = result.safe_counts || {};
                    message += ` Archived data: ${Number(counts.servers) || 0} servers, ${Number(counts.policies) || 0} policies, ${Number(counts.jobs) || 0} jobs. `;
                    message += [...(result.blockers || []), ...(result.warnings || [])].map(issue => issue.message).join(" ");
                }
                finishAdminCommand(plan, operation === "verify" ? result : null, message);
                output.textContent = message;
            } catch (error) {
                if (!lifecycle.isCurrent(lifetime)) return;
                const message = error.name === "AbortError" ? "Backup action cancelled." : error.message || "Registered backup request failed.";
                finishAdminCommand(plan, null, message, true);
                output.textContent = message;
            } finally {
                if (lifecycle.isCurrent(lifetime)) {
                    await fetchBackupStatus();
                    if (!lifecycle.isCurrent(lifetime)) return;
                    renderBackupCatalog();
                    const container = document.getElementById("backup-archive-catalog");
                    if (document.activeElement === document.body || container?.contains(document.activeElement)) {
                        const button = [...container.querySelectorAll("button[data-backup-archive]")].find(item => item.dataset.backupArchive === id && item.dataset.backupOperation === operation);
                        if (button && !button.disabled) button.focus({ preventScroll: true });
                    }
                }
            }
        }

        async function submitBackupAutomation(runNow = false) {
            const lifetime = lifecycle.capture();

            const config = {
                enabled: document.getElementById("backup-automation-enabled")
                    .checked,
                cadence: document.getElementById("backup-automation-cadence")
                    .value,
                time: document.getElementById("backup-automation-time").value,
                weekday: Number(
                    document.getElementById("backup-automation-weekday").value,
                ),
                retain: Number(
                    document.getElementById("backup-automation-retain").value,
                ),
                include_known_hosts: document.getElementById(
                    "backup-automation-known-hosts",
                ).checked,
            };
            const command = runNow ? "runBackupSchedule" : "saveBackupSchedule";
            const plan = beginAdminCommand(command, { config });
            if (!plan) {
                window.notifyApp(
                    adminPageInteraction.planCommand(command, { config })
                        .reason,
                );
                return;
            }
            renderBackupAutomationControls();
            try {
                const res = await fetch(
                    runNow
                        ? "/api/backup/automation/run"
                        : "/api/backup/automation",
                    {
                        method: runNow ? "POST" : "PUT",
                        headers: { "Content-Type": "application/json" },
                        ...(runNow
                            ? {}
                            : { body: JSON.stringify(plan.payload.config) }),
                    },
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok)
                    throw new Error(
                        await parseErrorResponse(
                            res,
                            "Automated backup request failed.",
                        ),
                    );
                const data = await res.json();
                if (!lifecycle.isCurrent(lifetime)) return;
                finishAdminCommand(plan, data, data.message);
                window.notifyApp(data.message);
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                finishAdminCommand(plan, null, err.message, true);
                window.notifyApp(err.message);
            } finally {
                if (!lifecycle.isCurrent(lifetime)) return;
                renderBackupAutomationControls();
                await fetchBackupStatus();
                if (!lifecycle.isCurrent(lifetime)) return;
            }
        }

        async function fetchBackupStatus() {
            const lifetime = lifecycle.capture();

            const status = document.getElementById("backup-status");
            if (!status) return;
            const requestID = beginAdminSnapshot("backup");
            try {
                const res = await fetch("/api/backup/status");
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    lifecycle.discardResponse(res);
                    adminPageInteraction.dispatch({
                        type: "snapshotFailed",
                        stream: "backup",
                        requestID,
                        error: "Backup status: unavailable",
                    });
                    status.textContent = "Backup status: unavailable";
                    renderAdminWorkspace();
                    return;
                }
                const data = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                adminPageInteraction.dispatch({
                    type: "backupSnapshotReceived",
                    requestID,
                    receivedAt: new Date().toISOString(),
                    data,
                });
                renderBackupRecoveryHealth();
                renderBackupAutomation(true);
                const knownHostsState = data.known_hosts_exists
                    ? "present"
                    : "missing";
                status.textContent = `Backup paths: DB=${data.db_path || "-"}, config=${data.config_path || "-"}, known_hosts=${data.known_hosts_path || "-"} (${knownHostsState})`;
                renderAdminWorkspace();
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                console.error("Failed to fetch backup status:", err);
                adminPageInteraction.dispatch({
                    type: "snapshotFailed",
                    stream: "backup",
                    requestID,
                    error: "Backup status: request failed",
                });
                status.textContent = "Backup status: request failed";
                renderAdminWorkspace();
            }
        }

        async function exportBackup() {
            const lifetime = lifecycle.capture();

            const exportPassInput = document.getElementById(
                "backup-export-passphrase",
            );
            const exportPassConfirmInput = document.getElementById(
                "backup-export-passphrase-confirm",
            );
            let plan;
            try {
                const pass = exportPassInput?.value || "";
                const confirmPass = exportPassConfirmInput?.value || "";
                const includeKnownHosts = !!document.getElementById(
                    "backup-include-known-hosts",
                )?.checked;
                if (pass.length < 12) {
                    window.notifyApp(
                        "Passphrase must be at least 12 characters.",
                    );
                    return;
                }
                if (pass !== confirmPass) {
                    window.notifyApp("Passphrase confirmation does not match.");
                    return;
                }
                plan = beginAdminCommand("exportBackup", {
                    passphraseValid: pass.length >= 12,
                    passwordsMatch: pass === confirmPass,
                    includeKnownHosts,
                });
                if (!plan) {
                    window.notifyApp(
                        adminPageInteraction.planCommand("exportBackup")
                            .reason || "Backup is unavailable.",
                    );
                    return;
                }
                const res = await fetch("/api/backup/export", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({
                        passphrase: pass,
                        include_known_hosts: includeKnownHosts,
                    }),
                });
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    const message = await parseErrorResponse(
                        res,
                        "Failed to export backup.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    finishAdminCommand(plan, null, message, true);
                    window.notifyApp(message);
                    return;
                }
                const blob = await res.blob();
                if (!lifecycle.isCurrent(lifetime)) return;
                const filename =
                    deriveDownloadFilename(
                        res.headers.get("Content-Disposition"),
                    ) || `simplelinuxupdater-backup-${Date.now()}.slubkp`;
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
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                console.error("Failed to export backup:", err);
                finishAdminCommand(
                    plan,
                    null,
                    "Failed to export backup.",
                    true,
                );
                window.notifyApp("Failed to export backup.");
            } finally {
                if (!lifecycle.isCurrent(lifetime)) return;
                if (exportPassInput) exportPassInput.value = "";
                if (exportPassConfirmInput) exportPassConfirmInput.value = "";
            }
        }

        async function restoreBackup() {
            const lifetime = lifecycle.capture();

            const fileInput = document.getElementById("backup-restore-file");
            const restorePassInput = document.getElementById(
                "backup-restore-passphrase",
            );
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
                    window.notifyApp(
                        "Passphrase must be at least 12 characters.",
                    );
                    return;
                }
                const readiness = adminPageInteraction.planCommand(
                    "restoreBackup",
                    { passphraseValid: pass.length >= 12 },
                );
                if (!readiness.enabled) {
                    window.notifyApp(
                        readiness.reason ||
                            "Review restore readiness before replacement.",
                    );
                    return;
                }
                const review = adminPageView().backup.review;
                const counts = review?.safeCounts || {};
                const isolate = document.getElementById("backup-restore-isolated")?.checked === true;
                if (
                    !(await window.confirmTypedAction(
                        "Type the confirmation text only after verifying this backup.",
                        "RESTORE",
                        {
                            title: "Restore application backup",
                            operation:
                                "Replace application data from an encrypted backup",
                            resources: `${file.name}; ${Number(counts.servers) || 0} servers, ${Number(counts.policies) || 0} policies, ${Number(counts.jobs) || 0} jobs, ${Number(counts.sessions) || 0} archived sessions`,
                            consequences:
                                "Current application data is replaced, all active Admin sessions are invalidated, the Metrics API credential is replaced, and requests pause during exclusive maintenance." +
                                (isolate ? " SSH and notification delivery stay paused after restore until an administrator resumes them." : ""),
                            reversibility:
                                "Not automatically reversible. Export a current backup first if rollback may be needed.",
                            authentication:
                                "The backup passphrase and typed confirmation are both required.",
                            confirmLabel: "Restore Backup",
                        },
                    ))
                ) {
                    return;
                }
                if (!lifecycle.isCurrent(lifetime)) return;
                plan = beginAdminCommand("restoreBackup", {
                    passphraseValid: pass.length >= 12,
                });
                if (!plan) {
                    window.notifyApp(
                        adminPageInteraction.planCommand("restoreBackup", {
                            passphraseValid: pass.length >= 12,
                        }).reason || "Restore review is no longer current.",
                    );
                    return;
                }
                const form = new FormData();
                form.append("file", file);
                form.append("passphrase", pass);
                form.append("isolate", String(isolate));
                clearCredential = true;
                const payload = await reauthentication.sensitive(
                    "/api/backup/restore",
                    { method: "POST", body: form },
                    "Restore application backup",
                );
                if (!lifecycle.isCurrent(lifetime)) return;
                finishAdminCommand(
                    plan,
                    payload,
                    "Backup restored successfully.",
                );
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
                if (!lifecycle.isCurrent(lifetime)) return;
            } catch (err) {
                if (!lifecycle.isCurrent(lifetime)) return;
                // The transport error contains no archive or passphrase.
                finishAdminCommand(
                    plan,
                    null,
                    err.message || "Failed to restore backup.",
                    true,
                );
                window.notifyApp(err.message || "Failed to restore backup.");
            } finally {
                if (!lifecycle.isCurrent(lifetime)) return;
                if (clearCredential) {
                    if (restorePassInput) restorePassInput.value = "";
                    adminPageInteraction.dispatch({
                        type: "backupPassphraseChanged",
                        valid: false,
                    });
                    renderBackupRestoreReview();
                }
            }
        }

        async function verifyBackup() {
            const lifetime = lifecycle.capture();

            const fileInput = document.getElementById("backup-restore-file");
            const restorePassInput = document.getElementById(
                "backup-restore-passphrase",
            );
            let plan;
            try {
                const pass = restorePassInput?.value || "";
                const file = fileInput?.files?.[0];
                if (!file) {
                    window.notifyApp("Choose a backup file first.");
                    return;
                }
                if (pass.length < 12) {
                    window.notifyApp(
                        "Passphrase must be at least 12 characters.",
                    );
                    return;
                }
                adminPageInteraction.dispatch({
                    type: "backupFileSelected",
                    file,
                });
                plan = beginAdminCommand("verifyBackup", {
                    passphraseValid: pass.length >= 12,
                });
                if (!plan) return;
                const form = new FormData();
                form.append("file", file);
                form.append("passphrase", pass);
                const res = await fetch("/api/backup/verify", {
                    method: "POST",
                    body: form,
                });
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    const message = await parseErrorResponse(
                        res,
                        "Failed to verify backup.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    finishAdminCommand(plan, null, message, true);
                    window.notifyApp(message);
                    return;
                }
                const payload = await res.json().catch(() => ({}));
                if (!lifecycle.isCurrent(lifetime)) return;
                const message = !payload.restore_ready || payload.installation_review?.state === "blocked"
                    ? "Restore readiness review completed with blockers."
                    : payload.installation_review?.state === "unavailable"
                        ? "Archive verified. APT compatibility could not be checked."
                        : "Restore readiness review completed.";
                finishAdminCommand(plan, payload, message);
                renderBackupRestoreReview();
                window.notifyApp(message);
            } catch (err) {
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                console.error("Failed to verify backup:", err);
                finishAdminCommand(
                    plan,
                    null,
                    "Failed to verify backup.",
                    true,
                );
                renderBackupRestoreReview();
                window.notifyApp("Failed to verify backup.");
            }
        }
        return Object.freeze({
            init,
            destroy: lifecycle.destroy,
            renderRestoreAPTControls,
            renderBackupRestoreReview: (...args) =>
                lifecycle.active
                    ? renderBackupRestoreReview(...args)
                    : undefined,
            submitBackupAutomation: (...args) =>
                lifecycle.active ? submitBackupAutomation(...args) : undefined,
            fetchBackupStatus: (...args) =>
                lifecycle.active ? fetchBackupStatus(...args) : undefined,
            exportBackup: (...args) =>
                lifecycle.active ? exportBackup(...args) : undefined,
            restoreBackup: (...args) =>
                lifecycle.active ? restoreBackup(...args) : undefined,
            verifyBackup: (...args) =>
                lifecycle.active ? verifyBackup(...args) : undefined,
        });
    }
    root.AdminBackupAdapter = Object.freeze({ create });
})(globalThis);
