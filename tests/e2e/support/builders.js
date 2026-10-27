const { ensureAuthenticatedSession } = require("./fixtures");
// Synthetic accepted facts and browser API fixtures shared by domain scenarios.
const { expect } = require("@playwright/test");
const fs = require("node:fs");
const path = require("node:path");
async function fulfillJson(route, payload, status = 200) {
    await route.fulfill({
        status,
        contentType: "application/json",
        body: JSON.stringify(payload),
    });
}

function backupRecoveryHealthFixture(state = "healthy") {
    const failed = state === "failed";
    const never = state === "never";
    const stale = state === "stale";
    const unavailable = state === "unavailable";
    return {
        state,
        message: {
            healthy:
                "Recent successful backup export and verification evidence is available.",
            stale: "Backup recovery evidence is older than the 168-hour threshold.",
            never: "No successful backup export and verification have both been recorded.",
            failed: "The latest backup export or verification did not produce accepted recovery evidence.",
            unavailable: "Backup recovery evidence is unavailable.",
        }[state],
        checked_at: "2026-07-28T05:00:00Z",
        stale_after_hours: 168,
        export: {
            state: unavailable ? "unavailable" : failed ? "failed" : state,
            last_attempt_at:
                never || unavailable
                    ? ""
                    : failed
                      ? "2026-07-28T04:00:00Z"
                      : "2026-07-27T05:00:00Z",
            last_success_at: never || unavailable ? "" : "2026-07-27T05:00:00Z",
            size_bytes: never || unavailable ? null : 4096,
            message: failed
                ? "Failed to build backup payload."
                : never
                  ? "No backup export has been recorded."
                  : "Backup exported.",
        },
        verification: {
            state: unavailable ? "unavailable" : never ? "never" : "healthy",
            last_attempt_at: never || unavailable ? "" : "2026-07-27T17:00:00Z",
            last_success_at: never || unavailable ? "" : "2026-07-27T17:00:00Z",
            size_bytes: never || unavailable ? null : 4096,
            message: never
                ? "No backup verification has been recorded."
                : "Backup restore readiness reviewed.",
        },
        schedule: { scheduled: false, message: "No backup is scheduled." },
        retention: {
            evidence_days: 90,
            archive_retained_by_app: false,
            automatic_deletion: false,
            evidence_description:
                "Backup recovery evidence is retained in audit history for up to 90 days.",
            archive_description:
                "Exported archives are downloaded to operator-managed storage and are not retained or deleted by the app.",
        },
    };
}

async function dismissTypedConfirm(page, trigger, wrongText = "WRONG") {
    await trigger.click();
    await expect(page.locator("#typed-confirm-modal")).toBeVisible();
    await page.locator("#typed-confirm-input").fill(wrongText);
    await expect(page.locator("#typed-confirm-submit")).toBeDisabled();
    await page.locator("#typed-confirm-cancel").click();
    await expect(page.locator("#typed-confirm-modal")).not.toBeVisible();
}

async function acceptTypedConfirm(page, trigger, requiredText) {
    await trigger.click();
    await expect(page.locator("#typed-confirm-modal")).toBeVisible();
    await page.locator("#typed-confirm-input").fill(requiredText);
    await expect(page.locator("#typed-confirm-submit")).toBeEnabled();
    await page.locator("#typed-confirm-submit").click();
    await expect(page.locator("#typed-confirm-modal")).not.toBeVisible();
}

function makeTimeline(status) {
    const phases = [
        ["prechecks", "Pre-checks"],
        ["apt_update", "APT update"],
        ["pending_approval", "Pending approval"],
        ["upgrade", "Upgrade"],
        ["postchecks", "Post-checks"],
        ["done_error", "Done / Error"],
    ];
    const statusMap = {
        pending_approval: ["pending_approval", "waiting", 60],
        updating: ["prechecks", "active", 32],
        upgrading: ["upgrade", "active", 72],
        done: ["done_error", "done", 100],
        error: ["done_error", "error", 100],
    };
    const [currentPhase, state, progress] = statusMap[status] || [
        "",
        "idle",
        0,
    ];
    const currentIndex = phases.findIndex(([key]) => key === currentPhase);
    return {
        current_phase: currentPhase,
        current_label: phases[currentIndex]?.[1] || "Idle",
        state,
        progress_pct: progress,
        summary:
            state === "idle"
                ? "No maintenance activity"
                : `Runtime status: ${status}`,
        updated_at: "2026-05-28T12:00:00Z",
        updated_at_display: "May 28, 2026 12:00",
        phases: phases.map(([key, label], index) => {
            let phaseState = "pending";
            if (state === "done") phaseState = "done";
            else if (state === "error")
                phaseState =
                    index < currentIndex
                        ? "done"
                        : index === currentIndex
                          ? "error"
                          : "pending";
            else if (currentIndex >= 0)
                phaseState =
                    index < currentIndex
                        ? "done"
                        : index === currentIndex
                          ? state
                          : "pending";
            return {
                key,
                label,
                state: phaseState,
                progress_pct: index === currentIndex ? progress : 0,
            };
        }),
    };
}

function makeDashboardSummary(servers) {
    const dashboardServers = servers.map((server) => {
        const pendingUpdates = Array.isArray(server.pending_updates)
            ? server.pending_updates
            : [];
        const cves = pendingUpdates.flatMap((update) => update.cves || []);
        const securityUpdates = pendingUpdates.filter(
            (update) => update.security,
        ).length;
        const plan = server.upgrade_plan || {};
        const standardSecurity = Number.isFinite(
            Number(plan.standard_security_count),
        )
            ? Number(plan.standard_security_count)
            : securityUpdates;
        const totalSecurity = Number.isFinite(Number(plan.total_security_count))
            ? Number(plan.total_security_count)
            : securityUpdates;
        const keptBackSecurity = Math.max(0, totalSecurity - standardSecurity);
        const riskLevel =
            cves.length > 0
                ? "critical"
                : securityUpdates > 0
                  ? "elevated"
                  : "normal";
        return {
            name: server.name,
            next_run: server.next_run || {
                state: "none",
                summary: "No scheduled run",
            },
            no_run: {
                active: false,
                summary: "No no-run window active",
                timezone: "UTC",
            },
            health: {
                source: server.facts_state === "stale" ? "unknown" : "facts",
                collected_at:
                    server.facts_state === "stale"
                        ? ""
                        : "2026-05-28T12:00:00Z",
                disk_status: "ok",
                apt_status: "ok",
                os_pretty_name: "Ubuntu",
                uptime_seconds: 3600,
            },
            risk: {
                level: riskLevel,
                summary:
                    cves.length > 0
                        ? `${cves.length} CVE`
                        : securityUpdates > 0
                          ? `${securityUpdates} security`
                          : "No CVE exposure",
                pending_packages: pendingUpdates.length,
                security_updates: securityUpdates,
                cves,
            },
            timeline: makeTimeline(server.timeline_status || server.status),
            approval_triage: {
                eligible:
                    server.status === "pending_approval" ||
                    pendingUpdates.length > 0,
                pending_packages: pendingUpdates.length,
                security_updates: securityUpdates,
                cve_count: cves.length,
                risk_level: riskLevel,
                risk_label:
                    cves.length > 0
                        ? `${cves.length} CVE`
                        : securityUpdates > 0
                          ? `${securityUpdates} security`
                          : "No CVE exposure",
                risk_order: cves.length > 0 ? 4 : securityUpdates > 0 ? 3 : 2,
                facts_state: server.facts_state || "fresh",
                last_check_display: "May 28, 2026 12:00",
                standard_packages: Number(
                    plan.standard_package_count || pendingUpdates.length,
                ),
                kept_back_packages: Number(plan.kept_back_package_count || 0),
                standard_security_updates: standardSecurity,
                kept_back_security_updates: keptBackSecurity,
                can_approve_all: server.status === "pending_approval",
                can_approve_security:
                    server.status === "pending_approval" &&
                    standardSecurity > 0,
                can_approve_kept_back_security:
                    server.status === "pending_approval" &&
                    keptBackSecurity > 0 &&
                    !!plan.kept_back_security_plan_available,
                can_cancel: server.status === "pending_approval",
                can_refresh_facts: true,
                can_run_checks: !["updating", "upgrading"].includes(
                    server.status,
                ),
            },
            command_history: Array.isArray(server.command_history)
                ? server.command_history
                : [],
        };
    });
    return {
        generated_at: "2026-05-28T12:00:00Z",
        fleet: {
            pending_approval: servers.filter(
                (server) => server.status === "pending_approval",
            ).length,
            prechecks_running: servers.filter((server) =>
                ["updating", "upgrading"].includes(server.status),
            ).length,
            in_progress: servers.filter((server) =>
                ["updating", "upgrading"].includes(server.status),
            ).length,
            done: servers.filter((server) => server.status === "done").length,
            stale_facts: servers.filter((server) =>
                ["stale", "unknown"].includes(server.facts_state),
            ).length,
            high_risk_cve: dashboardServers.filter(
                (server) => server.approval_triage.cve_count > 0,
            ).length,
            pending_packages: dashboardServers.reduce(
                (sum, server) => sum + server.approval_triage.pending_packages,
                0,
            ),
            security_updates: dashboardServers.reduce(
                (sum, server) => sum + server.approval_triage.security_updates,
                0,
            ),
        },
        servers: dashboardServers,
    };
}

function makeHealthTrends(servers) {
    const trendServers = servers.map((server, index) => {
        const pendingUpdates = Array.isArray(server.pending_updates)
            ? server.pending_updates
            : [];
        const securityUpdates = pendingUpdates.filter(
            (update) => update.security,
        ).length;
        const packageCount = pendingUpdates.length;
        const latestDiskFree = 8192 - index * 1024;
        return {
            name: server.name,
            samples: 2,
            latest: {
                captured_at: "2026-05-28T12:00:00Z",
                captured_at_display: "May 28, 2026 12:00",
                source: "audit",
                package_count: packageCount,
                security_count: securityUpdates,
                last_update_status:
                    server.status === "error" ? "failure" : "success",
                disk_status: server.facts_state === "stale" ? "unknown" : "ok",
                disk_free_kb: latestDiskFree,
                disk_total_kb: 16384,
                apt_status: "ok",
                reboot_required: false,
                os_pretty_name: "Ubuntu",
            },
            first: {
                captured_at: "2026-05-27T12:00:00Z",
                package_count: packageCount + 1,
                security_count: securityUpdates + 1,
                disk_status: "ok",
                disk_free_kb: latestDiskFree + 512,
                disk_total_kb: 16384,
                apt_status: "ok",
            },
            package_delta: -1,
            security_delta: -1,
            disk_free_delta_kb: -512,
            update_failures: server.status === "error" ? 1 : 0,
            scan_failures: 0,
            apt_problem_samples: 0,
            disk_problem_samples: server.facts_state === "stale" ? 1 : 0,
            reboot_seen: false,
            points: [],
        };
    });
    return {
        window: "7d",
        from: "2026-05-21T12:00:00Z",
        from_display: "May 21, 2026 12:00",
        to: "2026-05-28T12:00:00Z",
        to_display: "May 28, 2026 12:00",
        generated_at: "2026-05-28T12:00:00Z",
        retention_days: 90,
        fleet: {
            servers_with_samples: trendServers.length,
            samples: trendServers.reduce(
                (sum, server) => sum + server.samples,
                0,
            ),
            update_failures: trendServers.reduce(
                (sum, server) => sum + server.update_failures,
                0,
            ),
            scan_failures: 0,
            apt_problem_samples: 0,
            disk_problem_samples: trendServers.reduce(
                (sum, server) => sum + server.disk_problem_samples,
                0,
            ),
            reboot_seen: 0,
        },
        servers: trendServers,
    };
}

async function stubDashboardApi(page, getServers) {
    await page.route("**/api/servers", (route) =>
        fulfillJson(route, getServers()),
    );
    await page.route("**/api/keys/global", (route) =>
        fulfillJson(route, { has_key: false }),
    );
    await page.route("**/api/audit-events*", (route) => {
        const requestURL = new URL(route.request().url());
        const targetName = requestURL.searchParams.get("target_name");
        const targetType = requestURL.searchParams.get("target_type");
        const server =
            targetType === "server" && targetName
                ? getServers().find((item) => item.name === targetName)
                : null;
        if (server)
            expect(requestURL.searchParams.get("order")).toBe("created_at");
        const items = Array.isArray(server?.command_history)
            ? server.command_history.slice(0, 8)
            : [];
        return fulfillJson(route, {
            items,
            total: items.length,
            page: 1,
            page_size: 8,
        });
    });
    await page.route("**/api/observability/summary*", (route) =>
        fulfillJson(route, {
            totals: { updates_total: 0, success_rate_pct: 0 },
        }),
    );
    await page.route("**/api/observability/health-trends*", (route) =>
        fulfillJson(route, makeHealthTrends(getServers())),
    );
    await page.route("**/api/update-policies", (route) =>
        fulfillJson(route, []),
    );
    await page.route("**/api/dashboard/summary*", (route) => {
        const requestURL = new URL(route.request().url());
        const summary = makeDashboardSummary(getServers());
        if (
            requestURL.searchParams.get("include_command_history") === "false"
        ) {
            summary.servers = summary.servers.map((server) => ({
                ...server,
                command_history: [],
            }));
        }
        return fulfillJson(route, summary);
    });
}

function makeServer(
    name,
    status = "idle",
    pendingUpdates = [],
    overrides = {},
) {
    return {
        name,
        host: `${name}.example.test`,
        port: 22,
        user: "root",
        status,
        tags: [],
        pending_updates: pendingUpdates,
        pending_package_count: pendingUpdates.length,
        security_update_count: pendingUpdates.filter(
            (update) => update.security,
        ).length,
        logs: "ready",
        ...overrides,
    };
}

function makePendingUpdates(count) {
    return Array.from({ length: count }, (_, index) => ({
        package: `pkg-${String(index + 1).padStart(2, "0")}`,
        current_version: "1.0.0",
        candidate_version: "1.0.1",
        source: "ubuntu",
        security: index % 3 === 0,
        cve_state: index % 2 === 0 ? "pending" : "ready",
        cves:
            index % 5 === 0
                ? [`CVE-2026-${String(index + 1).padStart(4, "0")}`]
                : [],
    }));
}

async function stubAdminApi(page, state = {}) {
    state.sessions = state.sessions || [
        {
            id: "current-session",
            current: true,
            created_at: "2026-05-17T12:00:00Z",
            last_seen_at: "2026-05-17T12:05:00Z",
            expires_at: "2026-06-16T12:05:00Z",
            client_ip: "192.168.1.x",
            client_label: "Chrome · Windows",
        },
        {
            id: "other-session",
            current: false,
            created_at: "2026-05-16T09:00:00Z",
            last_seen_at: "2026-05-17T11:00:00Z",
            expires_at: "2026-06-16T11:00:00Z",
            client_ip: "203.0.113.x",
            client_label: "Firefox · Linux",
        },
    ];
    await page.route("**/api/app-settings/timezone", async (route) => {
        if (route.request().method() === "PUT") {
            state.timezoneSave = await route.request().postDataJSON();
        }
        const configuredTimezone =
            state.timezoneSave?.timezone ?? "America/Toronto";
        const resolvedTimezone = configuredTimezone || "America/Toronto";
        return fulfillJson(route, {
            timezone: resolvedTimezone,
            resolved_timezone: resolvedTimezone,
            editable_timezone: configuredTimezone,
        });
    });
    await page.route("**/api/auth/sessions/*", async (route) => {
        const id = decodeURIComponent(route.request().url().split("/").pop());
        state.sessionRevokeID = id;
        state.sessions = state.sessions.filter((session) => session.id !== id);
        return fulfillJson(route, { current_session: false });
    });
    await page.route("**/api/auth/sessions/*/reveal-ip", async (route) => {
        state.sessionIPRevealPayload = await route.request().postDataJSON();
        if (state.sessionIPRevealDelayMs) {
            await new Promise((resolve) =>
                setTimeout(resolve, state.sessionIPRevealDelayMs),
            );
        }
        if (state.sessionIPRevealFailure) {
            return route.fulfill({
                status: 401,
                contentType: "application/json",
                body: JSON.stringify({ error: "current password is invalid" }),
            });
        }
        return fulfillJson(route, {
            ip: "192.168.1.44",
            visible_for_seconds: state.sessionIPRevealSeconds || 30,
        });
    });
    await page.route("**/api/auth/sessions/others", async (route) => {
        state.sessionClearOthersCount =
            (state.sessionClearOthersCount || 0) + 1;
        const before = state.sessions.length;
        state.sessions = state.sessions.filter((session) => session.current);
        return fulfillJson(route, {
            deleted_sessions: before - state.sessions.length,
        });
    });
    await page.route("**/api/auth/sessions", async (route) => {
        if (route.request().method() === "DELETE") {
            state.sessionClearCount = (state.sessionClearCount || 0) + 1;
            return fulfillJson(route, { deleted: 2 });
        }
        return fulfillJson(route, {
            session_count: state.sessions.length,
            sessions: state.sessions,
            password_policy: {
                min_length: 10,
                max_length: 64,
                requires_letter: true,
                requires_digit: true,
            },
        });
    });
    await page.route("**/api/auth/password", async (route) => {
        state.passwordPayload = await route.request().postDataJSON();
        state.passwordPayloads = [
            ...(state.passwordPayloads || []),
            state.passwordPayload,
        ];
        const preservedBefore = state.sessions.length;
        if (state.passwordPartialFailure) {
            return route.fulfill({
                status: 207,
                contentType: "application/json",
                body: JSON.stringify({
                    message:
                        "password changed, but other sessions could not be invalidated",
                    outcome: "partial_failure",
                    password_changed: true,
                    invalidation_requested: true,
                    invalidated_sessions: 0,
                    preserved_sessions: preservedBefore,
                    current_session_preserved: true,
                }),
            });
        }
        let invalidated = 0;
        if (state.passwordPayload.invalidate_other_sessions) {
            const before = state.sessions.length;
            state.sessions = state.sessions.filter(
                (session) => session.current,
            );
            invalidated = before - state.sessions.length;
        }
        return fulfillJson(route, {
            message: "password changed",
            outcome: "succeeded",
            password_changed: true,
            invalidation_requested: Boolean(
                state.passwordPayload.invalidate_other_sessions,
            ),
            invalidated_sessions: invalidated,
            preserved_sessions: state.sessions.length,
            current_session_preserved: true,
        });
    });
    await page.route("**/api/notifications/settings", async (route) => {
        if (route.request().method() === "PUT") {
            state.notificationPayload = await route.request().postDataJSON();
            if (state.notificationPayload.webhook_url_intent === "replace") {
                state.notificationConfiguredURL =
                    state.notificationPayload.webhook_url;
            }
            if (state.notificationPayload.webhook_url_intent === "clear") {
                state.notificationConfiguredURL = "";
            }
            return fulfillJson(route, {
                enabled: state.notificationPayload.enabled,
                webhook_configured: Boolean(state.notificationConfiguredURL),
                webhook_url_masked: state.notificationConfiguredURL
                    ? "https://hooks.example.test/••••"
                    : "",
                webhook_url_intent:
                    state.notificationPayload.webhook_url_intent,
                event_types: state.notificationPayload.event_types,
                reminders_enabled: state.notificationPayload.reminders_enabled,
                reminder_interval_minutes:
                    state.notificationPayload.reminder_interval_minutes,
                supported_events: [
                    "update.complete",
                    "schedule.run.failed",
                    "schedule.run.skipped",
                    "backup.restore",
                ],
            });
        }
        state.notificationLoadCount = (state.notificationLoadCount || 0) + 1;
        if ((state.notificationFailuresRemaining || 0) > 0) {
            state.notificationFailuresRemaining -= 1;
            return route.fulfill({
                status: 503,
                contentType: "application/json",
                body: JSON.stringify({ error: "notifications unavailable" }),
            });
        }
        return fulfillJson(route, {
            enabled: false,
            webhook_configured: Boolean(state.notificationConfiguredURL),
            webhook_url_masked: state.notificationConfiguredURL
                ? "https://hooks.example.test/••••"
                : "",
            webhook_url_intent: "preserve",
            event_types: [
                "update.complete",
                "schedule.run.failed",
                "schedule.run.skipped",
                "backup.restore",
            ],
            supported_events: [
                "update.complete",
                "schedule.run.failed",
                "schedule.run.skipped",
                "backup.restore",
            ],
        });
    });
    await page.route(
        "**/api/notifications/delivery-diagnostics",
        async (route) => {
            state.notificationDiagnosticsLoadCount =
                (state.notificationDiagnosticsLoadCount || 0) + 1;
            if ((state.notificationDiagnosticsFailuresRemaining || 0) > 0) {
                state.notificationDiagnosticsFailuresRemaining -= 1;
                return route.fulfill({
                    status: 503,
                    contentType: "application/json",
                    body: JSON.stringify({
                        error: "delivery diagnostics unavailable",
                    }),
                });
            }
            return fulfillJson(route, {
                last_attempt: state.notificationDiagnosticsResponse || null,
            });
        },
    );
    await page.route("**/api/notifications/test*", async (route) => {
        state.notificationTestCount = (state.notificationTestCount || 0) + 1;
        const lastAttempt = state.notificationTestResponse || {
            event_type: "notification.test",
            action: "notification.test",
            target_name: "webhook",
            outcome: "succeeded",
            success: true,
            attempts: 1,
            status_code: 202,
            attempted_at: "2026-05-17T12:00:00Z",
            completed_at: "2026-05-17T12:00:00Z",
            delivered_at: "2026-05-17T12:00:00Z",
            duration_ms: 125,
            consecutive_failures: 0,
        };
        state.notificationDiagnosticsResponse = lastAttempt;
        const body = {
            last_attempt: lastAttempt,
            last_delivery: lastAttempt,
        };
        if (state.notificationTestFailure) {
            return route.fulfill({
                status: 400,
                contentType: "application/json",
                body: JSON.stringify({
                    error: "notification test failed",
                    ...body,
                }),
            });
        }
        return fulfillJson(route, body);
    });
    await page.route("**/api/metrics/token", (route) => {
        const method = route.request().method();
        if (method === "GET") {
            state.metricsLoadCount = (state.metricsLoadCount || 0) + 1;
            if ((state.metricsFailuresRemaining || 0) > 0) {
                state.metricsFailuresRemaining -= 1;
                return route.fulfill({
                    status: 503,
                    contentType: "application/json",
                    body: JSON.stringify({
                        error: "metrics credential unavailable",
                    }),
                });
            }
            return fulfillJson(
                route,
                state.metricsResponse || {
                    enabled: true,
                    lifecycle_state: "current",
                    created_at: "2026-05-01T12:00:00Z",
                    rotated_at: "2026-05-15T12:00:00Z",
                    last_used_at: "2026-07-27T12:00:00Z",
                    last_used_origin_masked: "192.0.2.x",
                    stale_after_days: 30,
                },
            );
        }
        if (method === "POST") {
            state.metricsRotateCount = (state.metricsRotateCount || 0) + 1;
            const response = state.metricsRotateResponse || {
                enabled: true,
                lifecycle_state: "never_used",
                created_at: "2026-05-01T12:00:00Z",
                rotated_at: "2026-07-28T12:00:00Z",
                last_used_at: "",
                last_used_origin_masked: "",
                stale_after_days: 30,
                token: "one-time-metrics-token",
            };
            state.metricsResponse = { ...response };
            delete state.metricsResponse.token;
            return fulfillJson(route, response);
        }
        if (method === "DELETE") {
            state.metricsDisableCount = (state.metricsDisableCount || 0) + 1;
            state.metricsResponse = {
                enabled: false,
                lifecycle_state: "disabled",
                created_at: "",
                rotated_at: "",
                last_used_at: "",
                last_used_origin_masked: "",
                stale_after_days: 30,
            };
            return fulfillJson(route, state.metricsResponse);
        }
        return route.fulfill({ status: 405 });
    });
    await page.route("**/api/audit-events?*", (route) => {
        state.adminActivityLoadCount = (state.adminActivityLoadCount || 0) + 1;
        state.adminActivityURLs = [
            ...(state.adminActivityURLs || []),
            route.request().url(),
        ];
        if ((state.adminActivityFailuresRemaining || 0) > 0) {
            state.adminActivityFailuresRemaining -= 1;
            return route.fulfill({
                status: 503,
                contentType: "application/json",
                body: JSON.stringify({ error: "recent activity unavailable" }),
            });
        }
        return fulfillJson(
            route,
            state.adminActivityResponse || {
                items: [
                    {
                        id: 82,
                        created_at: "2026-05-17T12:05:00Z",
                        created_at_display:
                            "2026-05-17 08:05 EDT · America/Toronto",
                        actor: "admin",
                        action: "auth.session.ip_reveal",
                        status: "success",
                        message: "DO-NOT-RENDER-MESSAGE",
                        meta_json: '{"password":"DO-NOT-RENDER-META"}',
                        request_id: "DO-NOT-RENDER-REQUEST",
                        client_ip: "203.0.113.44",
                        target_name: "DO-NOT-RENDER-TARGET",
                    },
                    {
                        id: 81,
                        created_at: "2026-05-17T12:00:00Z",
                        created_at_display:
                            "2026-05-17 08:00 EDT · America/Toronto",
                        actor: "scheduler",
                        action: "update_policy.update",
                        status: "failure",
                    },
                ],
                total: 2,
                page: 1,
                page_size: 8,
            },
        );
    });
    await page.route("**/api/backup/status", (route) => {
        state.backupLoadCount = (state.backupLoadCount || 0) + 1;
        if ((state.backupFailuresRemaining || 0) > 0) {
            state.backupFailuresRemaining -= 1;
            return route.fulfill({
                status: 503,
                contentType: "application/json",
                body: JSON.stringify({ error: "backup unavailable" }),
            });
        }
        return fulfillJson(
            route,
            state.backupStatusResponse || {
                db_path: "/tmp/simplelinuxupdater.db",
                backup_supported: true,
                known_hosts_path: "/tmp/known_hosts",
                recovery_health: backupRecoveryHealthFixture("healthy"),
            },
        );
    });
    await page.route("**/api/backup/restore", async (route) => {
        state.restoreCount = (state.restoreCount || 0) + 1;
        if (state.deferRestore) {
            await new Promise((resolve) => {
                state.releaseRestore = resolve;
            });
        }
        state.restoreCompleted = true;
        return fulfillJson(route, {
            restored: true,
            sessions_invalidated: false,
        });
    });
    await page.route("**/api/backup/verify", async (route) => {
        state.verifyCount = (state.verifyCount || 0) + 1;
        return fulfillJson(
            route,
            state.verifyResponse || {
                valid: true,
                compatible: true,
                restore_ready: true,
                archive: {
                    format: "simplelinuxupdater-backup",
                    version: 1,
                    created_at: "2026-05-17T06:00:00Z",
                    size_bytes: 4096,
                },
                manifest_files: 3,
                resources: [
                    {
                        name: "servers.db",
                        size_bytes: 3072,
                        required: true,
                        included: true,
                    },
                    {
                        name: "config.json",
                        size_bytes: 256,
                        required: true,
                        included: true,
                    },
                    {
                        name: "known_hosts",
                        size_bytes: 0,
                        required: false,
                        included: false,
                    },
                ],
                missing_resources: [],
                safe_counts: { servers: 5, policies: 2, jobs: 14, sessions: 3 },
                impact: {
                    sessions_invalidated: true,
                    metrics_access_replaced: true,
                    maintenance_required: true,
                    downtime_expected: true,
                    restart_required: false,
                },
                blockers: [],
                warnings: [
                    {
                        code: "sessions_invalidated",
                        message:
                            "All active Admin sessions will be invalidated after replacement.",
                    },
                    {
                        code: "metrics_access_replaced",
                        message:
                            "The current Metrics API credential will be replaced by the archived state.",
                    },
                    {
                        code: "known_hosts_not_included",
                        message:
                            "known_hosts is not included; the current host-key trust file will remain unchanged.",
                    },
                ],
                known_hosts_included: false,
                created_at: "2026-05-17T06:00:00Z",
            },
        );
    });
    await page.route("**/api/update-policies/settings", (route) =>
        fulfillJson(route, {
            timezone: "America/Toronto",
            resolved_timezone: "America/Toronto",
            global_blackouts: [],
        }),
    );
    await page.route("**/api/update-policies/runs?*", (route) => {
        state.scheduledRunsLoadCount = (state.scheduledRunsLoadCount || 0) + 1;
        const requestURL = new URL(route.request().url());
        const query = Object.fromEntries(requestURL.searchParams.entries());
        state.scheduledRunsQueries = [
            ...(state.scheduledRunsQueries || []),
            query,
        ];
        const filtered =
            query.policy ||
            query.server ||
            query.outcome ||
            query.from ||
            query.to;
        const pageNumber = Number(query.page || 1);
        const item = filtered
            ? {
                  id: 8,
                  policy_name: "Nightly security",
                  server_name: "srv-web-02",
                  status: "skipped",
                  terminal_outcome: "skipped",
                  reason: "busy",
                  exact_skip_reason: "busy",
                  summary: "Server already has an active operation",
                  scheduled_for_utc: "2026-05-18T06:00:00Z",
                  started_at: "2026-05-18T06:00:00Z",
                  finished_at: "2026-05-18T06:00:30Z",
                  duration_ms: 30000,
                  audit_url:
                      "/manage?audit_action=schedule.run.skipped&audit_target=srv-web-02#audit-trail",
              }
            : pageNumber === 2
              ? {
                    id: 6,
                    policy_name: "Older policy",
                    server_name: "srv-web-09",
                    status: "failed",
                    terminal_outcome: "failed",
                    summary: "Upgrade failed",
                    scheduled_for_utc: "2026-05-16T06:00:00Z",
                    started_at: "2026-05-16T06:00:00Z",
                    finished_at: "2026-05-16T06:02:00Z",
                    duration_ms: 120000,
                    audit_url:
                        "/manage?audit_action=schedule.run.failed&audit_target=srv-web-09#audit-trail",
                }
              : {
                    id: 7,
                    policy_name: "Nightly security",
                    server_name: "srv-web-01",
                    status: "succeeded",
                    terminal_outcome: "succeeded",
                    summary: "completed",
                    job_id: "job-report-1",
                    scheduled_for_utc: "2026-05-17T06:00:00Z",
                    started_at: "2026-05-17T06:00:00Z",
                    finished_at: "2026-05-17T06:01:30Z",
                    duration_ms: 90000,
                    job_detail_url: "/api/jobs/job-report-1",
                    report_url: "/api/reports/jobs/job-report-1",
                    audit_url:
                        "/manage?audit_action=schedule.run.completed&audit_target=srv-web-01#audit-trail",
                };
        return fulfillJson(route, {
            timezone: "America/Toronto",
            items: [item],
            page: pageNumber,
            page_size: Number(query.page_size || 25),
            total: filtered ? 1 : 26,
            total_pages: filtered ? 1 : 2,
        });
    });
    await page.route("**/api/update-policies/calendar?*", (route) => {
        state.calendarLoadCount = (state.calendarLoadCount || 0) + 1;
        return fulfillJson(route, {
            days: 14,
            start_date: "2026-05-17",
            end_date: "2026-05-30",
            generated_at: "2026-05-17T12:00:00Z",
            timezone: "America/Toronto",
            resolved_timezone: "America/Toronto",
            policies: [
                {
                    id: 12,
                    name: "Nightly security",
                    enabled: true,
                    cadence_kind: "daily",
                    time_local: "02:00",
                    weekdays: [],
                    matched_servers: ["srv-web-01"],
                    days: [
                        {
                            date: "2026-05-17",
                            weekday: "sun",
                            timezone_offset: "-04:00",
                            allowed_slots: [
                                {
                                    time_local: "02:00",
                                    scheduled_for_utc: "2026-05-17T06:00:00Z",
                                    timezone_offset: "-04:00",
                                    execution_mode: "approval_required",
                                    package_scope: "security",
                                    upgrade_mode: "standard",
                                    matched_servers: ["srv-web-01"],
                                },
                            ],
                            blocked_windows: [
                                {
                                    source: "global",
                                    weekdays: ["sat"],
                                    start_time: "23:00",
                                    end_time: "03:00",
                                    overnight: true,
                                    applies_to_slot: false,
                                },
                            ],
                        },
                    ],
                },
            ],
        });
    });
    await page.route("**/api/jobs/job-report-1", (route) =>
        fulfillJson(route, {
            report_url: "/api/reports/jobs/job-report-1",
            job: {
                id: "job-report-1",
                kind: "update",
                parent_job_id: "",
                server_name: "srv-web-01",
                actor: "admin",
                client_ip: "127.0.0.1",
                status: "succeeded",
                phase: "complete",
                summary: "completed",
                logs_text: "apt update\nupgrade completed",
                error_class: "",
                retry_policy_json: '{"max_attempts":3,"backoff_seconds":30}',
                meta_json: '{"packages":2}',
                created_at: "2026-05-17T06:00:00Z",
                updated_at: "2026-05-17T06:05:00Z",
                started_at: "2026-05-17T06:00:05Z",
                finished_at: "2026-05-17T06:05:00Z",
            },
        }),
    );
    await page.route("**/api/update-policies/preview", async (route) => {
        state.policyPreviewPayload = await route.request().postDataJSON();
        state.policyPreviewCount = (state.policyPreviewCount || 0) + 1;
        const timezone = state.timezoneSave?.timezone || "America/Toronto";
        const fixedOffset = timezone === "+05:30";
        const defaultPreview = {
            matched_servers: [
                { name: "srv-web-01", tags: ["prod", "web"] },
                { name: "srv-web-02", tags: ["prod"] },
            ],
            excluded_servers: [
                {
                    name: "srv-db-01",
                    tags: ["prod", "db"],
                    reason: "excluded_tag",
                },
                {
                    name: "srv-paused",
                    tags: ["prod"],
                    reason: "server_disabled",
                },
            ],
            disabled_by_override: [],
            warnings: [
                'Explicit server "srv-missing" is not in the current inventory.',
            ],
            validation_errors: [],
            operational_warnings: [
                {
                    code: "missing_explicit_server",
                    message:
                        'Explicit server "srv-missing" is not in the current inventory.',
                },
            ],
            informational_facts: [
                {
                    code: "application_timezone",
                    message: `Occurrences use the canonical application timezone ${timezone}.`,
                },
            ],
            upcoming_occurrences: [
                {
                    local_civil_time: fixedOffset
                        ? "2026-05-18 03:45"
                        : "2026-05-17 03:45",
                    timezone,
                    offset: fixedOffset ? "+05:30" : "-04:00",
                    abbreviation: fixedOffset ? "+05:30" : "EDT",
                    scheduled_for_utc: fixedOffset
                        ? "2026-05-17T22:15:00.000000000Z"
                        : "2026-05-17T07:45:00.000000000Z",
                    dst_status: "standard",
                    canonical_choice: "exact",
                    matched_server_count: 2,
                    applicable_no_run_windows: [],
                    admission_outcome: "admitted",
                },
            ],
        };
        return fulfillJson(
            route,
            state.policyPreviewResponse || defaultPreview,
        );
    });
    await page.route("**/api/update-policies", async (route) => {
        if (route.request().method() === "POST") {
            state.policyPayload = await route.request().postDataJSON();
            return fulfillJson(route, {
                id: 42,
                ...state.policyPayload,
                matched_servers: ["srv-web-01"],
            });
        }
        return fulfillJson(route, {
            timezone: "America/Toronto",
            items: state.policies || [
                {
                    id: 12,
                    name: "Nightly security",
                    enabled: true,
                    target_tag: "prod",
                    include_tags: ["web"],
                    exclude_tags: ["hold"],
                    target_servers: ["srv-web-01"],
                    package_scope: "security",
                    execution_mode: "approval_required",
                    cadence_kind: "daily",
                    time_local: "02:00",
                    weekdays: [],
                    matched_servers: ["srv-web-01"],
                },
            ],
        });
    });
    await page.route("**/api/update-policies/*", async (route) => {
        if (route.request().method() === "DELETE") {
            state.policyDeleteCount = (state.policyDeleteCount || 0) + 1;
            return fulfillJson(route, { ok: true });
        }
        return route.fallback();
    });
}

async function stubManageApi(page, state = {}) {
    const onboardingReport = () => ({
        ready: state.onboardingReady !== false,
        distribution: "ubuntu 24.04",
        disk_available_kb: 5000000,
        checks: [
            {
                id: "ssh",
                status: "passed",
                message:
                    "Authenticated SSH connection with the confirmed fingerprint.",
            },
            { id: "distribution", status: "passed", message: "ubuntu 24.04" },
            {
                id: "apt",
                status: "passed",
                message: "APT and dpkg are available.",
            },
            {
                id: "sudo",
                status: state.onboardingReady === false ? "failed" : "passed",
                message:
                    state.onboardingReady === false
                        ? "Maintenance permissions are missing."
                        : "Maintenance permissions verified.",
                remediation:
                    state.onboardingReady === false
                        ? "Install the managed root helper and sudoers policy."
                        : "",
            },
            {
                id: "disk",
                status: "passed",
                message: "At least 4.8 GiB available across /, /var and /boot.",
            },
            {
                id: "packages",
                status: "passed",
                message: "No unfinished dpkg configuration detected.",
            },
        ],
    });
    await page.route("**/api/servers/onboarding/check", async (route) => {
        state.onboardingCheckCount = (state.onboardingCheckCount || 0) + 1;
        return fulfillJson(route, onboardingReport());
    });
    await page.route("**/api/servers/onboarding", async (route) => {
        state.onboardingCreateCount = (state.onboardingCreateCount || 0) + 1;
        state.onboardingDraft = route.request().postDataJSON();
        if (state.onboardingReady === false)
            return route.fulfill({
                status: 422,
                contentType: "application/json",
                body: JSON.stringify({
                    error: "Verification failed.",
                    report: onboardingReport(),
                }),
            });
        const { name, host, port, user, tags } = state.onboardingDraft;
        return fulfillJson(route, { name, host, port, user, tags }, 201);
    });
    await page.route("**/api/servers", (route) => {
        state.inventoryLoadCount = (state.inventoryLoadCount || 0) + 1;
        return fulfillJson(route, state.servers || [makeServer("demo-host")]);
    });
    await page.route("**/api/servers/*/key", async (route) => {
        if (route.request().method() === "POST") {
            state.uploadServerKeyCount = (state.uploadServerKeyCount || 0) + 1;
            if (state.failServerKeyUpload) {
                return route.fulfill({
                    status: 422,
                    contentType: "application/json",
                    body: JSON.stringify({ error: "replacement key rejected" }),
                });
            }
            return fulfillJson(route, { ok: true });
        }
        return route.fallback();
    });
    await page.route("**/api/servers/*", async (route) => {
        if (route.request().method() === "PUT") {
            const update = route.request().postDataJSON();
            const existing = (state.servers || [makeServer("demo-host")])[0];
            state.serverUpdateCount = (state.serverUpdateCount || 0) + 1;
            state.servers = [
                {
                    ...existing,
                    ...update,
                    has_password: !!update.pass || !!existing.has_password,
                },
            ];
            return fulfillJson(route, state.servers[0]);
        }
        if (route.request().method() === "DELETE") {
            state.deleteServerCount = (state.deleteServerCount || 0) + 1;
            state.deletedServerUrl = route.request().url();
            return fulfillJson(route, { ok: true });
        }
        return route.fallback();
    });
    await page.route("**/api/keys/global", async (route) => {
        if (route.request().method() === "DELETE") {
            state.clearGlobalKeyCount = (state.clearGlobalKeyCount || 0) + 1;
            state.hasGlobalKey = false;
            return fulfillJson(route, { ok: true });
        }
        return fulfillJson(route, {
            has_key: state.hasGlobalKey ?? true,
            private_key: "DO-NOT-RENDER-PRIVATE-KEY",
        });
    });
    await page.route("**/api/hostkeys/scan", (route) => {
        const hostKeyState = state.hostKeyState || "trusted";
        if (hostKeyState === "error") {
            return route.fulfill({
                status: 502,
                contentType: "application/json",
                body: JSON.stringify({ error: "remote host unavailable" }),
            });
        }
        return fulfillJson(route, {
            host: "demo-host.example.test",
            port: 22,
            algorithm: "ssh-ed25519",
            fingerprint_sha256:
                hostKeyState === "changed" ? "SHA256:new" : "SHA256:trusted",
            already_trusted: hostKeyState === "trusted",
            host_entry_exists:
                hostKeyState === "trusted" || hostKeyState === "changed",
        });
    });
    await page.route("**/api/hostkeys/trust", async (route) => {
        state.trustHostKeyCount = (state.trustHostKeyCount || 0) + 1;
        state.hostKeyState = "trusted";
        return fulfillJson(route, {
            host: "demo-host.example.test",
            port: 22,
            fingerprint_sha256: "SHA256:trusted",
            already_trusted: false,
        });
    });
    await page.route("**/api/hostkeys/clear", async (route) => {
        state.clearKnownHostCount = (state.clearKnownHostCount || 0) + 1;
        state.hostKeyState = "missing";
        return fulfillJson(route, { removed_entries: 1 });
    });
    await page.route("**/api/audit-events/prune", async (route) => {
        state.auditPruneCount = (state.auditPruneCount || 0) + 1;
        return fulfillJson(route, { deleted: 3 });
    });
    await page.route("**/api/audit-events*", (route) => {
        state.auditListUrls = [
            ...(state.auditListUrls || []),
            route.request().url(),
        ];
        return fulfillJson(route, {
            items: [
                {
                    id: 55,
                    created_at: "2026-05-17T12:00:00Z",
                    created_at_display: "2026-05-17 08:00:00 America/Toronto",
                    actor: "admin",
                    action: "server.delete",
                    target_type: "server",
                    target_name: "demo-host",
                    status: "success",
                    message: "Deleted server",
                    meta_json: '{"scope":"security","count":2}',
                    request_id: "req-55",
                    client_ip: "127.0.0.1",
                },
            ],
            total: 1,
            page: 1,
            page_size: 20,
        });
    });
    await page.route("**/api/update-policies", (route) =>
        fulfillJson(route, {
            items: state.policies || [
                {
                    id: 9,
                    name: "Prod security",
                    target_tag: "prod",
                    include_tags: ["web"],
                    exclude_tags: ["hold"],
                    matched_servers: ["demo-host"],
                },
            ],
        }),
    );
    await page.route("**/api/update-policies/*/overrides", (route) =>
        fulfillJson(route, { items: [] }),
    );
    await page.route("**/api/update-policies/*/overrides/*", async (route) => {
        state.policyOverrideSaveCount =
            (state.policyOverrideSaveCount || 0) + 1;
        if (state.failPolicyOverrideSave) {
            return route.fulfill({
                status: 503,
                contentType: "application/json",
                body: JSON.stringify({ error: "policy override unavailable" }),
            });
        }
        return fulfillJson(route, { ok: true });
    });
}

async function capturePolicy472Panel(page, locator, name, mobile = false) {
    const dir = path.join(process.cwd(), ".github/assets/policy-schedules-472");
    fs.mkdirSync(dir, { recursive: true });
    await page.setViewportSize({ width: mobile ? 390 : 1440, height: 1800 });
    await locator.evaluate((element) =>
        window.scrollTo(
            0,
            Math.max(
                0,
                window.scrollY + element.getBoundingClientRect().top - 420,
            ),
        ),
    );
    await locator.screenshot({ path: path.join(dir, name) });
}

async function prepareImportConfigureResult(page) {
    await ensureAuthenticatedSession(page);
    const state = { servers: [makeServer("other-editor")] };
    await stubManageApi(page, state);
    const entry = {
        row: 1,
        name: "review-import",
        host: "192.0.2.222",
        port: 22,
        user: "root",
        tags: [],
        errors: [],
    };
    await page.route("**/api/servers/import/preview?format=json", (route) =>
        fulfillJson(route, { batch_id: "a".repeat(32), rows: [entry] }),
    );
    await page.route("**/api/servers/import", (route) => {
        state.servers.push(
            makeServer(entry.name, "idle", [], { ...entry, disabled: true }),
        );
        return fulfillJson(
            route,
            {
                outcome: "success",
                preview: { batch_id: "a".repeat(32), rows: [entry] },
                created: [entry.name],
            },
            201,
        );
    });
    await page.goto("/manage#manage-section-import");
    await expect(page.locator("#manage-servers-table tbody")).toContainText(
        "other-editor",
    );
    await page
        .locator("#inventory-import-file")
        .setInputFiles({
            name: "review.json",
            mimeType: "application/json",
            buffer: Buffer.from("[]"),
        });
    await page.locator("#import-preview-button").click();
    await expect(page.locator("#import-preview")).toBeVisible();
    await page.locator("#import-review-button").click();
    return state;
}

async function captureDeferral473(page, locator, name, mobile = false) {
    if (!process.env.CAPTURE_PACKAGE_DEFERRALS_473) return;
    const dir = path.join(
        process.cwd(),
        ".github/assets/package-deferrals-473",
    );
    fs.mkdirSync(dir, { recursive: true });
    await page.setViewportSize({
        width: mobile ? 390 : 1440,
        height: mobile ? 2400 : 1800,
    });
    if (
        mobile &&
        (await locator.getAttribute("id")) === "server-package-deferrals"
    ) {
        await page.setViewportSize({ width: 390, height: 1100 });
        const scrollTo = async (selector) =>
            page
                .locator("#edit-modal .modal-grid")
                .evaluate((scroller, selector) => {
                    const target = document.querySelector(selector);
                    scroller.scrollTop +=
                        target.getBoundingClientRect().top -
                        scroller.getBoundingClientRect().top -
                        12;
                }, selector);
        await scrollTo("#server-package-deferrals [data-deferral-list]");
        await page
            .locator("#edit-modal .modal")
            .screenshot({ path: path.join(dir, "mobile-manage-rules.png") });
        await scrollTo("#server-package-deferrals .deferral-fields");
        await page
            .locator("#edit-modal .modal")
            .screenshot({ path: path.join(dir, name) });
        return;
    }
    await locator.evaluate((element) =>
        element.scrollIntoView({ block: "center", behavior: "instant" }),
    );
    await locator.screenshot({ path: path.join(dir, name) });
}
module.exports = {
    fulfillJson,
    backupRecoveryHealthFixture,
    dismissTypedConfirm,
    acceptTypedConfirm,
    makeTimeline,
    makeDashboardSummary,
    makeHealthTrends,
    stubDashboardApi,
    makeServer,
    makePendingUpdates,
    stubAdminApi,
    stubManageApi,
    capturePolicy472Panel,
    prepareImportConfigureResult,
    captureDeferral473,
};
