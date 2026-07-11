(function initDashboardProjectionConsumption(root, factory) {
    const api = factory();
    if (typeof module === "object" && module.exports) module.exports = api;
    if (root) root.DashboardProjectionConsumption = api;
}(typeof globalThis !== "undefined" ? globalThis : this, function dashboardProjectionConsumptionFactory() {
    "use strict";

    const activeStatuses = new Set(["updating", "upgrading", "autoremove", "sudoers", "facts_refresh"]);
    const nonFailedStatuses = new Set(["idle", "updating", "pending_approval", "approved", "upgrading", "autoremove", "sudoers", "facts_refresh", "done"]);
    const fallbackRiskOrder = Object.freeze({ critical: 4, high: 4, elevated: 3, warning: 2, normal: 1, routine: 1 });

    function clone(value) {
        if (Array.isArray(value)) return value.map(clone);
        if (!value || typeof value !== "object") return value;
        return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, clone(item)]));
    }

    function deepFreeze(value, seen = new Set()) {
        if (!value || typeof value !== "object" || seen.has(value)) return value;
        seen.add(value);
        Object.values(value).forEach(item => deepFreeze(item, seen));
        return Object.freeze(value);
    }

    function record(value) {
        return value && typeof value === "object" && !Array.isArray(value) ? value : {};
    }

    function list(value) {
        return Array.isArray(value) ? value : [];
    }

    function text(value, fallback = "") {
        const normalized = String(value === null || value === undefined ? "" : value).trim();
        return normalized || fallback;
    }

    function count(value, fallback = 0) {
        const parsed = Number(value);
        return Number.isFinite(parsed) && parsed >= 0 ? parsed : fallback;
    }

    function canonicalCount(source, key, fallback) {
        return Object.hasOwn(record(source), key) ? count(source[key], fallback) : fallback;
    }

    function statusLabel(value) {
        return text(value, "unknown").replace(/_/g, " ");
    }

    function runningTimeline(state) {
        return ["active", "queued"].includes(text(state).toLowerCase());
    }

    function pendingApprovalCounts(server) {
        const pending = list(server.pending_updates);
        const plan = record(server.upgrade_plan);
        const planStandard = Number(plan.standard_package_count);
        const planKeptBack = Number(plan.kept_back_package_count);
        const planStandardSecurity = Number(plan.standard_security_count);
        const planTotalSecurity = Number(plan.total_security_count);
        const hasPlan = Number.isFinite(planStandard)
            && Number.isFinite(planKeptBack)
            && (planStandard > 0 || planKeptBack > 0 || count(plan.full_upgrade_package_count) > 0);
        const pendingSecurity = pending.filter(update => !!record(update).security);
        const keptSecurity = pendingSecurity.filter(update => !!record(update).kept_back || !!record(update).requires_full_upgrade);
        const upgradableFallback = list(server.upgradable).length;
        const total = pending.length > 0 ? pending.length : upgradableFallback;
        const standardSecurity = Math.max(0, pendingSecurity.length - keptSecurity.length);

        return {
            total,
            standard: hasPlan ? planStandard : total,
            keptBack: hasPlan ? planKeptBack : pending.filter(update => !!record(update).kept_back || !!record(update).requires_full_upgrade).length,
            full: count(plan.full_upgrade_package_count) || total,
            security: hasPlan && Number.isFinite(planStandardSecurity) ? planStandardSecurity : (pending.length > 0 ? standardSecurity : null),
            totalSecurity: hasPlan && Number.isFinite(planTotalSecurity) ? planTotalSecurity : (pending.length > 0 ? pendingSecurity.length : null),
            keptBackSecurity: hasPlan && Number.isFinite(planTotalSecurity) && Number.isFinite(planStandardSecurity)
                ? Math.max(0, planTotalSecurity - planStandardSecurity)
                : (pending.length > 0 ? keptSecurity.length : 0),
            fullPlanAvailable: !!plan.full_upgrade_plan_available,
            keptBackSecurityPlanAvailable: !!plan.kept_back_security_plan_available,
            newPackages: clone(list(plan.full_upgrade_new_packages)),
            removedPackages: clone(list(plan.full_upgrade_removed_packages)),
            keptBackSecurityNewPackages: clone(list(plan.kept_back_security_new_packages)),
            keptBackSecurityRemovedPackages: clone(list(plan.kept_back_security_removed_packages))
        };
    }

    function defaultTimeline() {
        return { current_phase: "", current_label: "Idle", state: "idle", progress_pct: 0, summary: "No maintenance activity", phases: [] };
    }

    function normalizedTimeline(value) {
        const source = record(value);
        if (Object.keys(source).length === 0) return defaultTimeline();
        return {
            ...clone(source),
            current_phase: text(source.current_phase),
            current_label: text(source.current_label, "Idle"),
            state: text(source.state, "idle").toLowerCase(),
            progress_pct: Math.max(0, Math.min(100, count(source.progress_pct))),
            summary: text(source.summary, "No maintenance activity"),
            phases: clone(list(source.phases))
        };
    }

    function actionFor(actionViews, key) {
        const action = record(record(actionViews)[key]);
        return Object.hasOwn(action, "enabled") ? clone(action) : null;
    }

    function actionEnabled(actionViews, key, fallback) {
        const action = actionFor(actionViews, key);
        return action ? !!action.enabled : !!fallback;
    }

    function authFacts(server, globalKeyAvailable) {
        const usesGlobalKey = !!globalKeyAvailable && !server.has_key;
        const effectiveKey = !!server.has_key || usesGlobalKey;
        let label = "No auth configured";
        if (server.has_key && server.has_password) label = "Server key + password";
        else if (usesGlobalKey && server.has_password) label = "Global SSH key + password";
        else if (server.has_key) label = "Server key";
        else if (usesGlobalKey) label = "Global SSH key";
        else if (server.has_password) label = "Password";
        return { label, effectiveKey, usesGlobalKey };
    }

    function failureTimestamp(intelligence, timeline) {
        const candidates = [
            record(intelligence.last_failed_update).finished_at,
            record(intelligence.last_failed).finished_at,
            record(intelligence.last_failure).finished_at,
            record(intelligence.last_update).finished_at,
            timeline.updated_at
        ];
        for (const value of candidates) {
            const parsed = value ? new Date(value).getTime() : 0;
            if (Number.isFinite(parsed) && parsed > 0) return parsed;
        }
        return 0;
    }

    function newestFirst(items, timestampKey) {
        return list(items)
            .map((item, index) => ({ item: clone(record(item)), index }))
            .sort((left, right) => {
                const leftTime = new Date(left.item[timestampKey] || "").getTime();
                const rightTime = new Date(right.item[timestampKey] || "").getTime();
                const safeLeft = Number.isFinite(leftTime) ? leftTime : 0;
                const safeRight = Number.isFinite(rightTime) ? rightTime : 0;
                return safeRight - safeLeft || left.index - right.index;
            })
            .map(entry => entry.item);
    }

    function projectServer(serverValue, intelligenceValue, actionViewsValue, globalKeyAvailable, inFlightNames) {
        const server = clone(record(serverValue));
        const intelligence = clone(record(intelligenceValue));
        const actionViews = record(actionViewsValue);
        const timeline = normalizedTimeline(intelligence.timeline);
        const rawTriage = record(intelligence.approval_triage);
        const rawRisk = record(intelligence.risk);
        const health = clone(record(intelligence.health));
        const approvalCounts = pendingApprovalCounts(server);
        const pendingUpdates = list(server.pending_updates);
        const runtimePending = text(server.status).toLowerCase() === "pending_approval";
        const timelinePending = text(timeline.current_phase).toLowerCase() === "pending_approval";
        const pendingApproval = runtimePending || timelinePending;
        const pendingPackages = canonicalCount(rawTriage, "pending_packages", approvalCounts.total);
        const securityFallback = pendingUpdates.length > 0 ? pendingUpdates.filter(update => !!record(update).security).length : 0;
        const securityUpdates = canonicalCount(rawTriage, "security_updates", securityFallback);
        const cveFallback = pendingUpdates.reduce((sum, update) => sum + list(record(update).cves).length, 0);
        const cveCount = canonicalCount(rawTriage, "cve_count", cveFallback);
        let riskLabel = text(rawTriage.risk_label) || text(rawRisk.summary);
        if (!riskLabel) {
            if (cveCount > 0) riskLabel = `${cveCount} CVE`;
            else if (securityUpdates > 0) riskLabel = `${securityUpdates} security`;
            else if (pendingPackages > 0) riskLabel = "Package updates";
            else riskLabel = "Normal";
        }
        const riskLevel = (text(rawTriage.risk_level) || text(rawRisk.level) || "normal").toLowerCase();
        const riskOrder = canonicalCount(rawTriage, "risk_order", fallbackRiskOrder[riskLevel] || 0);
        const transientFallback = !["updating", "pending_approval", "approved", "upgrading", "autoremove", "sudoers", "facts_refresh"].includes(text(server.status).toLowerCase());
        const triage = {
            ...clone(rawTriage),
            eligible: Object.hasOwn(rawTriage, "eligible") ? !!rawTriage.eligible : (pendingApproval && pendingPackages > 0),
            pending_packages: pendingPackages,
            security_updates: securityUpdates,
            cve_count: cveCount,
            standard_packages: canonicalCount(rawTriage, "standard_packages", approvalCounts.standard),
            kept_back_packages: canonicalCount(rawTriage, "kept_back_packages", approvalCounts.keptBack),
            standard_security_updates: canonicalCount(rawTriage, "standard_security_updates", approvalCounts.security || 0),
            kept_back_security_updates: canonicalCount(rawTriage, "kept_back_security_updates", approvalCounts.keptBackSecurity),
            risk_level: riskLevel,
            risk_label: riskLabel,
            risk_order: riskOrder,
            facts_state: text(rawTriage.facts_state, "unknown").toLowerCase(),
            last_check_display: text(rawTriage.last_check_display) || text(rawTriage.last_check_at) || "--",
            can_approve_all: actionEnabled(actionViews, "approve_all", runtimePending && approvalCounts.standard > 0),
            can_approve_security: actionEnabled(actionViews, "approve_security", runtimePending && (approvalCounts.security || 0) > 0),
            can_approve_kept_back_security: actionEnabled(actionViews, "approve_security_kept_back", runtimePending && approvalCounts.keptBackSecurity > 0 && approvalCounts.keptBackSecurityPlanAvailable),
            can_approve_full: actionEnabled(actionViews, "approve_full", runtimePending && approvalCounts.fullPlanAvailable),
            can_cancel: actionEnabled(actionViews, "cancel", runtimePending),
            can_refresh_facts: actionEnabled(actionViews, "refresh_facts", transientFallback),
            can_run_checks: actionEnabled(actionViews, "update", transientFallback)
        };
        const auth = authFacts(server, globalKeyAvailable);
        let driftReason = "";
        if (!runtimePending && timelinePending) {
            const summary = text(timeline.summary);
            const suffix = summary && summary.toLowerCase() !== "no maintenance activity"
                ? `: ${summary}`
                : ". Run a fresh update check or inspect logs before approving";
            driftReason = `Timeline is waiting for approval, but runtime status is ${statusLabel(server.status)}${suffix}`;
        }
        const failed = text(server.status).toLowerCase() === "error" || timeline.state === "error";
        const failureReason = failed
            ? [timeline.summary, record(intelligence.last_update).failure_cause, record(intelligence.last_failed_update).failure_cause, record(intelligence.last_failed).failure_cause, record(intelligence.last_failure).failure_cause, server.failure_cause]
                .map(value => text(value))
                .find(value => value && value.toLowerCase() !== "no maintenance activity") || "Completed with errors"
            : "";
        const actions = Object.fromEntries(["update", "autoremove", "enable_apt", "disable_apt", "refresh_facts", "approve_all", "approve_security", "approve_security_kept_back", "approve_full", "cancel"]
            .map(key => [key, actionFor(actionViews, key)]));

        return {
            name: text(server.name),
            server,
            intelligence,
            timeline,
            triage,
            health,
            nextRun: clone(record(intelligence.next_run)),
            noRun: clone(record(intelligence.no_run)),
            lastUpdate: clone(record(intelligence.last_update)),
            lastFailedUpdate: clone(record(intelligence.last_failed_update)),
            commandHistory: newestFirst(intelligence.command_history, "created_at"),
            approvalCounts,
            pendingApproval,
            runtimePending,
            timelinePending,
            hasPendingUpdates: pendingApproval && pendingUpdates.length > 0,
            risk: { label: riskLabel, level: riskLevel, order: riskOrder, cveCount, securityUpdates, pendingPackages },
            auth,
            driftReason,
            failed,
            failureReason,
            failureTimestamp: failureTimestamp(intelligence, timeline),
            active: activeStatuses.has(text(server.status).toLowerCase()) || runningTimeline(timeline.state),
            reachable: nonFailedStatuses.has(text(server.status).toLowerCase()) || (!!text(server.status) && text(server.status).toLowerCase() !== "error"),
            staleFacts: triage.facts_state === "stale",
            rebootRequired: health.reboot_required === true,
            busy: inFlightNames.has(text(server.name)) || !transientFallback,
            actions,
            canRunUpdate: actionEnabled(actionViews, "update", transientFallback),
            canRunAutoremove: actionEnabled(actionViews, "autoremove", transientFallback),
            canRunSudoers: actionEnabled(actionViews, "enable_apt", transientFallback),
            canRefreshFacts: actionEnabled(actionViews, "refresh_facts", transientFallback)
        };
    }

    function compareRisk(left, right) {
        return right.risk.order - left.risk.order
            || right.risk.cveCount - left.risk.cveCount
            || right.risk.securityUpdates - left.risk.securityUpdates
            || right.risk.pendingPackages - left.risk.pendingPackages
            || left.name.localeCompare(right.name);
    }

    function compareFailure(left, right) {
        return right.risk.order - left.risk.order
            || right.failureTimestamp - left.failureTimestamp
            || left.name.localeCompare(right.name);
    }

    function compareApproval(left, right) {
        return right.risk.order - left.risk.order
            || right.risk.pendingPackages - left.risk.pendingPackages
            || left.name.localeCompare(right.name);
    }

    function fleetProjection(models, fleetValue) {
        const fleet = record(fleetValue);
        const fallback = {
            pending_approval: models.filter(model => model.pendingApproval).length,
            in_progress: models.filter(model => model.active).length,
            done: models.filter(model => text(model.server.status).toLowerCase() === "done" || model.timeline.state === "done").length,
            pending_packages: models.reduce((sum, model) => sum + model.risk.pendingPackages, 0),
            security_updates: models.reduce((sum, model) => sum + model.risk.securityUpdates, 0),
            high_risk_cve: models.filter(model => model.risk.cveCount > 0).length,
            hosts_needing_reboot: models.filter(model => model.rebootRequired).length,
            stale_facts: models.filter(model => model.staleFacts).length
        };
        return {
            total: models.length,
            reachable: models.filter(model => model.reachable).length,
            failed: models.filter(model => model.failed).length,
            pendingApproval: canonicalCount(fleet, "pending_approval", fallback.pending_approval),
            active: canonicalCount(fleet, "in_progress", fallback.in_progress),
            done: canonicalCount(fleet, "done", fallback.done),
            pendingPackages: canonicalCount(fleet, "pending_packages", fallback.pending_packages),
            securityUpdates: canonicalCount(fleet, "security_updates", fallback.security_updates),
            highRiskCVE: canonicalCount(fleet, "high_risk_cve", fallback.high_risk_cve),
            rebootRequired: canonicalCount(fleet, "hosts_needing_reboot", fallback.hosts_needing_reboot),
            staleFacts: canonicalCount(fleet, "stale_facts", fallback.stale_facts)
        };
    }

    function authPosture(models) {
        const withKey = models.filter(model => model.auth.effectiveKey).length;
        const withServerKey = models.filter(model => !!model.server.has_key).length;
        const withGlobalKey = models.filter(model => model.auth.usesGlobalKey).length;
        const withPassword = models.filter(model => !!model.server.has_password).length;
        const missing = models.filter(model => !model.auth.effectiveKey && !model.server.has_password).length;
        const mixed = models.filter(model => model.auth.effectiveKey && !!model.server.has_password).length;
        let label = "--";
        if (models.length > 0 && missing > 0) label = "Gaps";
        else if (models.length > 0 && (mixed > 0 || (withKey > 0 && withPassword > 0))) label = "Mixed";
        else if (withKey > 0) label = "Key";
        else if (withPassword > 0) label = "Password";
        return { label, withKey, withServerKey, withGlobalKey, withPassword, missing };
    }

    function projectActivity(items) {
        return newestFirst(items, "created_at").map((item, index) => {
            const event = record(item);
            return {
                key: text(event.id, `${text(event.created_at)}:${index}`),
                status: text(event.status, "unknown").toLowerCase(),
                action: text(event.action, "activity"),
                target: [text(event.target_type), text(event.target_name)].filter(Boolean).join(": ") || text(event.message, "system"),
                createdAt: text(event.created_at),
                createdAtDisplay: text(event.created_at_display),
                raw: clone(event)
            };
        });
    }

    function project(inputValue) {
        const input = record(inputValue);
        const statusView = record(input.statusView);
        const dashboardSnapshot = record(statusView.dashboardSnapshot);
        const dashboardServers = list(statusView.dashboardServers);
        const dashboardByName = new Map(dashboardServers.map(item => [text(record(item).name), record(item)]));
        const actionViews = record(statusView.actionViews);
        const inFlightNames = new Set(list(record(statusView.actions).inFlightServerNames).map(name => text(name)));
        const models = list(statusView.servers).map(server => projectServer(server, dashboardByName.get(text(record(server).name)), actionViews[text(record(server).name)], !!input.globalKeyAvailable, inFlightNames));
        const byName = Object.fromEntries(models.map(model => [model.name, model]));
        const visibleModels = list(statusView.visibleServers).map(server => byName[text(record(server).name)]).filter(Boolean);
        const pageModels = list(statusView.pageServers).map(server => byName[text(record(server).name)]).filter(Boolean);
        const groups = list(statusView.groups).map(group => ({
            key: text(record(group).key),
            items: list(record(group).items).map(server => byName[text(record(server).name)]).filter(Boolean)
        }));
        const selectedHost = byName[text(statusView.primaryServerName)] || null;
        const failed = models.filter(model => model.failed).sort(compareFailure);
        const reboot = models.filter(model => model.rebootRequired).sort(compareRisk);
        const risk = models.filter(model => ["critical", "high", "elevated"].includes(model.risk.level)).sort(compareRisk);
        const approval = visibleModels.filter(model => model.triage.eligible || model.pendingApproval).sort(compareApproval);
        const scheduled = models
            .filter(model => text(model.nextRun.state).toLowerCase() === "scheduled")
            .sort((left, right) => text(left.nextRun.scheduled_for_utc).localeCompare(text(right.nextRun.scheduled_for_utc)) || left.name.localeCompare(right.name));
        const tagCounts = new Map();
        models.forEach(model => {
            const tags = list(model.server.tags).filter(Boolean);
            (tags.length ? tags : ["untagged"]).forEach(tag => tagCounts.set(tag, (tagCounts.get(tag) || 0) + 1));
        });
        const extras = record(input.extras);
        const observability = record(extras.observabilitySummary);
        const policySummary = Array.isArray(extras.policySummary) ? extras.policySummary : null;
        return deepFreeze({
            servers: models,
            serversByName: byName,
            visibleServers: visibleModels,
            pageServers: pageModels,
            groups,
            selectedHost,
            fleet: fleetProjection(models, dashboardSnapshot.fleet),
            auth: authPosture(models),
            panels: {
                active: models.filter(model => model.active),
                failed,
                reboot,
                risk,
                approval,
                scheduled,
                commandHistory: selectedHost ? selectedHost.commandHistory : [],
                recentActivity: projectActivity(extras.recentActivity),
                tags: Array.from(tagCounts.entries()).sort((left, right) => right[1] - left[1] || left[0].localeCompare(right[0])).map(([tag, total]) => ({ tag, total }))
            },
            summaries: {
                policyCount: policySummary ? policySummary.length : null,
                observabilityUpdates: count(record(observability.totals).updates_total),
                observabilitySuccessRate: count(record(observability.totals).success_rate_pct)
            }
        });
    }

    function projectBulkReview(planValue) {
        const plan = record(planValue);
        const eligible = clone(list(plan.eligibleHosts));
        const skipped = clone(list(plan.skippedHosts));
        const actionLabel = text(plan.actionLabel, "action");
        return deepFreeze({
            title: `Review bulk ${actionLabel}`,
            summary: `${eligible.length} eligible visible host${eligible.length === 1 ? "" : "s"} will run. ${skipped.length} host${skipped.length === 1 ? "" : "s"} will be skipped.`,
            eligibleLabel: `Eligible hosts (${eligible.length})`,
            skippedLabel: `Skipped hosts (${skipped.length})`,
            eligible,
            skipped,
            warning: text(plan.warning),
            canConfirm: eligible.length > 0
        });
    }

    function interactionApprovalCounts(server) {
        const counts = pendingApprovalCounts(server);
        return {
            ...counts,
            standardSecurity: counts.security ?? 0
        };
    }

    const presentationFacts = Object.freeze({
        approvalCounts: interactionApprovalCounts,
        authFacts
    });

    return Object.freeze({ project, projectBulkReview, presentationFacts });
}));
