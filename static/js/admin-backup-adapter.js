// Browser effects for AdminBackupAdapter; accepted state and command plans stay in the injected interaction owner.
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
            decodeURIComponent,
            formatSessionTime,
            adminPageView,
            escapeHtml,
            renderAdminDestructiveControls,
            adminPageInteraction,
            beginAdminCommand,
            parseErrorResponse,
            finishAdminCommand,
            beginAdminSnapshot,
            renderAdminWorkspace,
            updateFileLabel,
        } = options;
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
            readiness.textContent = review.restoreReady
                ? "Ready for confirmation"
                : "Blocked";
            readiness.className = `pill ${review.restoreReady ? "pill-success" : "pill-danger"}`;
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

            renderBackupIssueList("backup-review-blockers", review.blockers);
            renderBackupIssueList("backup-review-warnings", review.warnings);
            renderAdminDestructiveControls();
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
            document.getElementById("backup-automation-run").disabled =
                !adminPageInteraction.planCommand("runBackupSchedule").enabled;
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
            const save = document.getElementById("backup-automation-save");
            const run = document.getElementById("backup-automation-run");
            save.disabled = true;
            run.disabled = true;
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
                save.disabled = false;
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
                                "Current application data is replaced, all active Admin sessions are invalidated, the Metrics API credential is replaced, and requests pause during exclusive maintenance.",
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
                clearCredential = true;
                const res = await fetch("/api/backup/restore", {
                    method: "POST",
                    body: form,
                });
                if (!lifecycle.isCurrent(lifetime)) return;
                if (!res.ok) {
                    const message = await parseErrorResponse(
                        res,
                        "Failed to restore backup.",
                    );
                    if (!lifecycle.isCurrent(lifetime)) return;
                    finishAdminCommand(plan, null, message, true);
                    window.notifyApp(message);
                    return;
                }
                const payload = await res.json().catch(() => ({}));
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
                if (
                    !lifecycle.isCurrent(lifetime) ||
                    err?.name === "AbortError"
                )
                    return;
                console.error("Failed to restore backup:", err);
                finishAdminCommand(
                    plan,
                    null,
                    "Failed to restore backup.",
                    true,
                );
                window.notifyApp("Failed to restore backup.");
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
                const message = payload.restore_ready
                    ? "Restore readiness review completed."
                    : "Restore readiness review completed with blockers.";
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
            init: lifecycle.init,
            destroy: lifecycle.destroy,
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
