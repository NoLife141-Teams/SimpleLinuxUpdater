const { test, expect } = require('@playwright/test');
const fs = require('node:fs');
const path = require('node:path');

test.describe.serial('setup and login flows', () => {
  const username = 'admin';
  const password = 'StrongPass1234';
  const changedPassword = 'NewStrongPass123';
  let knownWorkingPassword = password;
  let authCookies = [];

  async function rememberAuthCookies(page) {
    authCookies = await page.context().cookies('http://127.0.0.1:8080');
  }

  async function signIn(page) {
    await page.locator('#username').fill(username);
    await page.locator('#password').fill(knownWorkingPassword);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page).toHaveURL('http://127.0.0.1:8080/');
    await expect(page.locator('#logout-btn')).toBeVisible();
    await rememberAuthCookies(page);
  }

  async function ensureSignedIn(page) {
    await page.goto('/login');
    if (/\/login$/.test(page.url())) {
      await signIn(page);
      return;
    }
    await expect(page.locator('#logout-btn')).toBeVisible();
  }

  async function ensureAuthenticatedSession(page) {
    if (authCookies.length > 0) {
      await page.context().addCookies(authCookies);
    }
    await page.goto('/login');

    const status = await page.evaluate(async () => {
      const response = await fetch('/api/auth/status', { cache: 'no-store' });
      return response.json();
    });

    if (!status.authenticated) {
      const endpoint = status.setup_required ? '/api/auth/setup' : '/api/auth/login';
      const candidates = [...new Set([knownWorkingPassword, password, changedPassword])];
      let result = { ok: false, status: 0, payload: {} };
      if (status.setup_required) {
        for (const candidatePassword of candidates) {
          result = await page.evaluate(async ({ endpoint, username, password }) => {
            const response = await fetch(endpoint, {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ username, password }),
            });
            const payload = await response.json().catch(() => ({}));
            return { ok: response.ok, status: response.status, payload };
          }, { endpoint, username, password: candidatePassword });
          if (result.ok) {
            knownWorkingPassword = candidatePassword;
            break;
          }
        }
      } else {
        for (const candidatePassword of candidates) {
          await page.goto('/login');
          await page.locator('#username').fill(username);
          await page.locator('#password').fill(candidatePassword);
          await page.getByRole('button', { name: 'Sign in' }).click();
          await page.waitForURL('http://127.0.0.1:8080/', { timeout: 2500 }).catch(() => {});
          if (page.url() === 'http://127.0.0.1:8080/') {
            knownWorkingPassword = candidatePassword;
            result = { ok: true, status: 200, payload: {} };
            break;
          }
          result = { ok: false, status: 401, payload: {} };
        }
      }

      expect(result, `${endpoint} should create an authenticated test session`).toMatchObject({ ok: true });
    }

    await page.goto('/');
    await expect(page.locator('#logout-btn')).toBeVisible();
    await rememberAuthCookies(page);
  }

  async function fulfillJson(route, payload) {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(payload),
    });
  }

  function backupRecoveryHealthFixture(state = 'healthy') {
    const failed = state === 'failed';
    const never = state === 'never';
    const stale = state === 'stale';
    const unavailable = state === 'unavailable';
    return {
      state,
      message: {
        healthy: 'Recent successful backup export and verification evidence is available.',
        stale: 'Backup recovery evidence is older than the 168-hour threshold.',
        never: 'No successful backup export and verification have both been recorded.',
        failed: 'The latest backup export or verification did not produce accepted recovery evidence.',
        unavailable: 'Backup recovery evidence is unavailable.',
      }[state],
      checked_at: '2026-07-28T05:00:00Z',
      stale_after_hours: 168,
      export: {
        state: unavailable ? 'unavailable' : failed ? 'failed' : state,
        last_attempt_at: never || unavailable ? '' : failed ? '2026-07-28T04:00:00Z' : '2026-07-27T05:00:00Z',
        last_success_at: never || unavailable ? '' : '2026-07-27T05:00:00Z',
        size_bytes: never || unavailable ? null : 4096,
        message: failed ? 'Failed to build backup payload.' : never ? 'No backup export has been recorded.' : 'Backup exported.',
      },
      verification: {
        state: unavailable ? 'unavailable' : never ? 'never' : 'healthy',
        last_attempt_at: never || unavailable ? '' : '2026-07-27T17:00:00Z',
        last_success_at: never || unavailable ? '' : '2026-07-27T17:00:00Z',
        size_bytes: never || unavailable ? null : 4096,
        message: never ? 'No backup verification has been recorded.' : 'Backup restore readiness reviewed.',
      },
      schedule: { scheduled: false, message: 'No backup is scheduled.' },
      retention: {
        evidence_days: 90,
        archive_retained_by_app: false,
        automatic_deletion: false,
        evidence_description: 'Backup recovery evidence is retained in audit history for up to 90 days.',
        archive_description: 'Exported archives are downloaded to operator-managed storage and are not retained or deleted by the app.',
      },
    };
  }

  async function dismissTypedConfirm(page, trigger, wrongText = 'WRONG') {
    await trigger.click();
    await expect(page.locator('#typed-confirm-modal')).toBeVisible();
    await page.locator('#typed-confirm-input').fill(wrongText);
    await expect(page.locator('#typed-confirm-submit')).toBeDisabled();
    await page.locator('#typed-confirm-cancel').click();
    await expect(page.locator('#typed-confirm-modal')).not.toBeVisible();
  }

  async function acceptTypedConfirm(page, trigger, requiredText) {
    await trigger.click();
    await expect(page.locator('#typed-confirm-modal')).toBeVisible();
    await page.locator('#typed-confirm-input').fill(requiredText);
    await expect(page.locator('#typed-confirm-submit')).toBeEnabled();
    await page.locator('#typed-confirm-submit').click();
    await expect(page.locator('#typed-confirm-modal')).not.toBeVisible();
  }

  function makeTimeline(status) {
    const phases = [
      ['prechecks', 'Pre-checks'],
      ['apt_update', 'APT update'],
      ['pending_approval', 'Pending approval'],
      ['upgrade', 'Upgrade'],
      ['postchecks', 'Post-checks'],
      ['done_error', 'Done / Error'],
    ];
    const statusMap = {
      pending_approval: ['pending_approval', 'waiting', 60],
      updating: ['prechecks', 'active', 32],
      upgrading: ['upgrade', 'active', 72],
      done: ['done_error', 'done', 100],
      error: ['done_error', 'error', 100],
    };
    const [currentPhase, state, progress] = statusMap[status] || ['', 'idle', 0];
    const currentIndex = phases.findIndex(([key]) => key === currentPhase);
    return {
      current_phase: currentPhase,
      current_label: phases[currentIndex]?.[1] || 'Idle',
      state,
      progress_pct: progress,
      summary: state === 'idle' ? 'No maintenance activity' : `Runtime status: ${status}`,
      updated_at: '2026-05-28T12:00:00Z',
      updated_at_display: 'May 28, 2026 12:00',
      phases: phases.map(([key, label], index) => {
        let phaseState = 'pending';
        if (state === 'done') phaseState = 'done';
        else if (state === 'error') phaseState = index < currentIndex ? 'done' : (index === currentIndex ? 'error' : 'pending');
        else if (currentIndex >= 0) phaseState = index < currentIndex ? 'done' : (index === currentIndex ? state : 'pending');
        return { key, label, state: phaseState, progress_pct: index === currentIndex ? progress : 0 };
      }),
    };
  }

  function makeDashboardSummary(servers) {
    const dashboardServers = servers.map(server => {
      const pendingUpdates = Array.isArray(server.pending_updates) ? server.pending_updates : [];
      const cves = pendingUpdates.flatMap(update => update.cves || []);
      const securityUpdates = pendingUpdates.filter(update => update.security).length;
      const plan = server.upgrade_plan || {};
      const standardSecurity = Number.isFinite(Number(plan.standard_security_count)) ? Number(plan.standard_security_count) : securityUpdates;
      const totalSecurity = Number.isFinite(Number(plan.total_security_count)) ? Number(plan.total_security_count) : securityUpdates;
      const keptBackSecurity = Math.max(0, totalSecurity - standardSecurity);
      const riskLevel = cves.length > 0 ? 'critical' : (securityUpdates > 0 ? 'elevated' : 'normal');
      return {
        name: server.name,
        next_run: server.next_run || { state: 'none', summary: 'No scheduled run' },
        no_run: { active: false, summary: 'No no-run window active', timezone: 'UTC' },
        health: {
          source: server.facts_state === 'stale' ? 'unknown' : 'facts',
          collected_at: server.facts_state === 'stale' ? '' : '2026-05-28T12:00:00Z',
          disk_status: 'ok',
          apt_status: 'ok',
          os_pretty_name: 'Ubuntu',
          uptime_seconds: 3600,
        },
        risk: {
          level: riskLevel,
          summary: cves.length > 0 ? `${cves.length} CVE` : (securityUpdates > 0 ? `${securityUpdates} security` : 'No CVE exposure'),
          pending_packages: pendingUpdates.length,
          security_updates: securityUpdates,
          cves,
        },
        timeline: makeTimeline(server.timeline_status || server.status),
        approval_triage: {
          eligible: server.status === 'pending_approval' || pendingUpdates.length > 0,
          pending_packages: pendingUpdates.length,
          security_updates: securityUpdates,
          cve_count: cves.length,
          risk_level: riskLevel,
          risk_label: cves.length > 0 ? `${cves.length} CVE` : (securityUpdates > 0 ? `${securityUpdates} security` : 'No CVE exposure'),
          risk_order: cves.length > 0 ? 4 : (securityUpdates > 0 ? 3 : 2),
          facts_state: server.facts_state || 'fresh',
          last_check_display: 'May 28, 2026 12:00',
          standard_packages: Number(plan.standard_package_count || pendingUpdates.length),
          kept_back_packages: Number(plan.kept_back_package_count || 0),
          standard_security_updates: standardSecurity,
          kept_back_security_updates: keptBackSecurity,
          can_approve_all: server.status === 'pending_approval',
          can_approve_security: server.status === 'pending_approval' && standardSecurity > 0,
          can_approve_kept_back_security: server.status === 'pending_approval' && keptBackSecurity > 0 && !!plan.kept_back_security_plan_available,
          can_cancel: server.status === 'pending_approval',
          can_refresh_facts: true,
          can_run_checks: !['updating', 'upgrading'].includes(server.status),
        },
        command_history: [],
      };
    });
    return {
      generated_at: '2026-05-28T12:00:00Z',
      fleet: {
        pending_approval: servers.filter(server => server.status === 'pending_approval').length,
        prechecks_running: servers.filter(server => ['updating', 'upgrading'].includes(server.status)).length,
        in_progress: servers.filter(server => ['updating', 'upgrading'].includes(server.status)).length,
        done: servers.filter(server => server.status === 'done').length,
        stale_facts: servers.filter(server => server.facts_state === 'stale').length,
        high_risk_cve: dashboardServers.filter(server => server.approval_triage.cve_count > 0).length,
        pending_packages: dashboardServers.reduce((sum, server) => sum + server.approval_triage.pending_packages, 0),
        security_updates: dashboardServers.reduce((sum, server) => sum + server.approval_triage.security_updates, 0),
      },
      servers: dashboardServers,
    };
  }

  function makeHealthTrends(servers) {
    const trendServers = servers.map((server, index) => {
      const pendingUpdates = Array.isArray(server.pending_updates) ? server.pending_updates : [];
      const securityUpdates = pendingUpdates.filter(update => update.security).length;
      const packageCount = pendingUpdates.length;
      const latestDiskFree = 8192 - (index * 1024);
      return {
        name: server.name,
        samples: 2,
        latest: {
          captured_at: '2026-05-28T12:00:00Z',
          captured_at_display: 'May 28, 2026 12:00',
          source: 'audit',
          package_count: packageCount,
          security_count: securityUpdates,
          last_update_status: server.status === 'error' ? 'failure' : 'success',
          disk_status: server.facts_state === 'stale' ? 'unknown' : 'ok',
          disk_free_kb: latestDiskFree,
          disk_total_kb: 16384,
          apt_status: 'ok',
          reboot_required: false,
          os_pretty_name: 'Ubuntu',
        },
        first: {
          captured_at: '2026-05-27T12:00:00Z',
          package_count: packageCount + 1,
          security_count: securityUpdates + 1,
          disk_status: 'ok',
          disk_free_kb: latestDiskFree + 512,
          disk_total_kb: 16384,
          apt_status: 'ok',
        },
        package_delta: -1,
        security_delta: -1,
        disk_free_delta_kb: -512,
        update_failures: server.status === 'error' ? 1 : 0,
        scan_failures: 0,
        apt_problem_samples: 0,
        disk_problem_samples: server.facts_state === 'stale' ? 1 : 0,
        reboot_seen: false,
        points: [],
      };
    });
    return {
      window: '7d',
      from: '2026-05-21T12:00:00Z',
      from_display: 'May 21, 2026 12:00',
      to: '2026-05-28T12:00:00Z',
      to_display: 'May 28, 2026 12:00',
      generated_at: '2026-05-28T12:00:00Z',
      retention_days: 90,
      fleet: {
        servers_with_samples: trendServers.length,
        samples: trendServers.reduce((sum, server) => sum + server.samples, 0),
        update_failures: trendServers.reduce((sum, server) => sum + server.update_failures, 0),
        scan_failures: 0,
        apt_problem_samples: 0,
        disk_problem_samples: trendServers.reduce((sum, server) => sum + server.disk_problem_samples, 0),
        reboot_seen: 0,
      },
      servers: trendServers,
    };
  }

  async function stubDashboardApi(page, getServers) {
    await page.route('**/api/servers', route => fulfillJson(route, getServers()));
    await page.route('**/api/keys/global', route => fulfillJson(route, { has_key: false }));
    await page.route('**/api/audit-events*', route => fulfillJson(route, { items: [] }));
    await page.route('**/api/observability/summary*', route => fulfillJson(route, { totals: { updates_total: 0, success_rate_pct: 0 } }));
    await page.route('**/api/observability/health-trends*', route => fulfillJson(route, makeHealthTrends(getServers())));
    await page.route('**/api/update-policies', route => fulfillJson(route, []));
    await page.route('**/api/dashboard/summary*', route => fulfillJson(route, makeDashboardSummary(getServers())));
  }

  function makeServer(name, status = 'idle', pendingUpdates = [], overrides = {}) {
    return {
      name,
      host: `${name}.example.test`,
      port: 22,
      user: 'root',
      status,
      tags: [],
      pending_updates: pendingUpdates,
      pending_package_count: pendingUpdates.length,
      security_update_count: pendingUpdates.filter(update => update.security).length,
      logs: 'ready',
      ...overrides,
    };
  }

  function makePendingUpdates(count) {
    return Array.from({ length: count }, (_, index) => ({
      package: `pkg-${String(index + 1).padStart(2, '0')}`,
      current_version: '1.0.0',
      candidate_version: '1.0.1',
      source: 'ubuntu',
      security: index % 3 === 0,
      cve_state: index % 2 === 0 ? 'pending' : 'ready',
      cves: index % 5 === 0 ? [`CVE-2026-${String(index + 1).padStart(4, '0')}`] : [],
    }));
  }

  async function stubAdminApi(page, state = {}) {
    state.sessions = state.sessions || [
      {
        id: 'current-session',
        current: true,
        created_at: '2026-05-17T12:00:00Z',
        last_seen_at: '2026-05-17T12:05:00Z',
        expires_at: '2026-06-16T12:05:00Z',
        client_ip: '192.168.1.x',
        client_label: 'Chrome · Windows',
      },
      {
        id: 'other-session',
        current: false,
        created_at: '2026-05-16T09:00:00Z',
        last_seen_at: '2026-05-17T11:00:00Z',
        expires_at: '2026-06-16T11:00:00Z',
        client_ip: '203.0.113.x',
        client_label: 'Firefox · Linux',
      },
    ];
    await page.route('**/api/app-settings/timezone', async route => {
      if (route.request().method() === 'PUT') {
        state.timezoneSave = await route.request().postDataJSON();
      }
      const configuredTimezone = state.timezoneSave?.timezone ?? 'America/Toronto';
      const resolvedTimezone = configuredTimezone || 'America/Toronto';
      return fulfillJson(route, {
        timezone: resolvedTimezone,
        resolved_timezone: resolvedTimezone,
        editable_timezone: configuredTimezone,
      });
    });
    await page.route('**/api/auth/sessions/*', async route => {
      const id = decodeURIComponent(route.request().url().split('/').pop());
      state.sessionRevokeID = id;
      state.sessions = state.sessions.filter(session => session.id !== id);
      return fulfillJson(route, { current_session: false });
    });
    await page.route('**/api/auth/sessions/*/reveal-ip', async route => {
      state.sessionIPRevealPayload = await route.request().postDataJSON();
      if (state.sessionIPRevealDelayMs) {
        await new Promise(resolve => setTimeout(resolve, state.sessionIPRevealDelayMs));
      }
      if (state.sessionIPRevealFailure) {
        return route.fulfill({
          status: 401,
          contentType: 'application/json',
          body: JSON.stringify({ error: 'current password is invalid' }),
        });
      }
      return fulfillJson(route, {
        ip: '192.168.1.44',
        visible_for_seconds: state.sessionIPRevealSeconds || 30,
      });
    });
    await page.route('**/api/auth/sessions/others', async route => {
      state.sessionClearOthersCount = (state.sessionClearOthersCount || 0) + 1;
      const before = state.sessions.length;
      state.sessions = state.sessions.filter(session => session.current);
      return fulfillJson(route, { deleted_sessions: before - state.sessions.length });
    });
    await page.route('**/api/auth/sessions', async route => {
      if (route.request().method() === 'DELETE') {
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
    await page.route('**/api/auth/password', async route => {
      state.passwordPayload = await route.request().postDataJSON();
      state.passwordPayloads = [...(state.passwordPayloads || []), state.passwordPayload];
      const preservedBefore = state.sessions.length;
      if (state.passwordPartialFailure) {
        return route.fulfill({
          status: 207,
          contentType: 'application/json',
          body: JSON.stringify({
            message: 'password changed, but other sessions could not be invalidated',
            outcome: 'partial_failure',
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
        state.sessions = state.sessions.filter(session => session.current);
        invalidated = before - state.sessions.length;
      }
      return fulfillJson(route, {
        message: 'password changed',
        outcome: 'succeeded',
        password_changed: true,
        invalidation_requested: Boolean(state.passwordPayload.invalidate_other_sessions),
        invalidated_sessions: invalidated,
        preserved_sessions: state.sessions.length,
        current_session_preserved: true,
      });
    });
    await page.route('**/api/notifications/settings', async route => {
      if (route.request().method() === 'PUT') {
        state.notificationPayload = await route.request().postDataJSON();
        if (state.notificationPayload.webhook_url_intent === 'replace') {
          state.notificationConfiguredURL = state.notificationPayload.webhook_url;
        }
        if (state.notificationPayload.webhook_url_intent === 'clear') {
          state.notificationConfiguredURL = '';
        }
        return fulfillJson(route, {
          enabled: state.notificationPayload.enabled,
          webhook_configured: Boolean(state.notificationConfiguredURL),
          webhook_url_masked: state.notificationConfiguredURL ? 'https://hooks.example.test/••••' : '',
          webhook_url_intent: state.notificationPayload.webhook_url_intent,
          event_types: state.notificationPayload.event_types,
          supported_events: ['update.complete', 'schedule.run.failed', 'schedule.run.skipped', 'backup.restore'],
        });
      }
      state.notificationLoadCount = (state.notificationLoadCount || 0) + 1;
      if ((state.notificationFailuresRemaining || 0) > 0) {
        state.notificationFailuresRemaining -= 1;
        return route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: 'notifications unavailable' }) });
      }
      return fulfillJson(route, {
        enabled: false,
        webhook_configured: Boolean(state.notificationConfiguredURL),
        webhook_url_masked: state.notificationConfiguredURL ? 'https://hooks.example.test/••••' : '',
        webhook_url_intent: 'preserve',
        event_types: ['update.complete', 'schedule.run.failed', 'schedule.run.skipped', 'backup.restore'],
        supported_events: ['update.complete', 'schedule.run.failed', 'schedule.run.skipped', 'backup.restore'],
      });
    });
    await page.route('**/api/notifications/delivery-diagnostics', async route => {
      state.notificationDiagnosticsLoadCount = (state.notificationDiagnosticsLoadCount || 0) + 1;
      if ((state.notificationDiagnosticsFailuresRemaining || 0) > 0) {
        state.notificationDiagnosticsFailuresRemaining -= 1;
        return route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: 'delivery diagnostics unavailable' }) });
      }
      return fulfillJson(route, {
        last_attempt: state.notificationDiagnosticsResponse || null,
      });
    });
    await page.route('**/api/notifications/test', async route => {
      state.notificationTestCount = (state.notificationTestCount || 0) + 1;
      const lastAttempt = state.notificationTestResponse || {
          event_type: 'notification.test',
          action: 'notification.test',
          target_name: 'webhook',
          outcome: 'succeeded',
          success: true,
          attempts: 1,
          status_code: 202,
          attempted_at: '2026-05-17T12:00:00Z',
          completed_at: '2026-05-17T12:00:00Z',
          delivered_at: '2026-05-17T12:00:00Z',
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
          contentType: 'application/json',
          body: JSON.stringify({ error: 'notification test failed', ...body }),
        });
      }
      return fulfillJson(route, body);
    });
    await page.route('**/api/metrics/token', route => {
      const method = route.request().method();
      if (method === 'GET') {
        state.metricsLoadCount = (state.metricsLoadCount || 0) + 1;
        if ((state.metricsFailuresRemaining || 0) > 0) {
          state.metricsFailuresRemaining -= 1;
          return route.fulfill({
            status: 503,
            contentType: 'application/json',
            body: JSON.stringify({ error: 'metrics credential unavailable' }),
          });
        }
        return fulfillJson(route, state.metricsResponse || {
          enabled: true,
          lifecycle_state: 'current',
          created_at: '2026-05-01T12:00:00Z',
          rotated_at: '2026-05-15T12:00:00Z',
          last_used_at: '2026-07-27T12:00:00Z',
          last_used_origin_masked: '192.0.2.x',
          stale_after_days: 30,
        });
      }
      if (method === 'POST') {
        state.metricsRotateCount = (state.metricsRotateCount || 0) + 1;
        const response = state.metricsRotateResponse || {
          enabled: true,
          lifecycle_state: 'never_used',
          created_at: '2026-05-01T12:00:00Z',
          rotated_at: '2026-07-28T12:00:00Z',
          last_used_at: '',
          last_used_origin_masked: '',
          stale_after_days: 30,
          token: 'one-time-metrics-token',
        };
        state.metricsResponse = { ...response };
        delete state.metricsResponse.token;
        return fulfillJson(route, response);
      }
      if (method === 'DELETE') {
        state.metricsDisableCount = (state.metricsDisableCount || 0) + 1;
        state.metricsResponse = {
          enabled: false,
          lifecycle_state: 'disabled',
          created_at: '',
          rotated_at: '',
          last_used_at: '',
          last_used_origin_masked: '',
          stale_after_days: 30,
        };
        return fulfillJson(route, state.metricsResponse);
      }
      return route.fulfill({ status: 405 });
    });
    await page.route('**/api/audit-events?*', route => {
      state.adminActivityLoadCount = (state.adminActivityLoadCount || 0) + 1;
      state.adminActivityURLs = [...(state.adminActivityURLs || []), route.request().url()];
      if ((state.adminActivityFailuresRemaining || 0) > 0) {
        state.adminActivityFailuresRemaining -= 1;
        return route.fulfill({
          status: 503,
          contentType: 'application/json',
          body: JSON.stringify({ error: 'recent activity unavailable' }),
        });
      }
      return fulfillJson(route, state.adminActivityResponse || {
        items: [
          {
            id: 82,
            created_at: '2026-05-17T12:05:00Z',
            created_at_display: '2026-05-17 08:05 EDT · America/Toronto',
            actor: 'admin',
            action: 'auth.session.ip_reveal',
            status: 'success',
            message: 'DO-NOT-RENDER-MESSAGE',
            meta_json: '{"password":"DO-NOT-RENDER-META"}',
            request_id: 'DO-NOT-RENDER-REQUEST',
            client_ip: '203.0.113.44',
            target_name: 'DO-NOT-RENDER-TARGET',
          },
          {
            id: 81,
            created_at: '2026-05-17T12:00:00Z',
            created_at_display: '2026-05-17 08:00 EDT · America/Toronto',
            actor: 'scheduler',
            action: 'update_policy.update',
            status: 'failure',
          },
        ],
        total: 2,
        page: 1,
        page_size: 8,
      });
    });
    await page.route('**/api/backup/status', route => {
      state.backupLoadCount = (state.backupLoadCount || 0) + 1;
      if ((state.backupFailuresRemaining || 0) > 0) {
        state.backupFailuresRemaining -= 1;
        return route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: 'backup unavailable' }) });
      }
      return fulfillJson(route, state.backupStatusResponse || {
        db_path: '/tmp/simplelinuxupdater.db',
        backup_supported: true,
        known_hosts_path: '/tmp/known_hosts',
        recovery_health: backupRecoveryHealthFixture('healthy'),
      });
    });
    await page.route('**/api/backup/restore', async route => {
      state.restoreCount = (state.restoreCount || 0) + 1;
      if (state.deferRestore) {
        await new Promise(resolve => { state.releaseRestore = resolve; });
      }
      state.restoreCompleted = true;
      return fulfillJson(route, { restored: true, sessions_invalidated: false });
    });
    await page.route('**/api/backup/verify', async route => {
      state.verifyCount = (state.verifyCount || 0) + 1;
      return fulfillJson(route, state.verifyResponse || {
        valid: true,
        compatible: true,
        restore_ready: true,
        archive: {
          format: 'simplelinuxupdater-backup',
          version: 1,
          created_at: '2026-05-17T06:00:00Z',
          size_bytes: 4096,
        },
        manifest_files: 3,
        resources: [
          { name: 'servers.db', size_bytes: 3072, required: true, included: true },
          { name: 'config.json', size_bytes: 256, required: true, included: true },
          { name: 'known_hosts', size_bytes: 0, required: false, included: false },
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
          { code: 'sessions_invalidated', message: 'All active Admin sessions will be invalidated after replacement.' },
          { code: 'metrics_access_replaced', message: 'The current Metrics API credential will be replaced by the archived state.' },
          { code: 'known_hosts_not_included', message: 'known_hosts is not included; the current host-key trust file will remain unchanged.' },
        ],
        known_hosts_included: false,
        created_at: '2026-05-17T06:00:00Z',
      });
    });
    await page.route('**/api/update-policies/settings', route => fulfillJson(route, {
      timezone: 'America/Toronto',
      resolved_timezone: 'America/Toronto',
      global_blackouts: [],
    }));
    await page.route('**/api/update-policies/runs?*', route => {
      state.scheduledRunsLoadCount = (state.scheduledRunsLoadCount || 0) + 1;
      const requestURL = new URL(route.request().url());
      const query = Object.fromEntries(requestURL.searchParams.entries());
      state.scheduledRunsQueries = [...(state.scheduledRunsQueries || []), query];
      const filtered = query.policy || query.server || query.outcome || query.from || query.to;
      const pageNumber = Number(query.page || 1);
      const item = filtered ? {
        id: 8,
        policy_name: 'Nightly security',
        server_name: 'srv-web-02',
        status: 'skipped',
        terminal_outcome: 'skipped',
        reason: 'busy',
        exact_skip_reason: 'busy',
        summary: 'Server already has an active operation',
        scheduled_for_utc: '2026-05-18T06:00:00Z',
        started_at: '2026-05-18T06:00:00Z',
        finished_at: '2026-05-18T06:00:30Z',
        duration_ms: 30000,
        audit_url: '/manage?audit_action=schedule.run.skipped&audit_target=srv-web-02#audit-trail',
      } : pageNumber === 2 ? {
        id: 6,
        policy_name: 'Older policy',
        server_name: 'srv-web-09',
        status: 'failed',
        terminal_outcome: 'failed',
        summary: 'Upgrade failed',
        scheduled_for_utc: '2026-05-16T06:00:00Z',
        started_at: '2026-05-16T06:00:00Z',
        finished_at: '2026-05-16T06:02:00Z',
        duration_ms: 120000,
        audit_url: '/manage?audit_action=schedule.run.failed&audit_target=srv-web-09#audit-trail',
      } : {
        id: 7,
        policy_name: 'Nightly security',
        server_name: 'srv-web-01',
        status: 'succeeded',
        terminal_outcome: 'succeeded',
        summary: 'completed',
        job_id: 'job-report-1',
        scheduled_for_utc: '2026-05-17T06:00:00Z',
        started_at: '2026-05-17T06:00:00Z',
        finished_at: '2026-05-17T06:01:30Z',
        duration_ms: 90000,
        job_detail_url: '/api/jobs/job-report-1',
        report_url: '/api/reports/jobs/job-report-1',
        audit_url: '/manage?audit_action=schedule.run.completed&audit_target=srv-web-01#audit-trail',
      };
      return fulfillJson(route, {
        timezone: 'America/Toronto',
        items: [item],
        page: pageNumber,
        page_size: Number(query.page_size || 25),
        total: filtered ? 1 : 26,
        total_pages: filtered ? 1 : 2,
      });
    });
    await page.route('**/api/update-policies/calendar?*', route => {
      state.calendarLoadCount = (state.calendarLoadCount || 0) + 1;
      return fulfillJson(route, {
      days: 14,
      start_date: '2026-05-17',
      end_date: '2026-05-30',
      generated_at: '2026-05-17T12:00:00Z',
      timezone: 'America/Toronto',
      resolved_timezone: 'America/Toronto',
      policies: [{
        id: 12,
        name: 'Nightly security',
        enabled: true,
        cadence_kind: 'daily',
        time_local: '02:00',
        weekdays: [],
        matched_servers: ['srv-web-01'],
        days: [{
          date: '2026-05-17',
          weekday: 'sun',
          timezone_offset: '-04:00',
          allowed_slots: [{
            time_local: '02:00',
            scheduled_for_utc: '2026-05-17T06:00:00Z',
            timezone_offset: '-04:00',
            execution_mode: 'approval_required',
            package_scope: 'security',
            upgrade_mode: 'standard',
            matched_servers: ['srv-web-01'],
          }],
          blocked_windows: [{
            source: 'global',
            weekdays: ['sat'],
            start_time: '23:00',
            end_time: '03:00',
            overnight: true,
            applies_to_slot: false,
          }],
        }],
      }],
      });
    });
    await page.route('**/api/jobs/job-report-1', route => fulfillJson(route, {
      report_url: '/api/reports/jobs/job-report-1',
      job: {
        id: 'job-report-1',
        kind: 'update',
        parent_job_id: '',
        server_name: 'srv-web-01',
        actor: 'admin',
        client_ip: '127.0.0.1',
        status: 'succeeded',
        phase: 'complete',
        summary: 'completed',
        logs_text: 'apt update\nupgrade completed',
        error_class: '',
        retry_policy_json: '{"max_attempts":3,"backoff_seconds":30}',
        meta_json: '{"packages":2}',
        created_at: '2026-05-17T06:00:00Z',
        updated_at: '2026-05-17T06:05:00Z',
        started_at: '2026-05-17T06:00:05Z',
        finished_at: '2026-05-17T06:05:00Z',
      },
    }));
    await page.route('**/api/update-policies/preview', async route => {
      state.policyPreviewPayload = await route.request().postDataJSON();
      state.policyPreviewCount = (state.policyPreviewCount || 0) + 1;
      const timezone = state.timezoneSave?.timezone || 'America/Toronto';
      const fixedOffset = timezone === '+05:30';
      const defaultPreview = {
        matched_servers: [
          { name: 'srv-web-01', tags: ['prod', 'web'] },
          { name: 'srv-web-02', tags: ['prod'] },
        ],
        excluded_servers: [
          { name: 'srv-db-01', tags: ['prod', 'db'], reason: 'excluded_tag' },
        ],
        disabled_by_override: [],
        warnings: ['Explicit server "srv-missing" is not in the current inventory.'],
        validation_errors: [],
        operational_warnings: [
          { code: 'missing_explicit_server', message: 'Explicit server "srv-missing" is not in the current inventory.' },
        ],
        informational_facts: [
          { code: 'application_timezone', message: `Occurrences use the canonical application timezone ${timezone}.` },
        ],
        upcoming_occurrences: [
          {
            local_civil_time: fixedOffset ? '2026-05-18 03:45' : '2026-05-17 03:45',
            timezone,
            offset: fixedOffset ? '+05:30' : '-04:00',
            abbreviation: fixedOffset ? '+05:30' : 'EDT',
            scheduled_for_utc: fixedOffset ? '2026-05-17T22:15:00.000000000Z' : '2026-05-17T07:45:00.000000000Z',
            dst_status: 'standard',
            canonical_choice: 'exact',
            matched_server_count: 2,
            applicable_no_run_windows: [],
            admission_outcome: 'admitted',
          },
        ],
      };
      return fulfillJson(route, state.policyPreviewResponse || defaultPreview);
    });
    await page.route('**/api/update-policies', async route => {
      if (route.request().method() === 'POST') {
        state.policyPayload = await route.request().postDataJSON();
        return fulfillJson(route, { id: 42, ...state.policyPayload, matched_servers: ['srv-web-01'] });
      }
      return fulfillJson(route, {
        timezone: 'America/Toronto',
        items: state.policies || [{
          id: 12,
          name: 'Nightly security',
          enabled: true,
          target_tag: 'prod',
          include_tags: ['web'],
          exclude_tags: ['hold'],
          target_servers: ['srv-web-01'],
          package_scope: 'security',
          execution_mode: 'approval_required',
          cadence_kind: 'daily',
          time_local: '02:00',
          weekdays: [],
          matched_servers: ['srv-web-01'],
        }],
      });
    });
    await page.route('**/api/update-policies/*', async route => {
      if (route.request().method() === 'DELETE') {
        state.policyDeleteCount = (state.policyDeleteCount || 0) + 1;
        return fulfillJson(route, { ok: true });
      }
      return route.fallback();
    });
  }

  async function stubManageApi(page, state = {}) {
    await page.route('**/api/servers', route => {
      state.inventoryLoadCount = (state.inventoryLoadCount || 0) + 1;
      return fulfillJson(route, state.servers || [makeServer('demo-host')]);
    });
    await page.route('**/api/servers/*/key', async route => {
      if (route.request().method() === 'POST') {
        state.uploadServerKeyCount = (state.uploadServerKeyCount || 0) + 1;
        if (state.failServerKeyUpload) {
          return route.fulfill({
            status: 422,
            contentType: 'application/json',
            body: JSON.stringify({ error: 'replacement key rejected' }),
          });
        }
        return fulfillJson(route, { ok: true });
      }
      return route.fallback();
    });
    await page.route('**/api/servers/*', async route => {
      if (route.request().method() === 'PUT') {
        const update = route.request().postDataJSON();
        const existing = (state.servers || [makeServer('demo-host')])[0];
        state.serverUpdateCount = (state.serverUpdateCount || 0) + 1;
        state.servers = [{
          ...existing,
          ...update,
          has_password: !!update.pass || !!existing.has_password,
        }];
        return fulfillJson(route, state.servers[0]);
      }
      if (route.request().method() === 'DELETE') {
        state.deleteServerCount = (state.deleteServerCount || 0) + 1;
        state.deletedServerUrl = route.request().url();
        return fulfillJson(route, { ok: true });
      }
      return route.fallback();
    });
    await page.route('**/api/keys/global', async route => {
        if (route.request().method() === 'DELETE') {
        state.clearGlobalKeyCount = (state.clearGlobalKeyCount || 0) + 1;
        state.hasGlobalKey = false;
        return fulfillJson(route, { ok: true });
      }
      return fulfillJson(route, { has_key: state.hasGlobalKey ?? true, private_key: 'DO-NOT-RENDER-PRIVATE-KEY' });
    });
    await page.route('**/api/hostkeys/scan', route => {
      const hostKeyState = state.hostKeyState || 'trusted';
      if (hostKeyState === 'error') {
        return route.fulfill({
          status: 502,
          contentType: 'application/json',
          body: JSON.stringify({ error: 'remote host unavailable' }),
        });
      }
      return fulfillJson(route, {
        host: 'demo-host.example.test',
        port: 22,
        algorithm: 'ssh-ed25519',
        fingerprint_sha256: hostKeyState === 'changed' ? 'SHA256:new' : 'SHA256:trusted',
        already_trusted: hostKeyState === 'trusted',
        host_entry_exists: hostKeyState === 'trusted' || hostKeyState === 'changed',
      });
    });
    await page.route('**/api/hostkeys/trust', async route => {
      state.trustHostKeyCount = (state.trustHostKeyCount || 0) + 1;
      state.hostKeyState = 'trusted';
      return fulfillJson(route, {
        host: 'demo-host.example.test',
        port: 22,
        fingerprint_sha256: 'SHA256:trusted',
        already_trusted: false,
      });
    });
    await page.route('**/api/hostkeys/clear', async route => {
      state.clearKnownHostCount = (state.clearKnownHostCount || 0) + 1;
      state.hostKeyState = 'missing';
      return fulfillJson(route, { removed_entries: 1 });
    });
    await page.route('**/api/audit-events/prune', async route => {
      state.auditPruneCount = (state.auditPruneCount || 0) + 1;
      return fulfillJson(route, { deleted: 3 });
    });
    await page.route('**/api/audit-events*', route => {
      state.auditListUrls = [...(state.auditListUrls || []), route.request().url()];
      return fulfillJson(route, {
        items: [{
          id: 55,
          created_at: '2026-05-17T12:00:00Z',
          created_at_display: '2026-05-17 08:00:00 America/Toronto',
          actor: 'admin',
          action: 'server.delete',
          target_type: 'server',
          target_name: 'demo-host',
          status: 'success',
          message: 'Deleted server',
          meta_json: '{"scope":"security","count":2}',
          request_id: 'req-55',
          client_ip: '127.0.0.1',
        }],
        total: 1,
        page: 1,
        page_size: 20,
      });
    });
    await page.route('**/api/update-policies', route => fulfillJson(route, {
      items: state.policies || [{
        id: 9,
        name: 'Prod security',
        target_tag: 'prod',
        include_tags: ['web'],
        exclude_tags: ['hold'],
        matched_servers: ['demo-host'],
      }],
    }));
    await page.route('**/api/update-policies/*/overrides', route => fulfillJson(route, { items: [] }));
    await page.route('**/api/update-policies/*/overrides/*', async route => {
      state.policyOverrideSaveCount = (state.policyOverrideSaveCount || 0) + 1;
      if (state.failPolicyOverrideSave) {
        return route.fulfill({
          status: 503,
          contentType: 'application/json',
          body: JSON.stringify({ error: 'policy override unavailable' }),
        });
      }
      return fulfillJson(route, { ok: true });
    });
  }

  test('setup form shows mismatch error', async ({ page }) => {
    await page.goto('/setup');
    if (!/\/setup$/.test(page.url())) {
      test.skip(true, 'setup already completed');
    }
    await page.locator('#username').fill(username);
    await page.locator('#password').fill(password);
    await page.locator('#password-confirm').fill('DifferentPass1234');
    await page.getByRole('button', { name: 'Create account' }).click();
    await expect(page.locator('#error-banner')).toBeVisible();
    await expect(page.locator('#error-banner')).toContainText('Passwords do not match.');
    await expect(page).toHaveURL(/\/setup$/);
  });

  test('setup creates account and redirects to status page', async ({ page }) => {
    await page.goto('/setup');
    if (/\/login$/.test(page.url())) {
      await signIn(page);
      return;
    }
    if (page.url() === 'http://127.0.0.1:8080/') {
      await expect(page.locator('#logout-btn')).toBeVisible();
      return;
    }
    await page.locator('#username').fill(username);
    await page.locator('#password').fill(password);
    await page.locator('#password-confirm').fill(password);
    await page.getByRole('button', { name: 'Create account' }).click();
    await expect(page).toHaveURL('http://127.0.0.1:8080/');
    await expect(page.locator('#logout-btn')).toBeVisible();
    await rememberAuthCookies(page);
  });

  test('invalid login shows error, valid login succeeds', async ({ page }) => {
    await page.goto('/login');
    await expect(page).toHaveURL(/\/login$/);

    await page.locator('#username').fill(username);
    await page.locator('#password').fill('WrongPassword123');
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page.locator('#error-banner')).toBeVisible();
    await expect(page.locator('#error-banner')).toContainText(/invalid credentials|login failed/i);
    await expect(page).toHaveURL(/\/login$/);

    await page.locator('#password').fill(password);
    await signIn(page);
  });

  test('observability keeps a successful summary when health trends fail', async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await page.route('**/api/observability/summary*', route => fulfillJson(route, {
      totals: { updates_total: 4, success_rate_pct: 75 },
      duration: { avg_ms: 1250, samples_with_duration: 3, samples_without_duration: 1 },
      failure_causes: [],
      status_breakdown: [],
    }));
    await page.route('**/api/observability/health-trends*', route => route.fulfill({
      status: 503,
      contentType: 'application/json',
      body: JSON.stringify({ error: 'temporarily unavailable' }),
    }));

    await page.goto('/observability');
    await expect(page.locator('#kpi-total')).toHaveText('4');
    await expect(page.locator('#kpi-success-rate')).toContainText('75.00%');
    await expect(page.locator('#error-banner')).toContainText('Health trends is unavailable (HTTP 503)');
    await expect(page.locator('#summary-lifecycle')).toContainText('Current');
    await expect(page.locator('#trends-lifecycle')).toContainText('Unavailable');
    await expect(page.locator('#trends-lifecycle').getByRole('button', { name: 'Retry' })).toBeVisible();
    await expect(page.locator('#trend-hosts')).toHaveText('Unavailable');
    await expect(page.locator('#health-trends-body')).toContainText('Host health unavailable');
  });

  test('observability keeps successful health trends when the summary is unavailable', async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await page.route('**/api/observability/summary*', route => route.fulfill({
      status: 503,
      contentType: 'application/json',
      body: JSON.stringify({ error: 'temporarily unavailable' }),
    }));
    await page.route('**/api/observability/health-trends*', route => fulfillJson(route, {
      window: '7d',
      from: '2026-07-22T12:00:00Z',
      to: '2026-07-29T12:00:00Z',
      generated_at: '2026-07-29T12:00:00Z',
      retention_days: 90,
      fleet: { servers_with_samples: 1, samples: 1 },
      servers: [{
        name: 'healthy-host',
        samples: 1,
        latest: { captured_at: '2026-07-29T11:00:00Z', disk_free_kb: 1048576, apt_status: 'ok', disk_status: 'ok' },
        first: { captured_at: '2026-07-29T11:00:00Z', disk_free_kb: 1048576 },
        points: [],
      }],
    }));

    await page.goto('/observability');
    await expect(page.locator('#summary-lifecycle')).toContainText('Unavailable');
    await expect(page.locator('#trends-lifecycle')).toContainText('Current');
    await expect(page.locator('#kpi-success-rate')).toHaveText('Unavailable');
    await expect(page.locator('#kpi-duration')).toHaveText('Unavailable');
    await expect(page.locator('#health-trends-body')).toContainText('healthy-host');
  });

  test('observability projects source lifecycle, fleet severity, confidence, and missing health facts', async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await page.route('**/api/observability/summary*', route => fulfillJson(route, {
      window: '24h',
      from: '2026-07-28T12:00:37Z',
      to: '2026-07-29T12:00:45Z',
      totals: { updates_total: 4, updates_success: 3, updates_failure: 1, success_rate_pct: 75 },
      duration: { avg_ms: 1250, samples_with_duration: 1, samples_without_duration: 3 },
      failure_causes: [{ cause: 'unknown', count: 1, servers: ['demo-host'] }],
      status_breakdown: [{ status: 'success', count: 3 }, { status: 'failure', count: 1 }],
    }));
    await page.route('**/api/observability/health-trends*', route => fulfillJson(route, {
      window: '24h',
      from: '2026-07-28T12:00:00Z',
      to: '2026-07-29T12:00:00Z',
      generated_at: '2026-07-29T12:00:00Z',
      retention_days: 90,
      fleet: { servers_with_samples: 0, samples: 0, update_failures: 0, scan_failures: 0 },
      servers: [{
        name: 'demo-host',
        samples: 0,
        last_observation: {
          captured_at: '2026-07-26T11:00:00Z',
          disk_free_kb: 0,
          apt_status: 'ok',
          disk_status: 'unknown',
        },
        update_failures: 0,
        scan_failures: 0,
        points: [],
      }],
    }));

    await page.goto('/observability?window=24h');
    await expect(page.locator('#summary-lifecycle')).toContainText('Current');
    await expect(page.locator('#trends-lifecycle')).toContainText('Current');
    await expect(page.locator('#summary-lifecycle .source-lifecycle-time')).toContainText('2026');
    await expect(page.locator('#summary-lifecycle .source-lifecycle-time')).toContainText('ago');
    await expect(page.locator('#trends-lifecycle .source-lifecycle-time')).toContainText('2026');
    await expect(page.locator('#trends-lifecycle .source-lifecycle-time')).toContainText('ago');
    await expect(page.locator('#kpi-success-rate-card')).toContainText('Critical');
    await expect(page.locator('#kpi-success-icon')).toHaveText('×');
    await expect(page.locator('#kpi-success-rate-card')).toContainText('3 successful');
    await expect(page.locator('#kpi-success-rate-card')).toContainText('1 failed');
    await expect(page.locator('#kpi-duration-card')).toContainText('Low confidence');
    await expect(page.locator('#kpi-duration-card')).toContainText('1 of 4 runs');
    await expect(page.locator('#failure-causes-body')).toContainText('Data quality issue');
    await expect(page.locator('#failure-causes-body')).toContainText('Affected: demo-host');
    await expect(page.locator('#failure-causes-body a[href*="audit_target=demo-host"]')).toHaveAttribute('href', /audit_status=failure/);
    await expect(page.locator('#health-trends-body')).toContainText('Unavailable');
    await expect(page.locator('#health-trends-body')).toContainText('Stale');
    await expect(page.locator('#health-trends-body')).not.toContainText('119231880');
    await expect(page.locator('#health-trends-body .health-signal-badge').nth(1)).toHaveClass(/status-unknown/);
    const failureCauseLink = page.locator('#failure-causes-body a[href*="failure_cause=unknown"]').first();
    await expect(failureCauseLink).toHaveAttribute('href', /audit_from=2026-07-28T12%3A00%3A37Z/);
    await expect(failureCauseLink).toHaveAttribute('href', /audit_to=2026-07-29T12%3A00%3A45Z/);
    await expect(failureCauseLink).toHaveAttribute('href', /#audit-trail$/);
    const causeRequest = page.waitForRequest(request => request.url().includes('/api/audit-events?')
      && new URL(request.url()).searchParams.get('failure_cause') === 'unknown'
      && new URL(request.url()).searchParams.get('from') === '2026-07-28T12:00:37Z'
      && new URL(request.url()).searchParams.get('to') === '2026-07-29T12:00:45Z');
    await page.locator('#failure-causes-body a[href*="audit_target=demo-host"]').click();
    await causeRequest;
    await expect(page).toHaveURL(/\/manage\?/);
    await expect(page).toHaveURL(/failure_cause=unknown/);
    await expect(page.locator('#audit-target-filter')).toHaveValue('demo-host');
    await expect(page.locator('#audit-action-filter')).toHaveValue('update.complete');
    await expect(page.locator('#audit-status-filter')).toHaveValue('failure');
    await expect(page.locator('#audit-failure-cause-active')).toContainText('unknown');
    const clearedCauseRequest = page.waitForRequest(request => request.url().includes('/api/audit-events?')
      && !new URL(request.url()).searchParams.has('failure_cause'));
    await page.locator('#audit-clear-failure-cause').click();
    await clearedCauseRequest;
    await expect(page.locator('#audit-failure-cause-active')).toBeHidden();
    await expect(page).not.toHaveURL(/failure_cause=/);
  });

  test('observability leaves unavailable disk intervals unconnected', async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await page.route('**/api/observability/summary*', route => fulfillJson(route, {
      window: '24h',
      from: '2026-07-28T12:00:00Z',
      to: '2026-07-29T12:00:00Z',
      totals: { updates_total: 0, updates_success: 0, updates_failure: 0, success_rate_pct: 0 },
      duration: { avg_ms: 0, samples_with_duration: 0, samples_without_duration: 0 },
      failure_causes: [],
      status_breakdown: [],
    }));
    const points = [
      { captured_at: '2026-07-29T08:00:00Z', disk_free_kb: 1024 },
      { captured_at: '2026-07-29T09:00:00Z', disk_free_kb: 896 },
      { captured_at: '2026-07-29T10:00:00Z', disk_free_kb: 0 },
      { captured_at: '2026-07-29T11:00:00Z', disk_free_kb: 768 },
      { captured_at: '2026-07-29T12:00:00Z', disk_free_kb: 640 },
    ];
    await page.route('**/api/observability/health-trends*', route => fulfillJson(route, {
      window: '24h',
      from: '2026-07-28T12:00:00Z',
      to: '2026-07-29T12:00:00Z',
      generated_at: '2026-07-29T12:00:00Z',
      retention_days: 90,
      fleet: { servers_with_samples: 1, samples: points.length },
      servers: [{
        name: 'disk-gap-host',
        samples: points.length,
        first: points[0],
        latest: points.at(-1),
        points,
      }],
    }));

    await page.goto('/observability?window=24h');
    await expect(page.locator('#kpi-duration')).toHaveText('No data');
    await expect(page.locator('#disk-trend-chart .trend-line')).toHaveCount(2);
    await expect(page.locator('#disk-trend-chart .trend-point')).toHaveCount(4);
  });

  test('observability CSV exports neutralize spreadsheet formulas', async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await page.route('**/api/observability/summary*', route => fulfillJson(route, {
      window: '24h',
      from: '2026-07-28T12:00:00Z',
      to: '2026-07-29T12:00:00Z',
      totals: { updates_total: 0, updates_success: 0, updates_failure: 0, success_rate_pct: 0 },
      duration: { avg_ms: 0, samples_with_duration: 0, samples_without_duration: 0 },
      failure_causes: [],
      status_breakdown: [],
    }));
    await page.route('**/api/observability/health-trends*', route => fulfillJson(route, {
      window: '24h',
      from: '2026-07-28T12:00:00Z',
      to: '2026-07-29T12:00:00Z',
      generated_at: '2026-07-29T12:00:00Z',
      retention_days: 90,
      fleet: { servers_with_samples: 2, samples: 2 },
      servers: [
        {
          name: '=HYPERLINK("https://example.invalid")',
          samples: 1,
          first: { captured_at: '2026-07-29T11:00:00Z', disk_free_kb: 1024 },
          latest: { captured_at: '2026-07-29T11:00:00Z', disk_free_kb: 1024 },
          points: [{ captured_at: '2026-07-29T11:00:00Z', disk_free_kb: 1024 }],
        },
        {
          name: 'safe\r=2+2',
          samples: 1,
          first: { captured_at: '2026-07-29T11:00:00Z', disk_free_kb: 1024 },
          latest: { captured_at: '2026-07-29T11:00:00Z', disk_free_kb: 1024 },
          points: [{ captured_at: '2026-07-29T11:00:00Z', disk_free_kb: 1024 }],
        },
      ],
    }));

    await page.goto('/observability?window=24h');
    const downloadPromise = page.waitForEvent('download');
    await page.locator('#export-health-csv').click();
    const download = await downloadPromise;
    const csv = fs.readFileSync(await download.path(), 'utf8');
    expect(csv).toContain(`"'=HYPERLINK(""https://example.invalid"")"`);
    expect(csv).not.toContain(`"=HYPERLINK(""https://example.invalid"")"`);
    expect(csv).toContain('"safe\r=2+2"');
  });

  test('observability renders an unchanged disk delta without an increase label', async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await page.route('**/api/observability/summary*', route => fulfillJson(route, {
      window: '24h',
      from: '2026-07-28T12:00:00Z',
      to: '2026-07-29T12:00:00Z',
      totals: { updates_total: 0, updates_success: 0, updates_failure: 0, success_rate_pct: 0 },
      duration: { avg_ms: 0, samples_with_duration: 0, samples_without_duration: 0 },
      failure_causes: [],
      status_breakdown: [],
    }));
    await page.route('**/api/observability/health-trends*', route => fulfillJson(route, {
      window: '24h',
      from: '2026-07-28T12:00:00Z',
      to: '2026-07-29T12:00:00Z',
      generated_at: '2026-07-29T12:00:00Z',
      retention_days: 90,
      fleet: { servers_with_samples: 1, samples: 2 },
      servers: [{
        name: 'steady-disk',
        samples: 2,
        first: { captured_at: '2026-07-28T11:00:00Z', disk_free_kb: 1024 },
        latest: { captured_at: '2026-07-29T11:00:00Z', disk_free_kb: 1024 },
        disk_free_delta_kb: 0,
        points: [],
      }],
    }));

    await page.goto('/observability?window=24h');
    const diskCell = page.locator('#health-trends-body tr').filter({ hasText: 'steady-disk' }).locator('td').nth(4);
    await expect(diskCell).toHaveText('1 MB (unchanged)');
    await expect(diskCell).not.toContainText('increase');
  });

  test('observability charts, filters, shareable URL, pagination, and CSV scale an investigation', async ({ page }) => {
    await page.route('**/api/app-settings/timezone', route => fulfillJson(route, {
      timezone: 'America/Toronto',
      resolved_timezone: 'America/Toronto',
      editable_timezone: 'America/Toronto',
    }));
    await ensureAuthenticatedSession(page);
    await page.route('**/api/observability/summary*', route => fulfillJson(route, {
      window: '7d',
      from: '2026-07-22T12:00:00Z',
      to: '2026-07-29T12:00:00Z',
      totals: { updates_total: 10, updates_success: 9, updates_failure: 1, success_rate_pct: 90 },
      duration: { avg_ms: 800, samples_with_duration: 10, samples_without_duration: 0 },
      failure_causes: [{ cause: 'retry_exhausted', count: 1, servers: ['prod-failing'] }],
      status_breakdown: [{ status: 'success', count: 9 }, { status: 'failure', count: 1 }],
    }));
    const servers = Array.from({ length: 28 }, (_, index) => ({
      name: index === 0 ? 'prod-failing' : `host-${String(index).padStart(2, '0')}`,
      samples: 2,
      latest: {
        captured_at: '2026-07-29T11:00:00Z',
        package_count: index,
        security_count: index % 3,
        disk_free_kb: 10485760 - index * 1024,
        apt_status: 'ok',
        disk_status: 'ok',
      },
      first: {
        captured_at: '2026-07-28T11:00:00Z',
        package_count: Math.max(0, index - 1),
        security_count: 0,
        disk_free_kb: 11534336,
      },
      package_delta: 2,
      security_delta: index % 3,
      disk_free_delta_kb: -1048576,
      update_failures: index === 0 ? 2 : 0,
      scan_failures: 0,
      apt_problem_samples: 0,
      disk_problem_samples: 0,
      reboot_seen: false,
      points: [
        { captured_at: '2026-07-28T11:00:00Z', package_count: 1, security_count: 0, disk_free_kb: 11534336, last_update_status: 'success' },
        { captured_at: '2026-07-29T11:00:00Z', package_count: 2, security_count: 1, disk_free_kb: 10485760, last_update_status: index === 0 ? 'failure' : 'success' },
      ],
    }));
    let healthTrendRequestCount = 0;
    await page.route('**/api/observability/health-trends*', route => {
      healthTrendRequestCount += 1;
      const responseServers = healthTrendRequestCount === 2
        ? servers.map(server => ({ ...server, points: server.points.slice(1) }))
        : servers;
      return fulfillJson(route, {
        window: '7d',
        from: '2026-07-22T12:00:00Z',
        to: '2026-07-29T12:00:00Z',
        generated_at: '2026-07-29T12:00:00Z',
        retention_days: 90,
        fleet: { servers_with_samples: 28, samples: 56, update_failures: 2, scan_failures: 0 },
        servers: responseServers,
      });
    });

    await page.goto('/observability?window=7d');
    await expect(page.locator('#observability-last-refresh')).toContainText('2026');
    await expect(page.locator('#observability-last-refresh')).toContainText('ago');
    await expect(page.getByRole('group', { name: /Package count trend/ })).toBeVisible();
    await expect(page.getByRole('group', { name: /Disk free trend/ })).toBeVisible();
    await expect(page.locator('#package-trend-chart .trend-axis-unit')).toHaveText('packages');
    await expect(page.locator('#disk-trend-chart .trend-axis-unit')).toHaveText('disk free');
    await expect(page.locator('#package-trend-chart .trend-y-axis-label')).toHaveCount(3);
    await expect(page.locator('#package-trend-chart .trend-x-axis-label')).toHaveCount(3);
    await expect(page.locator('#package-trend-chart .trend-x-axis-label')).toHaveText(['Jul 28', 'Jul 28', 'Jul 29']);
    await expect(page.locator('#package-trend-chart .trend-point')).toHaveCount(2);
    await expect(page.getByRole('group', { name: /Package count trend/ })).toHaveAttribute('aria-label', /2 time points/);
    const packageLinePoints = await page.locator('#package-trend-chart .trend-line').getAttribute('points');
    expect(packageLinePoints.trim().split(/\s+/)).toHaveLength(3);
    await expect(page.locator('.trend-chart-scope')).toHaveText(['Fleet total', 'Fleet total', 'Fleet total', 'Fleet total']);
    const packagePoint = page.locator('#package-trend-chart .trend-point').first();
    await expect(packagePoint).toHaveAttribute('aria-label', /exact observation Jul 28, 2026, 07:00 EDT/);
    await packagePoint.hover();
    await expect(page.locator('#package-trend-chart .trend-tooltip')).toBeVisible();
    await expect(page.locator('#package-trend-chart .trend-tooltip')).not.toHaveAttribute('style', /.+/);
    await expect(page.locator('#package-trend-chart .trend-tooltip')).toContainText('Fleet total');
    await expect(page.locator('#package-trend-chart .trend-tooltip')).toContainText('28 packages');
    await expect(page.locator('#package-trend-chart .trend-tooltip')).toContainText('Jul 28, 2026, 07:00 EDT');
    await expect(page.locator('#package-trend-chart .trend-tooltip')).toContainText('Daily bucket');
    await expect(page.locator('#package-trend-chart .trend-tooltip')).toContainText('28 hosts represented');
    await expect(page.locator('#package-trend-chart .trend-tooltip')).toContainText('28 observations');
    const lastPackagePoint = page.locator('#package-trend-chart .trend-point').last();
    await lastPackagePoint.hover();
    const tooltipBox = await page.locator('#package-trend-chart .trend-tooltip').boundingBox();
    const chartBox = await page.locator('#package-trend-chart').boundingBox();
    expect(tooltipBox.x).toBeGreaterThanOrEqual(chartBox.x);
    expect(tooltipBox.x + tooltipBox.width).toBeLessThanOrEqual(chartBox.x + chartBox.width);
    await page.locator('#health-search').focus();
    await expect(page.locator('#package-trend-chart .trend-tooltip')).toBeHidden();
    await packagePoint.focus();
    await expect(page.locator('#package-trend-chart .trend-tooltip')).toBeVisible();
    const focusedPointKey = await packagePoint.getAttribute('data-observability-focus-key');
    expect(focusedPointKey).toBeTruthy();
    await page.evaluate(() => {
      executeEffects(observabilityInteraction.dispatch({ type: 'manualRefresh' }));
    });
    await expect(page.locator(`[data-observability-focus-key="${focusedPointKey}"]`)).toHaveCount(0);
    await expect(page.locator('#package-trend-chart .trend-point')).toBeFocused();
    await page.locator('#health-trend-server').selectOption('prod-failing');
    await expect(page.locator('.trend-chart-scope')).toHaveText(['prod-failing', 'prod-failing', 'prod-failing', 'prod-failing']);
    await page.locator('#health-trend-server').selectOption('');
    await expect(page.locator('.trend-chart-scope')).toHaveText(['Fleet total', 'Fleet total', 'Fleet total', 'Fleet total']);
    await expect(page.locator('#failure-breakdown-bars progress')).toHaveCount(1);
    await expect(page.locator('#failure-causes-body a[href*="audit_target=prod-failing"]')).toHaveAttribute('href', /audit_status=failure/);
    await expect(page.locator('#status-breakdown-bars progress')).toHaveCount(2);
    await expect(page.locator('#health-result-count')).toContainText('25 of 28');
    await expect(page.locator('#health-trends-body tr')).toHaveCount(25);
    const failingHostRow = page.locator('#health-trends-body tr').filter({ hasText: 'prod-failing' });
    await expect(failingHostRow).toContainText('increase of 2 packages');
    await expect(failingHostRow).toContainText('10.0 GB');
    await page.locator('#health-next-page').click();
    await expect(page).toHaveURL(/page=2/);
    await expect(page.locator('#health-result-count')).toContainText('3 of 28');

    await page.locator('#health-search').fill('prod');
    await page.locator('#health-attention-filter').selectOption('failures');
    await expect(page).toHaveURL(/search=prod/);
    await expect(page).toHaveURL(/attention=failures/);
    await expect(page.locator('#health-trends-body')).toContainText('prod-failing');
    await expect(page.locator('#health-trends-body tr')).toHaveCount(1);
    await expect(page.locator('#health-trends-body a')).toHaveAttribute('href', /\/manage\?server=prod-failing#server-directory$/);

    const downloadPromise = page.waitForEvent('download');
    await page.locator('#export-health-csv').click();
    const download = await downloadPromise;
    expect(download.suggestedFilename()).toMatch(/^observability-health-7d\.csv$/);
    const healthCSV = fs.readFileSync(await download.path(), 'utf8');
    const healthCSVLines = healthCSV.trim().split('\n');
    expect(healthCSVLines).toHaveLength(3);
    expect(healthCSVLines[0]).toContain('Captured at app time,Captured at UTC');
    expect(healthCSVLines[1]).toContain('prod-failing');
    expect(healthCSVLines[1]).toContain('2026-07-28T11:00:00Z');
    expect(healthCSVLines[2]).toContain('prod-failing');
    expect(healthCSVLines[2]).toContain('2026-07-29T11:00:00Z');
    const failuresDownloadPromise = page.waitForEvent('download');
    await page.locator('#export-failures-csv').click();
    const failuresDownload = await failuresDownloadPromise;
    expect(failuresDownload.suggestedFilename()).toBe('observability-failures-7d.csv');

    await page.setViewportSize({ width: 390, height: 844 });
    const mobileLayout = await page.evaluate(() => ({
      bodyWidth: document.body.scrollWidth,
      viewportWidth: document.documentElement.clientWidth,
    }));
    expect(mobileLayout.bodyWidth).toBeLessThanOrEqual(mobileLayout.viewportWidth);
    await expect(page.locator('#health-scroll-hint')).toBeVisible();

    await page.goto('/manage?server=prod-failing#server-directory');
    await expect(page.locator('#search')).toHaveValue('prod-failing');
    await expect(page).toHaveURL(/server=prod-failing#server-directory$/);
  });

  test('pending updates drawer keeps scroll position after server refresh', async ({ page }) => {
    let servers = [
      makeServer('demo-host', 'pending_approval', makePendingUpdates(80), { tags: ['prod'], has_key: true }),
      makeServer('runner-host', 'updating', [], {
        tags: ['web'],
        has_password: true,
        next_run: {
          state: 'scheduled',
          policy_name: 'Nightly security',
          scheduled_for_utc: '2026-05-29T06:00:00Z',
          scheduled_for_display: 'May 29, 2026 06:00',
          status: 'scheduled',
        },
      }),
    ];
    const state = {};
    await stubDashboardApi(page, () => servers);
    await page.route('**/api/approve/demo-host', route => {
      state.approveAll = (state.approveAll || 0) + 1;
      return fulfillJson(route, { ok: true });
    });
    await page.route('**/api/approve-security/demo-host', route => {
      state.approveSecurity = (state.approveSecurity || 0) + 1;
      return fulfillJson(route, { ok: true });
    });
    await page.route('**/api/cancel/demo-host', route => {
      state.cancel = (state.cancel || 0) + 1;
      return fulfillJson(route, { ok: true });
    });

    await ensureAuthenticatedSession(page);

    await expect(page.locator('.fleet-rail h2')).toHaveText('Fleet filters');
    await expect(page.locator('#maintenance-timeline-title')).toBeVisible();
    await expect(page.locator('#approval-triage-title')).toBeVisible();
    await expect(page.locator('#selected-host-title')).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Scheduled runs' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Running operations' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Audit trail' })).toBeVisible();
    await expect(page.locator('#servers-table tbody tr[data-name="demo-host"]')).toBeVisible();
    await expect(page.locator('#selected-host-title')).toHaveText('demo-host');
    await expect(page.locator('#selected-host-panel').getByRole('button', { name: 'Update' })).toHaveCount(0);
    await expect(page.locator('#approval-triage-table tbody')).toContainText('demo-host');
    await expect(page.locator('#scheduled-runs')).toContainText('Nightly security');

    await page.goto('/observability');
    await expect(page.getByRole('heading', { name: 'Trend summary' })).toBeVisible();
    await expect(page.locator('#health-trends-body')).toContainText('demo-host');
    await expect(page.locator('#health-trends-body')).toContainText('runner-host');
    await expect(page.locator('#trend-hosts')).toHaveText('2');
    await page.locator('#health-trend-server').selectOption('demo-host');
    await expect(page.locator('#health-trends-body')).toContainText('demo-host');

    await page.goto('/');
    await expect(page.locator('#servers-table tbody tr[data-name="demo-host"]')).toBeVisible();

    await page.locator('#select-all').check();
    await page.locator('#bulk-approve-security').click();
    await expect.poll(() => state.approveSecurity || 0).toBe(1);
    await expect(page.locator('#bulk-review-modal')).toHaveCount(0);
    await expect(page.locator('#typed-confirm-modal')).not.toBeVisible();

    await page.locator('#approval-triage-table button[data-action="approve-all"][data-name="demo-host"]').click();
    await page.locator('#approval-triage-table button[data-action="approve-security"][data-name="demo-host"]').click();
    await page.locator('#approval-triage-table button[data-action="cancel-upgrade"][data-name="demo-host"]').click();
    await expect.poll(() => state.approveAll || 0).toBe(1);
    await expect.poll(() => state.approveSecurity || 0).toBe(2);
    await expect.poll(() => state.cancel || 0).toBe(1);

    await page.setViewportSize({ width: 1697, height: 825 });
    await page.locator('#servers-table tbody button[data-action="open-drawer"][data-tab="pending"]').click();
    const pendingPanel = page.locator('#drawer-panel-pending');
    await expect(pendingPanel).toHaveClass(/active/);
    await expect(pendingPanel.locator('tbody tr')).toHaveCount(80);
    await expect.poll(() => pendingPanel.evaluate(el => el.scrollHeight - el.clientHeight)).toBeGreaterThan(0);
    const pendingTableLayout = await pendingPanel.evaluate(panel => {
      const approvalActions = document.querySelector('#status-drawer-approval-actions');
      const heading = panel.querySelector('.pending-updates > h4');
      const summary = panel.querySelector('.pending-summary');
      const wrap = panel.querySelector('.pending-updates > .table-wrap');
      const riskHeader = wrap.querySelector('th:last-child').getBoundingClientRect();
      const approvalRect = approvalActions.getBoundingClientRect();
      const headingRect = heading.getBoundingClientRect();
      const summaryRect = summary.getBoundingClientRect();
      const wrapRect = wrap.getBoundingClientRect();
      return {
        approvalToHeadingGap: headingRect.top - approvalRect.bottom,
        headingToSummaryGap: summaryRect.top - headingRect.bottom,
        gapAbove: wrapRect.top - summaryRect.bottom,
        clientWidth: wrap.clientWidth,
        scrollWidth: wrap.scrollWidth,
        riskRight: riskHeader.right,
        visibleRight: wrapRect.right,
      };
    });
    expect(
      Math.abs(pendingTableLayout.approvalToHeadingGap - pendingTableLayout.headingToSummaryGap),
      'approval controls, pending heading, and summary must use the same vertical spacing',
    ).toBeLessThanOrEqual(1);
    expect(pendingTableLayout.gapAbove, 'pending table must be separated from its summary').toBeGreaterThanOrEqual(9);
    expect(pendingTableLayout.scrollWidth, 'pending updates must fit without horizontal scrolling').toBeLessThanOrEqual(pendingTableLayout.clientWidth + 1);
    expect(pendingTableLayout.riskRight, 'the Risk column must remain visible inside the drawer').toBeLessThanOrEqual(pendingTableLayout.visibleRight + 1);

    await pendingPanel.evaluate(el => { el.scrollTop = 520; });
    const beforeRefresh = await pendingPanel.evaluate(el => el.scrollTop);
    expect(beforeRefresh).toBeGreaterThan(0);

    servers = [
      makeServer('demo-host', 'pending_approval', makePendingUpdates(80).map(update => ({ ...update, cve_state: 'ready' })), { tags: ['prod'] }),
      servers[1],
    ];
    await page.evaluate(() => window.fetchServers());

    await expect.poll(() => pendingPanel.evaluate(el => el.scrollTop)).toBeGreaterThanOrEqual(beforeRefresh - 1);

    await page.setViewportSize({ width: 390, height: 844 });
    const mobilePendingLayout = await pendingPanel.locator('.table-wrap').evaluate(element => {
      const cells = Array.from(element.querySelectorAll('tbody tr:first-child td'));
      return {
        clientWidth: element.clientWidth,
        scrollWidth: element.scrollWidth,
        labels: cells.map(cell => getComputedStyle(cell, '::before').content.replaceAll('"', '')),
      };
    });
    expect(mobilePendingLayout.scrollWidth, 'mobile pending updates must fit without horizontal scrolling').toBeLessThanOrEqual(mobilePendingLayout.clientWidth + 1);
    expect(mobilePendingLayout.labels).toEqual(['Package', 'Version', 'Risk']);
  });

  test('cancelled approval refreshes to idle after the short cancellation state', async ({ page }) => {
    let servers = [makeServer('cancelled-host', 'pending_approval', makePendingUpdates(1), { has_key: true })];
    let cancellationCompletedAt = 0;
    await stubDashboardApi(page, () => servers);
    await page.route('**/api/cancel/cancelled-host', async route => {
      await fulfillJson(route, { ok: true });
      setTimeout(() => {
        servers = [makeServer('cancelled-host', 'idle', [], { has_key: true })];
        cancellationCompletedAt = Date.now();
      }, 200);
    });
    await ensureAuthenticatedSession(page);

    await page.locator('#approval-triage-table button[data-action="cancel-upgrade"][data-name="cancelled-host"]').click();
    await expect.poll(() => cancellationCompletedAt).toBeGreaterThan(0);
    await expect.poll(
      () => page.locator('#servers-table tbody tr[data-name="cancelled-host"] .status-pill').textContent(),
      { timeout: 1500 },
    ).toBe('idle');
  });

  test('APT upgrade log drawer renders carriage-return progress during refresh', async ({ page }) => {
    await page.addInitScript(() => {
      window.EventSource = undefined;
    });
    let servers = [
      makeServer('apt-live-host', 'upgrading', [], {
        job_id: 'job-polling',
        logs: 'Running apt upgrade...\nReading database ... 25%\rReading database ... 50%',
      }),
    ];
    await stubDashboardApi(page, () => servers);
    await ensureAuthenticatedSession(page);

    await page.locator('#servers-table tbody button[data-action="open-drawer"][data-tab="logs"]').click();
    const logs = page.locator('#drawer-logs');
    await expect(logs.locator('.log-line')).toHaveText([
      'Running apt upgrade...',
      'Reading database ... 50%',
    ]);

    servers = [
      makeServer('apt-live-host', 'upgrading', [], {
        job_id: 'job-polling',
        logs: 'Running apt upgrade...\nReading database ... 25%\rReading database ... 50%\rUnpacking openssl',
      }),
    ];
    await page.evaluate(() => window.fetchServers(true, 'apt-live-output'));

    await expect(logs.locator('.log-line').last()).toHaveText('Unpacking openssl');
    await expect.poll(() => logs.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThanOrEqual(1);
  });

  test('structured SSE job logs preserve drawer scroll and avoid server refetches', async ({ page }) => {
    await page.addInitScript(() => {
      class TestEventSource {
        constructor() {
          this.listeners = new Map();
          window.__statusEventSource = this;
          setTimeout(() => this.emit('open', {}), 0);
        }
        addEventListener(name, handler) {
          if (!this.listeners.has(name)) this.listeners.set(name, []);
          this.listeners.get(name).push(handler);
        }
        emit(name, event) {
          (this.listeners.get(name) || []).forEach(handler => handler(event));
        }
        close() {}
      }
      window.EventSource = TestEventSource;
    });
    const servers = [
      makeServer('stream-host', 'idle', [], {
        job_id: 'job-live',
        logs: '',
      }),
    ];
    let serverRequests = 0;
    page.on('request', request => {
      if (new URL(request.url()).pathname === '/api/servers') serverRequests += 1;
    });
    await stubDashboardApi(page, () => servers);
    await page.route('**/api/jobs/job-live/logs*', route => {
      const afterSequence = Number(new URL(route.request().url()).searchParams.get('after_seq') || 0);
      return fulfillJson(route, {
        job_id: 'job-live',
        fragments: [],
        next_sequence: afterSequence,
        has_more: false,
        expired: false,
        truncated: false,
        retention_days: 30,
      });
    });
    await ensureAuthenticatedSession(page);
    await page.locator('#servers-table tbody button[data-action="open-drawer"][data-tab="logs"]').click();
    const logs = page.locator('#drawer-logs');
    await expect(logs).toHaveAttribute('role', 'log');
    await expect(logs).toHaveAttribute('aria-live', 'polite');
    await expect.poll(() => page.evaluate(() => !!window.__statusEventSource)).toBe(true);
    const requestsBeforeLogs = serverRequests;

    const initialData = Array.from({ length: 100 }, (_, index) => `line-${index + 1}`).join('\n') + '\nReading 10%\rReading 50%\r';
    await page.evaluate(payload => {
      window.__statusEventSource.emit('dashboard', { data: JSON.stringify(payload) });
    }, {
      reason: 'job.log',
      server_name: 'stream-host',
      job_id: 'job-live',
      sequence: 1,
      stream: 'stdout',
      data: initialData,
    });
    await expect(logs.locator('.log-line').last()).toHaveText('Reading 50%');
    await expect.poll(() => logs.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThanOrEqual(1);

    await logs.evaluate(element => {
      element.scrollTop = 120;
      element.dispatchEvent(new Event('scroll'));
    });
    const pausedScrollTop = await logs.evaluate(element => element.scrollTop);
    await page.evaluate(payload => {
      window.__statusEventSource.emit('dashboard', { data: JSON.stringify(payload) });
    }, {
      reason: 'job.log',
      server_name: 'stream-host',
      job_id: 'job-live',
      sequence: 2,
      stream: 'stderr',
      data: 'warning while paused\n',
    });
    await expect.poll(() => logs.evaluate(element => element.scrollTop)).toBe(pausedScrollTop);
    await expect.poll(() => page.evaluate(() => window.statusPageInteraction.getView().jobLogs['stream-host'].rawText.includes('warning while paused'))).toBe(true);

    await logs.evaluate(element => {
      element.scrollTop = element.scrollHeight;
      element.dispatchEvent(new Event('scroll'));
    });
    await page.evaluate(payload => {
      window.__statusEventSource.emit('dashboard', { data: JSON.stringify(payload) });
    }, {
      reason: 'job.log',
      server_name: 'stream-host',
      job_id: 'job-live',
      sequence: 3,
      stream: 'stdout',
      data: 'followed tail\n',
    });
    await expect.poll(() => logs.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThanOrEqual(1);

    await page.locator('#status-drawer-close').click();
    await page.evaluate(payload => {
      window.__statusEventSource.emit('dashboard', { data: JSON.stringify(payload) });
    }, {
      reason: 'job.log',
      server_name: 'stream-host',
      job_id: 'job-live',
      sequence: 4,
      stream: 'stdout',
      data: '<b>escaped while closed</b>\n',
    });
    await page.locator('#servers-table tbody button[data-action="open-drawer"][data-tab="logs"]').click();
    await expect(logs).toContainText('<b>escaped while closed</b>');
    await expect(logs.locator('b')).toHaveCount(0);
    expect(serverRequests).toBe(requestsBeforeLogs);
  });

  test('bulk actions execute immediately, skip ineligible hosts, and report partial failures', async ({ page }) => {
    const servers = [
      makeServer('idle-host', 'idle', [], { has_key: true }),
      makeServer('ok-sec-host', 'pending_approval', makePendingUpdates(3), {
        has_password: true,
        upgrade_plan: {
          standard_package_count: 2,
          kept_back_package_count: 1,
          standard_security_count: 1,
          total_security_count: 2,
          kept_back_security_plan_available: true,
          kept_back_security_removed_packages: ['old-sec-lib'],
        },
      }),
      makeServer('fail-sec-host', 'pending_approval', makePendingUpdates(3), { has_password: true }),
      makeServer('no-sec-host', 'pending_approval', [], { has_password: true }),
    ];
    const state = {};
    await stubDashboardApi(page, () => servers);
    await page.route('**/api/update/idle-host', route => {
      state.updateIdle = (state.updateIdle || 0) + 1;
      return fulfillJson(route, { ok: true });
    });
    await page.route('**/api/approve/ok-sec-host', route => {
      state.approveStandard = (state.approveStandard || 0) + 1;
      return fulfillJson(route, { ok: true });
    });
    await page.route('**/api/approve-security-kept-back/ok-sec-host', route => {
      state.approveKept = (state.approveKept || 0) + 1;
      return fulfillJson(route, { ok: true });
    });
    await page.route('**/api/approve-security/ok-sec-host', route => {
      state.approveSecurityOk = (state.approveSecurityOk || 0) + 1;
      return fulfillJson(route, { ok: true });
    });
    await page.route('**/api/approve-security/fail-sec-host', route => {
      state.approveSecurityFail = (state.approveSecurityFail || 0) + 1;
      return route.fulfill({
        status: 500,
        contentType: 'application/json',
        body: JSON.stringify({ error: 'backend down' }),
      });
    });
    await page.route('**/api/approve-security/no-sec-host', route => {
      state.approveSecuritySkipped = (state.approveSecuritySkipped || 0) + 1;
      return fulfillJson(route, { ok: true });
    });
    await page.route('**/api/cancel/ok-sec-host', route => {
      state.cancelOk = (state.cancelOk || 0) + 1;
      return fulfillJson(route, { ok: true });
    });
    await page.route('**/api/autoremove/idle-host', route => {
      state.autoremoveIdle = (state.autoremoveIdle || 0) + 1;
      return fulfillJson(route, { ok: true });
    });
    await page.route('**/api/servers/idle-host/facts/refresh', route => {
      state.refreshIdle = (state.refreshIdle || 0) + 1;
      return fulfillJson(route, { ok: true });
    });

    await ensureAuthenticatedSession(page);

    await page.locator('#servers-table tbody tr[data-name="idle-host"] .row-select').check();
    await page.locator('#servers-table tbody tr[data-name="ok-sec-host"] .row-select').check();
    await page.locator('#auth-filter').selectOption('key');
    await page.locator('#bulk-update').click();
    await expect.poll(() => state.updateIdle || 0).toBe(1);
    await expect(page.locator('#bulk-review-modal')).toHaveCount(0);
    await expect(page.locator('#typed-confirm-modal')).not.toBeVisible();
    await page.locator('#auth-filter').selectOption('');
    await page.locator('#servers-table tbody tr[data-name="ok-sec-host"] .row-select').uncheck();

    await page.locator('#servers-table tbody tr[data-name="idle-host"] .row-select').uncheck();
    await page.locator('#servers-table tbody tr[data-name="ok-sec-host"] .row-select').check();
    await page.locator('#bulk-approve').click();
    await expect.poll(() => state.approveStandard || 0).toBe(1);

    await page.locator('#bulk-approve-kept-security').click();
    await expect.poll(() => state.approveKept || 0).toBe(1);

    await page.locator('#servers-table tbody tr[data-name="fail-sec-host"] .row-select').check();
    await page.locator('#servers-table tbody tr[data-name="no-sec-host"] .row-select').check();
    await page.locator('#bulk-approve-security').click();
    await expect.poll(() => state.approveSecurityOk || 0).toBe(1);
    await expect.poll(() => state.approveSecurityFail || 0).toBe(1);
    await expect.poll(() => state.approveSecuritySkipped || 0).toBe(0);
    const failureNotice = page.locator('#app-feedback-region[role="alert"]');
    await expect(failureNotice).toContainText('fail-sec-host: backend down');
    await expect(failureNotice).not.toContainText('no-sec-host: backend down');

    await page.locator('#servers-table tbody tr[data-name="fail-sec-host"] .row-select').uncheck();
    await page.locator('#servers-table tbody tr[data-name="no-sec-host"] .row-select').uncheck();
    await page.locator('#bulk-cancel').click();
    await expect.poll(() => state.cancelOk || 0).toBe(1);

    await page.locator('#servers-table tbody tr[data-name="ok-sec-host"] .row-select').uncheck();
    await page.locator('#servers-table tbody tr[data-name="idle-host"] .row-select').check();
    await page.locator('#bulk-autoremove').click();
    await expect.poll(() => state.autoremoveIdle || 0).toBe(1);

    await page.locator('#refresh-all-facts').click();
    await expect.poll(() => state.refreshIdle || 0).toBe(1);
  });

  test('auto refresh defers table replacement while an update action is being clicked', async ({ page }) => {
    let servers = [makeServer('demo-host')];
    let updateRequests = 0;
    await stubDashboardApi(page, () => servers);
    await page.route('**/api/update/demo-host', route => {
      updateRequests += 1;
      return fulfillJson(route, { ok: true });
    });

    await ensureAuthenticatedSession(page);

    const updateButton = page.locator('#servers-table tbody button[data-action="update-server"][data-name="demo-host"]');
    await expect(updateButton).toBeVisible();

    await updateButton.hover();
    await page.mouse.down();
    await page.waitForTimeout(25);

    servers = [makeServer('renamed-host')];
    await page.evaluate(() => window.fetchServers());
    await expect(page.locator('#servers-table tbody tr[data-name="demo-host"]')).toBeVisible();

    await page.mouse.up();

    await expect.poll(() => updateRequests).toBe(1);
  });

  test('auto refresh resumes when an action press loses page focus', async ({ page }) => {
    let servers = [makeServer('demo-host')];
    await stubDashboardApi(page, () => servers);

    await ensureAuthenticatedSession(page);

    const updateButton = page.locator('#servers-table tbody button[data-action="update-server"][data-name="demo-host"]');
    await expect(updateButton).toBeVisible();

    await updateButton.hover();
    await page.mouse.down();
    await page.waitForTimeout(25);

    servers = [makeServer('renamed-host')];
    await page.evaluate(() => window.fetchServers());
    await expect(page.locator('#servers-table tbody tr[data-name="demo-host"]')).toBeVisible();

    await page.evaluate(() => window.dispatchEvent(new Event('blur')));

    await expect(page.locator('#servers-table tbody tr[data-name="renamed-host"]')).toBeVisible();
    await page.mouse.up();
  });

  test('admin scheduled policy editor submits rich targeting fields and renders report links', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);

    await page.goto('/admin');
    await expect(page.locator('#scheduled-policy-table tbody')).toContainText('Nightly security');
    await expect(page.locator('#scheduled-policy-table tbody')).toContainText('include web');
    await page.locator('[data-admin-section-link="scheduled-policies"]').click();
    await expect(page.locator('#maintenance-calendar-list')).toContainText('Nightly security');
    await expect(page.locator('#maintenance-calendar-list')).toContainText('Allowed 02:00');
    await expect(page.locator('#maintenance-calendar-list')).toContainText('global 23:00-03:00 overnight');
    await page.locator('[data-admin-section-link="scheduled-runs"]').click();
    await expect(page.locator('#scheduled-runs-table a[href="/api/reports/jobs/job-report-1"]')).toBeVisible();
    await page.locator('#scheduled-runs-table button[data-action="job-detail"][data-job-id="job-report-1"]').click();
    await expect(page.locator('#job-detail-modal')).toContainText('Job job-report-1');
    await expect(page.locator('#job-detail-modal')).toContainText('Complete');
    await expect(page.locator('#job-detail-modal')).toContainText('"max_attempts": 3');
    await expect(page.locator('#job-detail-modal')).toContainText('upgrade completed');
    await expect(page.locator('#job-detail-copy-logs')).toBeVisible();
    await expect(page.locator('#job-detail-report')).toHaveAttribute('href', '/api/reports/jobs/job-report-1');
    await page.locator('#job-detail-close').click();

    await page.locator('[data-admin-section-link="notifications"]').click();
    await expect(page.locator('[data-admin-section-lifecycle="notifications"]')).toHaveAttribute('data-status', 'current');
    await page.locator('#notification-enabled').check();
    await page.locator('#notification-webhook-replace').click();
    await page.locator('#notification-webhook-url').fill('https://hooks.example.test/simplelinuxupdater');
    await page.locator('#notification-event-schedule-skipped').uncheck();
    await page.locator('#notification-save').click();
    await expect.poll(() => state.notificationPayload).toMatchObject({
      enabled: true,
      webhook_url: 'https://hooks.example.test/simplelinuxupdater',
      webhook_url_intent: 'replace',
      event_types: ['update.complete', 'schedule.run.failed', 'backup.restore'],
    });
    await expect(page.locator('#notification-status')).toContainText('Webhook URL replaced');
    await page.locator('#notification-test').click();
    await expect.poll(() => state.notificationTestCount || 0).toBe(1);
    await expect(page.locator('#notification-diagnostics-event')).toHaveText('notification.test');
    await expect(page.locator('#notification-diagnostics-outcome')).toHaveText('Succeeded');

    await page.locator('#policy-name').fill('Weekend prod');
    await page.locator('#policy-target-tag').fill('');
    await page.locator('#policy-include-tags').fill('prod, web, prod');
    await page.locator('#policy-exclude-tags').fill('hold, db');
    await page.locator('#policy-target-servers').fill('srv-web-01, srv-web-02');
    await page.locator('#policy-time-local').fill('03:45');
    await page.locator('#policy-execution-mode').selectOption('approval_required');
    await page.locator('#policy-approval-timeout').fill('90');
    await page.locator('#policy-package-scope').selectOption('security');
    await expect(page.locator('#policy-preview')).toContainText('2 matched');
    await expect(page.locator('#policy-preview')).toContainText('srv-web-02');
    await expect(page.locator('#policy-preview')).toContainText('srv-db-01');
    await expect(page.locator('#policy-preview-occurrences')).toContainText('2026-05-17 03:45');
    await expect(page.locator('#policy-preview-occurrences')).toContainText('America/Toronto');
    await expect(page.locator('#policy-preview-occurrences')).toContainText('UTC 2026-05-17T07:45:00.000000000Z');
    await expect(page.locator('#policy-preview-facts')).toContainText('canonical application timezone America/Toronto');

    const previewCountBeforeTimezoneChange = state.policyPreviewCount;
    await page.locator('#app-timezone-input').click();
    await page.locator('#app-timezone-search').fill('+05:30');
    await page.locator('#app-timezone-options [role="option"][data-value="+05:30"]').click();
    await page.locator('#app-timezone-save').click();
    await expect.poll(() => state.policyPreviewCount).toBeGreaterThan(previewCountBeforeTimezoneChange);
    await expect(page.locator('#policy-preview-occurrences')).toContainText('+05:30');
    await expect(page.locator('#policy-preview-occurrences')).toContainText('UTC 2026-05-17T22:15:00.000000000Z');
    await page.locator('#policy-save-btn').click();

    await expect.poll(() => state.policyPreviewPayload).toMatchObject({
      name: 'Weekend prod',
      target_tag: '',
      include_tags: ['prod', 'web'],
      exclude_tags: ['hold', 'db'],
      target_servers: ['srv-web-01', 'srv-web-02'],
    });
    await expect.poll(() => state.policyPayload).toMatchObject({
      name: 'Weekend prod',
      target_tag: '',
      include_tags: ['prod', 'web'],
      exclude_tags: ['hold', 'db'],
      target_servers: ['srv-web-01', 'srv-web-02'],
      execution_mode: 'approval_required',
      approval_timeout_minutes: 90,
    });
  });

  test('admin scheduled run history filters, paginates, and exposes existing investigation details', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);

    await page.goto('/admin');
    await page.locator('[data-admin-section-link="scheduled-runs"]').click();
    await expect(page.locator('#scheduled-runs-result-summary')).toHaveText('26 matching runs.');
    await expect(page.locator('#scheduled-runs-table')).toContainText('1m 30s');
    await expect(page.locator('#scheduled-runs-table')).toContainText('Job details');
    await expect(page.locator('#scheduled-runs-table a', { hasText: 'Audit trail' })).toHaveAttribute('href', /audit_action=schedule\.run\.completed/);

    await page.locator('#scheduled-runs-policy-filter').fill('Nightly');
    await page.locator('#scheduled-runs-server-filter').fill('web');
    await page.locator('#scheduled-runs-outcome-filter').selectOption('skipped');
    await page.locator('#scheduled-runs-from-filter').fill('2026-05-01');
    await page.locator('#scheduled-runs-to-filter').fill('2026-05-31');
    await page.locator('#scheduled-runs-filter').getByRole('button', { name: 'Apply filters' }).click();

    await expect(page.locator('#scheduled-runs-result-summary')).toHaveText('1 matching run.');
    await expect(page.locator('#scheduled-runs-table')).toContainText('Reason: busy');
    await expect.poll(() => state.scheduledRunsQueries.at(-1)).toMatchObject({
      policy: 'Nightly',
      server: 'web',
      outcome: 'skipped',
      from: '2026-05-01',
      to: '2026-05-31',
      page: '1',
      page_size: '25',
    });

    await page.locator('#scheduled-runs-reset').click();
    await expect(page.locator('#scheduled-runs-result-summary')).toHaveText('26 matching runs.');
    await page.locator('#scheduled-runs-next').click();
    await expect(page.locator('#scheduled-runs-page-info')).toHaveText('Page 2 of 2');
    await expect(page.locator('#scheduled-runs-table')).toContainText('Older policy');
    await expect.poll(() => state.scheduledRunsQueries.at(-1)?.page).toBe('2');

    await page.setViewportSize({ width: 390, height: 844 });
    const mobileLayout = await page.evaluate(() => ({
      viewportWidth: window.innerWidth,
      bodyWidth: document.body.scrollWidth,
      cellDisplay: getComputedStyle(document.querySelector('#scheduled-runs-table tbody td')).display,
      filterColumns: getComputedStyle(document.querySelector('.scheduled-runs-filter')).gridTemplateColumns,
    }));
    expect(mobileLayout.bodyWidth).toBeLessThanOrEqual(mobileLayout.viewportWidth);
    expect(mobileLayout.cellDisplay).toBe('grid');
    expect(mobileLayout.filterColumns.trim().split(/\s+/)).toHaveLength(1);

    await page.locator('#scheduled-runs-table a', { hasText: 'Audit trail' }).click();
    await expect(page).toHaveURL(/\/manage\?audit_action=schedule\.run\.failed&audit_target=srv-web-09#audit-trail$/);
    await expect(page.locator('#audit-target-filter')).toHaveValue('srv-web-09');
    await expect(page.locator('#audit-action-filter')).toHaveValue('schedule.run.failed');
  });

  test('admin policy preview explains canonical DST, no-run, and empty-target outcomes', async ({ page }) => {
    const state = {
      policyPreviewResponse: {
        matched_servers: [{ name: 'srv-web-01', tags: ['prod', 'web'] }],
        excluded_servers: [],
        disabled_by_override: [],
        validation_errors: [],
        operational_warnings: [
          { code: 'no_run_window', message: 'One or more upcoming occurrences are blocked by an applicable no-run window.' },
          { code: 'policy_schedule_overlap', message: 'One or more enabled policies target shared servers during the same projected occurrence.' },
        ],
        informational_facts: [
          { code: 'dst_nonexistent_skipped', message: '2026-03-08 02:30 does not exist in America/Toronto; the scheduler skips that local occurrence.' },
          { code: 'dst_fallback_canonical_choice', message: '2026-11-01 01:30 is repeated; the scheduler uses the earlier occurrence.' },
        ],
        upcoming_occurrences: [{
          local_civil_time: '2026-11-01 01:30',
          timezone: 'America/Toronto',
          offset: '-04:00',
          abbreviation: 'EDT',
          scheduled_for_utc: '2026-11-01T05:30:00.000000000Z',
          dst_status: 'ambiguous',
          canonical_choice: 'earlier_fallback_occurrence',
          matched_server_count: 1,
          applicable_no_run_windows: [{
            source: 'global',
            weekdays: ['sun'],
            start_time: '01:00',
            end_time: '02:00',
            overnight: false,
            applies_to_slot: true,
          }],
          admission_outcome: 'blocked_no_run',
        }],
        schedule_conflicts: [{
          policy_id: 7,
          policy_name: 'Nightly production',
          overlap_kind: 'partial',
          shared_servers: ['srv-web-01'],
          occurrence_windows: [
            {
              local_civil_time: '2026-11-01 01:30',
              timezone: 'America/Toronto',
              window_start_utc: '2026-11-01T05:30:00.000000000Z',
              window_end_utc: '2026-11-01T05:31:00.000000000Z',
              draft_admission_outcome: 'blocked_no_run',
              competing_admission_outcome: 'admitted',
              effective: false,
            },
            {
              local_civil_time: '2026-11-02 01:30',
              timezone: 'America/Toronto',
              window_start_utc: '2026-11-02T06:30:00.000000000Z',
              window_end_utc: '2026-11-02T06:31:00.000000000Z',
              draft_admission_outcome: 'admitted',
              competing_admission_outcome: 'admitted',
              effective: true,
            },
          ],
        }],
      },
    };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto('/admin');

    await page.locator('#policy-name').fill('DST policy');
    await page.locator('#policy-target-tag').fill('prod');
    await expect(page.locator('#policy-preview-occurrences')).toContainText('2026-11-01 01:30');
    await expect(page.locator('#policy-preview-occurrences')).toContainText('Repeated local time');
    await expect(page.locator('#policy-preview-occurrences')).toContainText('Expected: blocked by no-run window');
    await expect(page.locator('#policy-preview-occurrences')).toContainText('global 01:00-02:00');
    await expect(page.locator('#policy-preview-facts')).toContainText('does not exist');
    await expect(page.locator('#policy-preview-facts')).toContainText('earlier occurrence');
    await expect(page.locator('#policy-preview-warnings')).toContainText('blocked by an applicable no-run window');
    await expect(page.locator('#policy-preview-conflicts')).toContainText('Nightly production');
    await expect(page.locator('#policy-preview-conflicts')).toContainText('Partial target overlap');
    await expect(page.locator('#policy-preview-conflicts')).toContainText('srv-web-01');
    await expect(page.locator('#policy-preview-conflicts')).toContainText('Suppressed by no-run');
    await expect(page.locator('#policy-preview-conflicts')).toContainText('Effective overlap');
    await expect(page.locator('#policy-preview-conflicts')).toContainText('2026-11-02T06:30:00.000000000Z');
    await expect(page.locator('#policy-save-btn')).toBeEnabled();
    await expect(page.locator('#policy-preview-validation-errors')).toBeEmpty();
    await page.setViewportSize({ width: 390, height: 844 });
    const mobileConflictLayout = await page.locator('#policy-preview-conflicts .preview-conflict').evaluate(element => ({
      bodyWidth: document.body.scrollWidth,
      viewportWidth: document.documentElement.clientWidth,
      conflictScrollWidth: element.scrollWidth,
      conflictClientWidth: element.clientWidth,
    }));
    expect(mobileConflictLayout.bodyWidth).toBeLessThanOrEqual(mobileConflictLayout.viewportWidth);
    expect(mobileConflictLayout.conflictScrollWidth).toBeLessThanOrEqual(mobileConflictLayout.conflictClientWidth + 1);
    await page.setViewportSize({ width: 1280, height: 720 });

    state.policyPreviewResponse = {
      matched_servers: [],
      excluded_servers: [{ name: 'srv-web-01', tags: ['prod'], reason: 'no_target_match' }],
      disabled_by_override: [],
      validation_errors: [],
      operational_warnings: [{ code: 'no_matching_servers', message: 'No current server would be targeted by this policy.' }],
      informational_facts: [{ code: 'application_timezone', message: 'Occurrences use the canonical application timezone America/Toronto.' }],
      schedule_conflicts: [],
      upcoming_occurrences: [{
        local_civil_time: '2026-11-02 01:30',
        timezone: 'America/Toronto',
        offset: '-05:00',
        abbreviation: 'EST',
        scheduled_for_utc: '2026-11-02T06:30:00.000000000Z',
        dst_status: 'standard',
        canonical_choice: 'exact',
        matched_server_count: 0,
        applicable_no_run_windows: [],
        admission_outcome: 'no_matching_servers',
      }],
    };
    const previousPreviewCount = state.policyPreviewCount;
    await page.locator('#policy-target-tag').fill('');
    await page.locator('#policy-target-servers').fill('srv-missing');
    await expect.poll(() => state.policyPreviewCount).toBeGreaterThan(previousPreviewCount);
    await expect(page.locator('#policy-preview-summary')).toContainText('No current server');
    await expect(page.locator('#policy-preview-occurrences')).toContainText('Expected: no matching servers');
    await expect(page.locator('#policy-preview-warnings')).toContainText('No current server would be targeted');
    await expect(page.locator('#policy-preview-conflicts')).toContainText('No enabled policy overlap');

    await page.setViewportSize({ width: 390, height: 844 });
    const mobileLayout = await page.evaluate(() => ({
      bodyWidth: document.body.scrollWidth,
      viewportWidth: document.documentElement.clientWidth,
    }));
    expect(mobileLayout.bodyWidth).toBeLessThanOrEqual(mobileLayout.viewportWidth);
    await expect(page.locator('#policy-preview-occurrences')).toBeVisible();
  });

  test('admin timezone picker searches by city and saves the canonical timezone', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);

    await page.goto('/admin');
    const picker = page.locator('#app-timezone-input');
    const saveTimezone = page.locator('#app-timezone-save');
    await expect(picker).toContainText('Toronto');
    await expect(picker).toContainText('America/Toronto');
    await expect(saveTimezone).toBeDisabled();
    await expect(page.locator('#app-timezone-preview')).toContainText('Current app time:');
    await expect(page.locator('#app-timezone-preview')).toContainText('America/Toronto');
    await expect(page.locator('.app-time-context')).toBeVisible();
    await expect(page.locator('#app-timezone-source')).toHaveText('Explicit timezone');

    const desktopTimeLayout = await page.evaluate(() => {
      const context = document.querySelector('.app-time-context').getBoundingClientRect();
      const pickerPanel = document.querySelector('.app-time-picker-panel').getBoundingClientRect();
      return {
        contextRight: context.right,
        pickerLeft: pickerPanel.left,
        pickerWidth: pickerPanel.width,
      };
    });
    expect(desktopTimeLayout.pickerLeft - desktopTimeLayout.contextRight).toBeGreaterThanOrEqual(16);
    expect(desktopTimeLayout.pickerWidth).toBeLessThanOrEqual(580);

    await page.setViewportSize({ width: 390, height: 844 });
    const mobileTimeLayout = await page.evaluate(() => {
      const context = document.querySelector('.app-time-context').getBoundingClientRect();
      const pickerPanel = document.querySelector('.app-time-picker-panel').getBoundingClientRect();
      return {
        bodyWidth: document.body.scrollWidth,
        viewportWidth: document.documentElement.clientWidth,
        contextBottom: context.bottom,
        pickerTop: pickerPanel.top,
      };
    });
    expect(mobileTimeLayout.pickerTop).toBeGreaterThanOrEqual(mobileTimeLayout.contextBottom + 12);
    expect(mobileTimeLayout.bodyWidth).toBeLessThanOrEqual(mobileTimeLayout.viewportWidth);
    await page.setViewportSize({ width: 1280, height: 720 });

    await picker.click();
    await expect(page.locator('#app-timezone-popover')).toBeVisible();
    const systemDefault = page.locator('#app-timezone-options [role="option"][data-value=""]');
    await expect(systemDefault).toContainText('System default timezone');
    await expect(systemDefault).toContainText('Uses the server timezone detected when saved');
    await expect(systemDefault).not.toContainText('Detected at startup');
    await expect(page.locator('#app-timezone-options [role="option"][data-value="Local"]')).toHaveCount(0);
    const overlap = await page.evaluate(() => {
      const popover = document.querySelector('#app-timezone-popover');
      const followingSection = document.querySelector('.app-time-card + .workspace');
      const popoverRect = popover.getBoundingClientRect();
      const followingRect = followingSection.getBoundingClientRect();
      const x = Math.max(popoverRect.left, followingRect.left) + 24;
      const y = Math.max(popoverRect.top, followingRect.top) + 24;
      const hasOverlap = x < Math.min(popoverRect.right, followingRect.right)
        && y < Math.min(popoverRect.bottom, followingRect.bottom);
      const topElement = hasOverlap ? document.elementFromPoint(x, y) : null;
      return {
        hasOverlap,
        pickerOwnsOverlap: Boolean(topElement && popover.contains(topElement)),
      };
    });
    expect(overlap.hasOverlap).toBe(true);
    expect(overlap.pickerOwnsOverlap).toBe(true);
    await page.locator('#app-timezone-search').fill('paris');
    const paris = page.locator('#app-timezone-options [role="option"][data-value="Europe/Paris"]');
    await expect(paris).toContainText('Paris');
    await expect(paris).toContainText('Europe/Paris');
    await paris.click();

    await expect(picker).toContainText('Paris');
    await expect(page.locator('#app-timezone-popover')).toBeHidden();
    await expect(saveTimezone).toBeEnabled();
    await saveTimezone.click();
    await expect.poll(() => state.timezoneSave).toEqual({ timezone: 'Europe/Paris' });
    await expect(page.locator('#app-timezone-status')).toContainText('App timezone saved');
    await expect(saveTimezone).toBeDisabled();
  });

  test('admin workspace navigation restores disclosure and focuses deep links', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);

    await page.goto('/admin');
    const sectionLinks = page.locator('#admin-section-nav [data-admin-section-link]');
    await expect(sectionLinks).toHaveCount(8);
    await expect(sectionLinks.first()).toHaveAttribute('aria-current', 'location');

    await page.locator('#admin-section-account-security').scrollIntoViewIfNeeded();
    await expect(page.locator('[data-admin-section-link="account-security"]')).toHaveAttribute('aria-current', 'location');

    await page.locator('[data-admin-section-link="backup"]').click();
    await expect(page).toHaveURL(/#admin-section-backup$/);
    await expect(page.locator('#admin-section-backup-heading')).toBeFocused();
    await expect(page.locator('#admin-section-backup-heading')).toHaveCSS('outline-style', 'none');
    await expect(page.locator('[data-admin-section-link="backup"]')).toHaveAttribute('aria-current', 'location');

    await page.locator('[data-admin-section-link="notifications"]').click();
    await expect(page.locator('#admin-section-notifications-heading')).toBeFocused();
    await expect(page.locator('#admin-section-notifications-heading')).toHaveCSS('outline-style', 'none');
    await expect(page.locator('[data-admin-section-lifecycle="notifications"]')).toHaveAttribute('data-status', 'current');
    await page.locator('[data-admin-section-link="backup"]').click();
    await page.locator('[data-admin-section-toggle="notifications"]').click();
    await expect(page.locator('[data-admin-section-content="notifications"]')).toBeHidden();
    await expect(page.locator('[data-admin-section-summary="notifications"]')).toContainText('Webhook');
    await expect.poll(() => page.evaluate(() => JSON.parse(
      localStorage.getItem('simplelinuxupdater.admin.collapsed-sections.v1') || '[]',
    ))).toEqual(['notifications']);

    await page.reload();
    await expect(page.locator('[data-admin-section-content="notifications"]')).toBeHidden();
    await expect(page.locator('[data-admin-section-toggle="notifications"]')).toHaveAttribute('aria-expanded', 'false');

    await page.goto('/admin#admin-section-notifications');
    await expect(page.locator('[data-admin-section-content="notifications"]')).toBeVisible();
    await expect(page.locator('#admin-section-notifications-heading')).toBeFocused();
    await expect(page.locator('[data-admin-section-link="notifications"]')).toHaveAttribute('aria-current', 'location');

    await page.setViewportSize({ width: 390, height: 844 });
    const layout = await page.evaluate(() => ({
      bodyWidth: document.body.scrollWidth,
      viewportWidth: document.documentElement.clientWidth,
      navVisible: Boolean(document.querySelector('#admin-section-nav')?.getClientRects().length),
    }));
    expect(layout.navVisible).toBe(true);
    expect(layout.bodyWidth).toBeLessThanOrEqual(layout.viewportWidth);
  });

  test('admin policy fields align and adjacent action groups keep visible spacing', async ({ page }) => {
    // Chromium can report an authored 8px gap almost 1px lower after Linux
    // font metrics and subpixel rounding are applied.
    const minimumVisibleGap = 7;
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.goto('/admin#admin-section-scheduled-policies');
    await page.evaluate(() => document.fonts.ready);

    const boxes = {};
    for (const [name, selector] of Object.entries({
      policyNameLabel: 'label[for="policy-name"]',
      policyName: '#policy-name',
      targetLabel: 'label[for="policy-target-tag"]',
      target: '#policy-target-tag',
      executionLabel: 'label[for="policy-execution-mode"]',
      execution: '#policy-execution-mode',
      packageLabel: 'label[for="policy-package-scope"]',
      package: '#policy-package-scope',
      blackoutAdd: '#policy-blackout-add',
      blackoutFallback: '#policy-blackout-rows ~ .json-fallback',
    })) {
      boxes[name] = await page.locator(selector).boundingBox();
      expect(boxes[name], `${name} must have a layout box`).not.toBeNull();
    }

    expect(Math.abs(boxes.targetLabel.y - boxes.policyNameLabel.y)).toBeLessThanOrEqual(2);
    expect(Math.abs(boxes.target.y - boxes.policyName.y)).toBeLessThanOrEqual(2);
    expect(Math.abs(boxes.packageLabel.y - boxes.executionLabel.y)).toBeLessThanOrEqual(2);
    expect(Math.abs(boxes.package.y - boxes.execution.y)).toBeLessThanOrEqual(2);
    expect(boxes.blackoutFallback.y - (boxes.blackoutAdd.y + boxes.blackoutAdd.height)).toBeGreaterThanOrEqual(minimumVisibleGap);

    await page.goto('/admin#admin-section-metrics');
    const metricsOverview = await page.locator('.metrics-credential-overview').boundingBox();
    const metricsActions = await page.locator('#metrics-token-generate').locator('..').boundingBox();
    const metricsDanger = await page.locator('#metrics-token-danger-zone').boundingBox();
    expect(metricsOverview).not.toBeNull();
    expect(metricsActions).not.toBeNull();
    expect(metricsDanger).not.toBeNull();
    expect(metricsActions.y - (metricsOverview.y + metricsOverview.height)).toBeGreaterThanOrEqual(minimumVisibleGap);
    expect(metricsDanger.y - (metricsActions.y + metricsActions.height)).toBeGreaterThanOrEqual(minimumVisibleGap);
  });

  test('admin sections load heavy data lazily and recover failed sections with Retry', async ({ page }) => {
    const state = { backupFailuresRemaining: 1 };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.evaluate(() => {
      localStorage.setItem('simplelinuxupdater.admin.collapsed-sections.v1', JSON.stringify([
        'notifications',
        'recent-activity',
        'scheduled-policies',
        'scheduled-runs',
        'backup',
        'metrics',
      ]));
    });

    await page.goto('/admin');
    await expect.poll(() => ({
      notifications: state.notificationLoadCount || 0,
      activity: state.adminActivityLoadCount || 0,
      runs: state.scheduledRunsLoadCount || 0,
      calendar: state.calendarLoadCount || 0,
      backup: state.backupLoadCount || 0,
      metrics: state.metricsLoadCount || 0,
    })).toEqual({ notifications: 0, activity: 0, runs: 0, calendar: 0, backup: 0, metrics: 0 });

    await page.locator('[data-admin-section-toggle="backup"]').click();
    const backupLifecycle = page.locator('[data-admin-section-lifecycle="backup"]');
    await expect.poll(() => state.backupLoadCount || 0).toBe(1);
    await expect(backupLifecycle).toHaveAttribute('data-status', 'failed');
    await expect(backupLifecycle.locator('[data-admin-section-status]')).toHaveText('Failed');
    await expect(backupLifecycle.locator('[data-admin-section-retry]')).toBeVisible();

    await backupLifecycle.locator('[data-admin-section-retry]').click();
    await expect.poll(() => state.backupLoadCount || 0).toBe(2);
    await expect(backupLifecycle).toHaveAttribute('data-status', 'current');
    await expect(backupLifecycle.locator('[data-admin-section-refreshed]')).toContainText('Updated');

    await page.locator('[data-admin-section-toggle="backup"]').click();
    await page.locator('[data-admin-section-toggle="backup"]').click();
    await expect.poll(() => state.backupLoadCount || 0).toBe(2);

    await page.goto('/admin#admin-section-metrics');
    await expect.poll(() => state.metricsLoadCount || 0).toBe(1);
    await expect(page.locator('[data-admin-section-lifecycle="metrics"]')).toHaveAttribute('data-status', 'current');

    await page.goto('/admin#admin-section-scheduled-policies');
    await expect.poll(() => state.calendarLoadCount || 0).toBe(1);
    await expect(page.locator('[data-admin-section-lifecycle="scheduled-policies"]')).toHaveAttribute('data-status', 'current');
  });

  test('metrics credential lifecycle covers generate, use, stale, rotate, disable, restore, and safe copy feedback', async ({ page, context }) => {
    const state = {
      metricsResponse: {
        enabled: false,
        lifecycle_state: 'disabled',
        created_at: '',
        rotated_at: '',
        last_used_at: '',
        last_used_origin_masked: '',
        stale_after_days: 30,
      },
    };
    await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: 'http://127.0.0.1:8080' });
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto('/admin#admin-section-metrics');

    await expect(page.locator('#metrics-token-lifecycle-state')).toHaveText('Disabled');
    await expect(page.locator('#metrics-token-status')).toHaveText('Metrics access is disabled.');
    await expect(page.locator('#metrics-token-generate')).toBeEnabled();
    await expect(page.locator('#metrics-token-rotate')).toBeDisabled();
    await expect(page.locator('#metrics-token-disable')).toBeDisabled();

    await page.locator('#metrics-token-generate').click();
    await expect.poll(() => state.metricsRotateCount || 0).toBe(1);
    await expect(page.locator('#metrics-token-lifecycle-state')).toHaveText('Never used');
    await expect(page.locator('#metrics-token-status')).toContainText('never completed a successful metrics request');
    await expect(page.locator('#metrics-token-once')).toBeVisible();
    await expect(page.locator('#metrics-token-value')).toHaveText('one-time-metrics-token');

    await page.locator('#metrics-token-copy').click();
    await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe('one-time-metrics-token');
    await expect(page.locator('#app-feedback-region')).toHaveText('Metrics token copied.');
    await expect(page.locator('#app-feedback-region')).not.toContainText('one-time-metrics-token');

    state.metricsResponse = {
      enabled: true,
      lifecycle_state: 'current',
      created_at: '2026-05-01T12:00:00Z',
      rotated_at: '',
      last_used_at: '2026-07-27T12:00:00Z',
      last_used_origin_masked: '198.51.100.x',
      stale_after_days: 30,
    };
    await page.reload();
    await expect(page.locator('#metrics-token-lifecycle-state')).toHaveText('Current');
    await expect(page.locator('#metrics-token-last-origin')).toHaveText('198.51.100.x');
    await expect(page.locator('#metrics-token-once')).toBeHidden();
    await expect(page.locator('body')).not.toContainText('one-time-metrics-token');

    state.metricsResponse = {
      enabled: true,
      lifecycle_state: 'stale',
      created_at: '2026-05-01T12:00:00Z',
      rotated_at: '2026-05-15T12:00:00Z',
      last_used_at: '2026-05-16T12:00:00Z',
      last_used_origin_masked: '203.0.113.x',
      stale_after_days: 30,
      token: 'DO-NOT-REVEAL-STATUS-TOKEN',
      last_used_origin: '203.0.113.44',
    };
    await page.reload();
    await expect(page.locator('#metrics-token-lifecycle-state')).toHaveText('Stale');
    await expect(page.locator('#metrics-token-status')).toContainText('access remains enabled');
    await expect(page.locator('#metrics-token-last-origin')).toHaveText('203.0.113.x');
    await expect(page.locator('#metrics-token-stale-policy')).toContainText('never disabled automatically');
    await expect(page.locator('body')).not.toContainText('DO-NOT-REVEAL-STATUS-TOKEN');
    await expect(page.locator('body')).not.toContainText('203.0.113.44');

    state.metricsRotateResponse = {
      enabled: true,
      lifecycle_state: 'never_used',
      created_at: '2026-05-01T12:00:00Z',
      rotated_at: '2026-07-28T12:00:00Z',
      last_used_at: '',
      last_used_origin_masked: '',
      stale_after_days: 30,
      token: 'rotated-one-time-token',
    };
    await dismissTypedConfirm(page, page.locator('#metrics-token-rotate'));
    expect(state.metricsRotateCount).toBe(1);
    await acceptTypedConfirm(page, page.locator('#metrics-token-rotate'), 'ROTATE TOKEN');
    await expect.poll(() => state.metricsRotateCount || 0).toBe(2);
    await expect(page.locator('#metrics-token-value')).toHaveText('rotated-one-time-token');
    await expect(page.locator('#metrics-token-lifecycle-state')).toHaveText('Never used');
    await expect(page.locator('#metrics-token-last-used')).toHaveText('Never');

    await acceptTypedConfirm(page, page.locator('#metrics-token-disable'), 'DISABLE METRICS');
    await expect.poll(() => state.metricsDisableCount || 0).toBe(1);
    await expect(page.locator('#metrics-token-lifecycle-state')).toHaveText('Disabled');
    await expect(page.locator('#metrics-token-once')).toBeHidden();
    await expect(page.locator('#metrics-token-danger-zone')).toContainText('immediately stops every scraper');

    state.metricsResponse = {
      enabled: true,
      lifecycle_state: 'never_used',
      created_at: '2026-04-01T12:00:00Z',
      rotated_at: '',
      last_used_at: '',
      last_used_origin_masked: '',
      stale_after_days: 30,
    };
    await page.reload();
    await expect(page.locator('#metrics-token-lifecycle-state')).toHaveText('Never used');
    await expect(page.locator('#metrics-token-created')).not.toHaveText('Unavailable');
    await expect(page.locator('#metrics-token-last-used')).toHaveText('Never');
    await page.setViewportSize({ width: 390, height: 844 });
    const mobileMetricsLayout = await page.evaluate(() => ({
      bodyWidth: document.body.scrollWidth,
      viewportWidth: document.documentElement.clientWidth,
      factColumns: getComputedStyle(document.querySelector('#metrics-token-lifecycle-facts')).gridTemplateColumns.split(' ').length,
    }));
    expect(mobileMetricsLayout.bodyWidth).toBeLessThanOrEqual(mobileMetricsLayout.viewportWidth);
    expect(mobileMetricsLayout.factColumns).toBe(1);
  });

  test('recent Admin activity loads on demand with safe ordered facts and retains stale results', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.evaluate(() => {
      localStorage.setItem('simplelinuxupdater.admin.collapsed-sections.v1', JSON.stringify(['recent-activity']));
    });

    await page.goto('/admin');
    await expect.poll(() => state.adminActivityLoadCount || 0).toBe(0);
    await page.locator('[data-admin-section-link="recent-activity"]').click();
    const lifecycle = page.locator('[data-admin-section-lifecycle="recent-activity"]');
    const list = page.locator('#admin-activity-list');
    await expect(lifecycle).toHaveAttribute('data-status', 'current');
    await expect.poll(() => new URL(state.adminActivityURLs[0]).searchParams.get('category')).toBe('admin_activity');
    expect(new URL(state.adminActivityURLs[0]).searchParams.get('page_size')).toBe('8');
    await expect(list.locator('.admin-activity-item')).toHaveCount(2);
    await expect(list.locator('.admin-activity-action')).toHaveText(['Session IP revealed', 'Policy updated']);
    await expect(list).toContainText('2026-05-17 08:05 EDT · America/Toronto');
    await expect(list).toContainText('Actor: admin');
    await expect(list).toContainText('success');
    await expect(list.locator('a').first()).toHaveAttribute('href', '/api/reports/audit/82');
    await expect(page.locator('#admin-section-recent-activity a[href="/manage#audit-trail"]')).toBeVisible();
    const pageText = await page.locator('#admin-section-recent-activity').innerText();
    expect(pageText).not.toContain('DO-NOT-RENDER-MESSAGE');
    expect(pageText).not.toContain('DO-NOT-RENDER-META');
    expect(pageText).not.toContain('DO-NOT-RENDER-REQUEST');
    expect(pageText).not.toContain('203.0.113.44');
    expect(pageText).not.toContain('DO-NOT-RENDER-TARGET');

    state.adminActivityFailuresRemaining = 1;
    await page.evaluate(() => window.fetchRecentAdminActivity());
    await expect(lifecycle).toHaveAttribute('data-status', 'stale');
    await expect(page.locator('#admin-activity-message')).toContainText('Showing the last successful results');
    await expect(list.locator('.admin-activity-item')).toHaveCount(2);
    await lifecycle.locator('[data-admin-section-retry]').click();
    await expect(lifecycle).toHaveAttribute('data-status', 'current');

    state.adminActivityResponse = { items: [], total: 0, page: 1, page_size: 8 };
    await page.reload();
    await expect(page.locator('#admin-activity-message')).toHaveText('No recent administrative activity.');
    await expect(page.locator('#admin-activity-list .admin-activity-item')).toHaveCount(0);
  });

  test('notification webhook settings preserve, replace, clear, and mask secret-bearing URLs', async ({ page }) => {
    const state = {
      notificationConfiguredURL: 'https://hooks.example.test/path?token=stored-secret',
    };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto('/admin#admin-section-notifications');
    await expect(page.locator('[data-admin-section-lifecycle="notifications"]')).toHaveAttribute('data-status', 'current');

    await expect(page.locator('#notification-webhook-masked')).toHaveText('https://hooks.example.test/••••');
    await expect(page.locator('#notification-webhook-editor')).toBeHidden();
    await expect(page.locator('#notification-webhook-url')).toHaveValue('');
    expect(await page.locator('#admin-section-notifications').innerText()).not.toContain('stored-secret');
    await expect(page.locator('#notification-webhook-policy')).toContainText('Use HTTPS for public endpoints');
    await expect(page.locator('#notification-webhook-policy')).toContainText('Embedded URL credentials are not supported');

    await page.locator('#notification-event-schedule-skipped').uncheck();
    await page.locator('#notification-save').click();
    await expect.poll(() => state.notificationPayload).toMatchObject({
      webhook_url_intent: 'preserve',
    });
    expect(state.notificationPayload).not.toHaveProperty('webhook_url');
    await expect(page.locator('#notification-status')).toContainText('configured URL was preserved');
    await expect(page.locator('#notification-webhook-masked')).toHaveText('https://hooks.example.test/••••');

    await page.locator('#notification-webhook-replace').click();
    await expect(page.locator('#notification-webhook-editor')).toBeVisible();
    await page.locator('#notification-webhook-url').fill('http://public.example.test/hook');
    await expect(page.locator('#notification-save')).toBeDisabled();
    await expect(page.locator('#notification-draft-state')).toContainText('Use HTTPS for public endpoints');
    await page.locator('#notification-webhook-url').fill('https://replacement.example.test/hook?token=replacement-secret');
    await expect(page.locator('#notification-save')).toBeEnabled();
    await page.locator('#notification-save').click();
    await expect.poll(() => state.notificationPayload).toMatchObject({
      webhook_url_intent: 'replace',
      webhook_url: 'https://replacement.example.test/hook?token=replacement-secret',
    });
    await expect(page.locator('#notification-status')).toContainText('Webhook URL replaced');
    await expect(page.locator('#notification-webhook-editor')).toBeHidden();
    expect(await page.locator('#admin-section-notifications').innerText()).not.toContain('replacement-secret');

    await page.locator('#notification-webhook-clear').click();
    await expect(page.locator('#notification-webhook-masked')).toHaveText('Will be cleared when saved');
    await page.locator('#notification-save').click();
    await expect.poll(() => state.notificationPayload).toMatchObject({
      enabled: false,
      webhook_url_intent: 'clear',
    });
    expect(state.notificationPayload).not.toHaveProperty('webhook_url');
    await expect(page.locator('#notification-status')).toContainText('Webhook URL cleared');
    await expect(page.locator('#notification-webhook-masked')).toHaveText('Not configured');
    await expect(page.locator('#notification-webhook-clear')).toBeDisabled();
  });

  test('notification delivery health exposes safe retry diagnostics and independent test outcomes', async ({ page }) => {
    const state = {
      notificationConfiguredURL: 'https://hooks.example.test/path?token=stored-secret',
      notificationDiagnosticsResponse: {
        event_type: 'update.complete',
        action: 'update.complete',
        target_name: 'srv-a',
        outcome: 'retrying',
        success: false,
        attempts: 2,
        status_code: 503,
        error: 'webhook returned HTTP 503',
        attempted_at: '2026-05-17T12:00:00Z',
        completed_at: '2026-05-17T12:00:00Z',
        duration_ms: 820,
        consecutive_failures: 2,
        next_retry_at: '2026-05-17T12:00:05Z',
        response_body: 'DO-NOT-RENDER-REMOTE-BODY',
        headers: { authorization: 'DO-NOT-RENDER-AUTHORIZATION' },
      },
    };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto('/admin#admin-section-notifications');

    const facts = page.locator('#notification-diagnostics-facts');
    await expect(facts).toBeVisible();
    await expect(page.locator('#notification-diagnostics-outcome')).toHaveText('Retry scheduled');
    await expect(page.locator('#notification-diagnostics-http')).toHaveText('HTTP 503');
    await expect(page.locator('#notification-diagnostics-duration')).toHaveText('820 ms');
    await expect(page.locator('#notification-diagnostics-failures')).toHaveText('2');
    await expect(page.locator('#notification-diagnostics-retry-row')).toBeVisible();
    await expect(page.locator('#notification-diagnostics-message')).toContainText('automatic retry is scheduled');
    expect(await page.locator('#admin-section-notifications').innerText()).not.toContain('DO-NOT-RENDER');
    expect(await page.locator('#admin-section-notifications').innerText()).not.toContain('stored-secret');

    state.notificationFailuresRemaining = 1;
    await page.evaluate(() => window.fetchNotificationSettings());
    await expect(page.locator('[data-admin-section-lifecycle="notifications"]')).toHaveAttribute('data-status', 'stale');
    await expect(page.locator('#notification-diagnostics-outcome')).toHaveText('Retry scheduled');

    state.notificationDiagnosticsFailuresRemaining = 1;
    await page.locator('#notification-diagnostics-retry').click();
    await expect(page.locator('#notification-diagnostics-message')).toContainText('Showing the last successful diagnostics');
    await expect(page.locator('#notification-diagnostics-http')).toHaveText('HTTP 503');

    state.notificationDiagnosticsResponse = {
      event_type: 'update.complete',
      outcome: 'failed',
      attempts: 3,
      status_code: 502,
      error: 'webhook returned HTTP 502',
      attempted_at: '2026-05-17T12:01:00Z',
      completed_at: '2026-05-17T12:01:01Z',
      duration_ms: 1040,
      consecutive_failures: 3,
      next_retry_at: '2026-05-17T12:01:05Z',
    };
    await page.locator('#notification-diagnostics-retry').click();
    await expect(page.locator('#notification-diagnostics-outcome')).toHaveText('Failed');
    await expect(page.locator('#notification-diagnostics-retry-row')).toBeHidden();
    await expect(page.locator('#notification-diagnostics-message')).toContainText('no retry is scheduled');

    state.notificationTestFailure = true;
    state.notificationTestResponse = {
      event_type: 'notification.test',
      action: 'notification.test',
      target_name: 'webhook',
      outcome: 'failed',
      success: false,
      attempts: 3,
      status_code: 504,
      error: 'Webhook delivery timed out.',
      attempted_at: '2026-05-17T12:02:00Z',
      completed_at: '2026-05-17T12:02:02Z',
      duration_ms: 2000,
      consecutive_failures: 6,
      request_headers: 'DO-NOT-RENDER-TEST-HEADER',
      payload: 'DO-NOT-RENDER-TEST-PAYLOAD',
    };
    await page.locator('#notification-test').click();
    await expect(page.locator('#notification-error')).toContainText('Notification test failed');
    await expect(page.locator('#notification-diagnostics-event')).toHaveText('notification.test');
    await expect(page.locator('#notification-diagnostics-outcome')).toHaveText('Failed');
    await expect(page.locator('#notification-diagnostics-failures')).toHaveText('6');
    expect(await page.locator('#admin-section-notifications').innerText()).not.toContain('DO-NOT-RENDER');

    state.notificationTestFailure = false;
    state.notificationTestResponse = {
      event_type: 'notification.test',
      action: 'notification.test',
      target_name: 'webhook',
      outcome: 'succeeded',
      success: true,
      attempts: 1,
      status_code: 204,
      attempted_at: '2026-05-17T12:03:00Z',
      completed_at: '2026-05-17T12:03:00Z',
      duration_ms: 95,
      consecutive_failures: 0,
    };
    await page.locator('#notification-test').click();
    await expect(page.locator('#notification-status')).toContainText('Notification test delivered');
    await expect(page.locator('#notification-diagnostics-outcome')).toHaveText('Succeeded');
    await expect(page.locator('#notification-diagnostics-failures')).toHaveText('0');
    await expect(page.locator('#notification-diagnostics-error-row')).toBeHidden();
  });

  test('backup recovery health distinguishes healthy, stale, never, failed, and unavailable evidence', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto('/admin#admin-section-backup');

    const healthState = page.locator('#backup-recovery-health-state');
    const healthPanel = page.locator('.backup-recovery-health');
    const lifecycle = page.locator('[data-admin-section-lifecycle="backup"]');
    await expect(healthState).toHaveText('Healthy');
    await expect(healthPanel).toContainText('4.0 KB');
    await expect(healthPanel).toContainText('No backup is scheduled.');
    await expect(healthPanel).toContainText('up to 90 days');
    await expect(healthPanel).toContainText('not retained or deleted by the app');

    state.backupStatusResponse = {
      db_path: '/tmp/simplelinuxupdater.db',
      known_hosts_path: '/tmp/known_hosts',
      recovery_health: backupRecoveryHealthFixture('stale'),
    };
    await page.reload();
    await expect(healthState).toHaveText('Stale');
    await expect(page.locator('#backup-recovery-health-message')).toContainText('older than the 168-hour threshold');

    state.backupStatusResponse.recovery_health = backupRecoveryHealthFixture('never');
    await page.reload();
    await expect(healthState).toHaveText('Never recorded');
    await expect(page.locator('#backup-recovery-export')).toHaveText('Never recorded');
    await expect(page.locator('#backup-recovery-verification')).toHaveText('Never recorded');

    state.backupStatusResponse.recovery_health = backupRecoveryHealthFixture('failed');
    await page.reload();
    await expect(healthState).toHaveText('Failed');
    await expect(page.locator('#backup-recovery-export-detail')).toContainText('Latest attempt failed');
    await expect(page.locator('#backup-recovery-export')).toContainText('4.0 KB');

    state.backupStatusResponse.recovery_health = backupRecoveryHealthFixture('healthy');
    await page.reload();
    await expect(healthState).toHaveText('Healthy');
    state.backupFailuresRemaining = 1;
    await page.evaluate(() => window.fetchBackupStatus());
    await expect(lifecycle).toHaveAttribute('data-status', 'stale');
    await expect(healthState).toHaveText('Healthy');
    await expect(lifecycle.locator('[data-admin-section-retry]')).toBeVisible();

    state.backupFailuresRemaining = 1;
    await page.reload();
    await expect(lifecycle).toHaveAttribute('data-status', 'failed');
    await expect(healthState).toHaveText('Unavailable');
    await lifecycle.locator('[data-admin-section-retry]').click();
    await expect(lifecycle).toHaveAttribute('data-status', 'current');
    await expect(healthState).toHaveText('Healthy');
  });

  test('admin section drafts support save eligibility, discard, replacement confirmation, and safe navigation warnings', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto('/admin');

    const dispatchBeforeUnload = () => page.evaluate(() => {
      const event = new Event('beforeunload', { cancelable: true });
      return !window.dispatchEvent(event);
    });

    await page.locator('#auth-current-password').fill('secret-current-password');
    await page.locator('#backup-export-passphrase').fill('secret-backup-passphrase');
    expect(await dispatchBeforeUnload()).toBe(false);

    await page.locator('[data-admin-section-link="notifications"]').click();
    await expect(page.locator('[data-admin-section-lifecycle="notifications"]')).toHaveAttribute('data-status', 'current');
    await expect(page.locator('#notification-save')).toBeDisabled();
    await page.locator('#notification-webhook-replace').click();
    await page.locator('#notification-webhook-url').fill('https://hooks.example.test/replacement');
    await page.locator('#notification-enabled').check();
    await expect(page.locator('#notification-draft-state')).toContainText('Webhook URL will be replaced');
    await expect(page.locator('#notification-save')).toBeEnabled();
    expect(await dispatchBeforeUnload()).toBe(true);
    await page.locator('#notification-discard').click();
    await expect(page.locator('#notification-webhook-url')).toHaveValue('');
    await expect(page.locator('#notification-enabled')).not.toBeChecked();
    await expect(page.locator('#notification-save')).toBeDisabled();
    expect(await dispatchBeforeUnload()).toBe(false);

    await expect(page.locator('#policy-draft-action-bar')).toBeHidden();
    await page.locator('#policy-name').fill('Unsaved policy');
    await expect(page.locator('#policy-draft-action-bar')).toBeVisible();
    await expect(page.locator('#policy-save-btn')).toBeDisabled();
    await page.locator('#policy-target-tag').fill('prod');
    await expect(page.locator('#policy-save-btn')).toBeEnabled();

    const editPolicy = page.locator('button[data-action="edit-policy"][data-id="12"]');
    await editPolicy.click();
    await expect(page.locator('#action-confirm-modal')).toBeVisible();
    await expect(page.locator('#action-confirm-title')).toHaveText('Discard unsaved policy changes');
    await page.locator('#action-confirm-cancel').click();
    await expect(page.locator('#policy-name')).toHaveValue('Unsaved policy');

    await editPolicy.click();
    await page.locator('#action-confirm-submit').click();
    await expect(page.locator('#action-confirm-modal')).not.toBeVisible();
    await expect(page.locator('#policy-name')).toHaveValue('Nightly security');
    await expect(page.locator('#policy-draft-action-bar')).toBeHidden();

    await page.locator('#policy-name').fill('Changed accepted policy');
    await expect(page.locator('#policy-draft-action-bar')).toBeVisible();
    await page.locator('#policy-discard-btn').click();
    await expect(page.locator('#policy-name')).toHaveValue('Nightly security');
    await expect(page.locator('#policy-draft-action-bar')).toBeHidden();
  });

  test('admin typed confirmations gate restore and policy deletion', async ({ page }) => {
    const state = { deferRestore: true };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);

    await page.goto('/admin');
    await page.locator('#backup-restore-file').setInputFiles({
      name: 'backup.slubkp',
      mimeType: 'application/octet-stream',
      buffer: Buffer.from('fake-backup'),
    });
    await page.locator('#backup-restore-passphrase').fill('LongPassphrase123');
    await expect(page.locator('#backup-restore-btn')).toBeDisabled();
    await page.locator('#backup-verify-btn').click();
    await expect.poll(() => state.verifyCount || 0).toBe(1);
    await expect(page.locator('#backup-review-readiness')).toHaveText('Ready for confirmation');
    await expect(page.locator('#backup-restore-review')).toContainText('5');
    await expect(page.locator('#backup-restore-review')).toContainText('All active Admin sessions will be invalidated');
    await expect(page.locator('#backup-restore-btn')).toBeEnabled();
    await dismissTypedConfirm(page, page.locator('#backup-restore-btn'));
    await expect.poll(() => state.restoreCount || 0).toBe(0);
    await expect(page.locator('#backup-restore-btn')).toBeEnabled();

    await page.evaluate(() => { window.alert = () => {}; });
    await acceptTypedConfirm(page, page.locator('#backup-restore-btn'), 'RESTORE');
    await expect.poll(() => state.restoreCount || 0).toBe(1);
    await expect(page.locator('#auth-sessions-clear')).toBeDisabled();
    await expect(page.locator('#scheduled-policy-table button[data-action="delete-policy"][data-id="12"]')).toBeDisabled();
    state.releaseRestore();
    await expect.poll(() => state.restoreCompleted || false).toBe(true);
    await expect(page.locator('#auth-sessions-clear')).toBeEnabled();

    const deletePolicyButton = page.locator('#scheduled-policy-table button[data-action="delete-policy"][data-id="12"]');
    await dismissTypedConfirm(page, deletePolicyButton);
    await expect.poll(() => state.policyDeleteCount || 0).toBe(0);

    await acceptTypedConfirm(page, deletePolicyButton, 'Nightly security');
    await expect.poll(() => state.policyDeleteCount || 0).toBe(1);
  });

  test('restore readiness distinguishes blockers and expires when the archive or passphrase changes', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto('/admin');
    await page.locator('[data-admin-section-link="backup"]').click();

    await page.locator('#backup-restore-file').setInputFiles({
      name: 'ready.slubkp',
      mimeType: 'application/octet-stream',
      buffer: Buffer.from('ready-backup'),
    });
    await page.locator('#backup-restore-passphrase').fill('LongPassphrase123');
    await page.locator('#backup-verify-btn').click();
    await expect(page.locator('#backup-review-readiness')).toHaveText('Ready for confirmation');
    await expect(page.locator('#backup-review-warnings')).toContainText('Metrics API credential');
    await expect(page.locator('#backup-restore-btn')).toBeEnabled();

    await page.locator('#backup-restore-file').setInputFiles({
      name: 'future.slubkp',
      mimeType: 'application/octet-stream',
      buffer: Buffer.from('future-backup'),
    });
    await expect(page.locator('#backup-restore-review')).toBeHidden();
    await expect(page.locator('#backup-restore-btn')).toBeDisabled();

    state.verifyResponse = {
      valid: false,
      compatible: false,
      restore_ready: false,
      archive: {
        format: 'simplelinuxupdater-backup',
        version: 99,
        created_at: '2026-06-01T08:00:00Z',
        size_bytes: 8192,
      },
      resources: [],
      missing_resources: [],
      safe_counts: {},
      impact: {
        sessions_invalidated: true,
        metrics_access_replaced: true,
        maintenance_required: true,
        downtime_expected: true,
        restart_required: false,
      },
      blockers: [{ code: 'unsupported_version', message: 'Backup version 99 is not supported.' }],
      warnings: [],
    };
    await page.locator('#backup-verify-btn').click();
    await expect(page.locator('#backup-review-readiness')).toHaveText('Blocked');
    await expect(page.locator('#backup-review-blockers')).toContainText('Backup version 99 is not supported');
    await expect(page.locator('#backup-restore-btn')).toBeDisabled();

    state.verifyResponse = undefined;
    await page.locator('#backup-restore-passphrase').fill('AnotherLongPassphrase456');
    await expect(page.locator('#backup-restore-review')).toBeHidden();
    await page.locator('#backup-verify-btn').click();
    await expect(page.locator('#backup-review-readiness')).toHaveText('Ready for confirmation');
    await expect(page.locator('#backup-restore-btn')).toBeEnabled();
  });

  test('admin danger dialogs explain impact, trap keyboard focus, isolate the page, and restore focus', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto('/admin');

    await page.locator('[data-admin-section-link="account-security"]').click();
    const revokeSession = page.locator('#auth-session-inventory button[data-session-id="other-session"]');
    await revokeSession.focus();
    await page.keyboard.press('Enter');
    const actionModal = page.locator('#action-confirm-modal');
    await expect(actionModal).toBeVisible();
    await expect(actionModal.locator('#action-confirm-title')).toHaveText('Revoke server-side session');
    await expect(actionModal.locator('[data-confirm-operation]')).toContainText('Revoke one server-side session');
    await expect(actionModal.locator('[data-confirm-resources]')).toContainText('Firefox · Linux');
    await expect(actionModal.locator('[data-confirm-consequences]')).toContainText('logged out immediately');
    await expect(actionModal.locator('[data-confirm-reversibility]')).toContainText('Not reversible');
    await expect(actionModal.locator('[data-confirm-authentication]')).toContainText('signed-in Admin session');
    await expect(page.locator('.dashboard-shell')).toHaveAttribute('inert', '');
    await expect(page.locator('#action-confirm-cancel')).toBeFocused();
    await page.keyboard.press('Shift+Tab');
    await expect(page.locator('#action-confirm-submit')).toBeFocused();
    await page.keyboard.press('Escape');
    await expect(actionModal).toBeHidden();
    await expect(revokeSession).toBeFocused();
    await expect(page.locator('.dashboard-shell')).not.toHaveAttribute('inert', '');

    await page.emulateMedia({ reducedMotion: 'reduce' });
    await page.setViewportSize({ width: 390, height: 640 });
    await page.locator('[data-admin-section-link="backup"]').click();
    await page.locator('#backup-restore-file').setInputFiles({
      name: 'backup.slubkp',
      mimeType: 'application/octet-stream',
      buffer: Buffer.from('fake-backup'),
    });
    await page.locator('#backup-restore-passphrase').fill('LongPassphrase123');
    await page.locator('#backup-verify-btn').click();
    await expect(page.locator('#backup-review-readiness')).toHaveText('Ready for confirmation');
    const restore = page.locator('#backup-restore-btn');
    await restore.focus();
    await page.keyboard.press('Enter');
    const typedModal = page.locator('#typed-confirm-modal');
    await expect(typedModal).toBeVisible();
    await expect(typedModal.locator('#typed-confirm-title')).toHaveText('Restore application backup');
    await expect(typedModal.locator('[data-confirm-resources]')).toContainText('backup.slubkp');
    await expect(typedModal.locator('[data-confirm-authentication]')).toContainText('backup passphrase');
    await expect(page.locator('#typed-confirm-input')).toBeFocused();
    await page.keyboard.press('Shift+Tab');
    await expect(page.locator('#typed-confirm-cancel')).toBeFocused();
    await page.keyboard.press('Tab');
    await expect(page.locator('#typed-confirm-input')).toBeFocused();
    await expect(typedModal.locator('.confirmation-modal')).toBeInViewport();
    await page.keyboard.press('Escape');
    await expect(typedModal).toBeHidden();
    await expect(restore).toBeFocused();
  });

  test('admin password change sends payload and session clear requires typed confirmation', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);

    await page.goto('/admin');
    await expect(page.locator('#auth-session-status')).toContainText('2 active sessions');
    await expect(page.locator('#auth-session-inventory')).toContainText('Chrome · Windows');
    await expect(page.locator('#auth-session-inventory')).toContainText('Firefox · Linux');
    await expect(page.locator('#auth-session-inventory')).toContainText('192.168.1.x');
    await page.locator('#auth-session-inventory button[data-session-id="other-session"]').click();
    await expect(page.locator('#action-confirm-modal')).toBeVisible();
    await expect(page.locator('#action-confirm-title')).toHaveText('Revoke server-side session');
    await page.locator('#action-confirm-cancel').click();
    await expect.poll(() => state.sessionRevokeID || '').toBe('');

    await page.locator('#auth-sessions-clear-others').click();
    await expect(page.locator('#action-confirm-modal')).toBeVisible();
    await page.locator('#action-confirm-submit').click();
    await expect.poll(() => state.sessionClearOthersCount || 0).toBe(1);
    await expect(page.locator('#auth-session-status')).toContainText('1 active session');
    await expect(page.locator('#auth-sessions-clear-others')).toBeDisabled();

    await page.locator('#auth-current-password').fill(password);
    await page.locator('#auth-new-password').fill(changedPassword);
    await page.locator('#auth-confirm-password').fill(changedPassword);
    await page.locator('#auth-password-save').click();
    await expect.poll(() => state.passwordPayload).toEqual({
      current_password: password,
      new_password: changedPassword,
      confirm_password: changedPassword,
      invalidate_other_sessions: true,
    });
    await expect(page.locator('#auth-password-status')).toContainText('Password changed');

    await dismissTypedConfirm(page, page.locator('#auth-sessions-clear'));
    await expect.poll(() => state.sessionClearCount || 0).toBe(0);

    await page.evaluate(() => {
      window.location.assign = () => {};
    });
    await acceptTypedConfirm(page, page.locator('#auth-sessions-clear'), 'LOGOUT ALL');
    await expect.poll(() => state.sessionClearCount || 0).toBe(1);
  });

  test('admin password controls expose requirements, entry aids, and both session outcomes', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto('/admin');

    await expect(page.locator('#auth-password-requirements')).toContainText('10–64 characters');
    await expect(page.locator('#auth-password-requirements')).toContainText('At least one letter');
    await expect(page.locator('#auth-password-requirements')).toContainText('At least one digit');
    const current = page.locator('#auth-current-password');
    await current.fill(password);
    await page.getByRole('button', { name: 'Show current password' }).click();
    await expect(current).toHaveAttribute('type', 'text');
    await expect(page.getByRole('button', { name: 'Hide current password' })).toHaveAttribute('aria-pressed', 'true');
    await page.getByRole('button', { name: 'Hide current password' }).click();
    await expect(current).toHaveAttribute('type', 'password');

    await current.evaluate(input => {
      const event = new KeyboardEvent('keydown', { key: 'A', bubbles: true });
      Object.defineProperty(event, 'getModifierState', { value: key => key === 'CapsLock' });
      input.dispatchEvent(event);
    });
    await expect(page.locator('#auth-password-caps-warning')).toBeVisible();
    await current.evaluate(input => {
      const event = new KeyboardEvent('keyup', { key: 'a', bubbles: true });
      Object.defineProperty(event, 'getModifierState', { value: () => false });
      input.dispatchEvent(event);
    });
    await expect(page.locator('#auth-password-caps-warning')).toBeHidden();

    await page.locator('#auth-new-password').fill(changedPassword);
    await page.locator('#auth-confirm-password').fill('DifferentPass123');
    await expect(page.locator('#auth-password-save')).toBeDisabled();
    await expect(page.locator('#auth-password-save')).toHaveAttribute('title', 'New passwords do not match.');

    await page.locator('#auth-confirm-password').fill(changedPassword);
    await page.locator('#auth-password-invalidate-others').uncheck();
    await page.locator('#auth-password-save').click();
    await expect.poll(() => state.passwordPayloads?.length || 0).toBe(1);
    await expect(state.passwordPayloads[0]).toMatchObject({ invalidate_other_sessions: false });
    await expect(page.locator('#auth-password-status')).toHaveText('Password changed. 0 sessions invalidated; 2 active sessions preserved.');
    await expect(page.locator('#auth-current-password')).toHaveValue('');
    await expect(page.locator('#auth-new-password')).toHaveValue('');
    await expect(page.locator('#auth-confirm-password')).toHaveValue('');

    await page.locator('#auth-current-password').fill(password);
    await page.locator('#auth-new-password').fill(changedPassword);
    await page.locator('#auth-confirm-password').fill(changedPassword);
    await page.locator('#auth-password-invalidate-others').check();
    await page.locator('#auth-password-save').click();
    await expect.poll(() => state.passwordPayloads?.length || 0).toBe(2);
    await expect(state.passwordPayloads[1]).toMatchObject({ invalidate_other_sessions: true });
    await expect(page.locator('#auth-password-status')).toHaveText('Password changed. 1 other session invalidated; 1 active session preserved.');
    await expect(page.locator('#auth-session-status')).toHaveText('1 active session.');
  });

  test('admin password change reports partial session invalidation without retaining secrets', async ({ page }) => {
    const state = { passwordPartialFailure: true };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto('/admin');

    await page.locator('#auth-current-password').fill(password);
    await page.locator('#auth-new-password').fill(changedPassword);
    await page.locator('#auth-confirm-password').fill(changedPassword);
    await page.locator('#auth-password-save').click();
    await expect(page.locator('#auth-password-status')).toHaveText(
      'Password changed, but other sessions could not be invalidated. 2 active sessions preserved.',
    );
    await expect(page.locator('#auth-password-status')).toHaveClass(/form-feedback-warning/);
    await expect(page.locator('#auth-session-status')).toHaveText('2 active sessions.');
    await expect(page.locator('#auth-current-password')).toHaveValue('');
    await expect(page.locator('#auth-new-password')).toHaveValue('');
    await expect(page.locator('#auth-confirm-password')).toHaveValue('');
    await expect.poll(() => page.evaluate(() => JSON.stringify({
      local: { ...localStorage },
      session: { ...sessionStorage },
    }))).not.toContain(changedPassword);
  });

  test('active session inventory keeps a temporary IP reveal controllable and undisclosed', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    state.sessions.push(
      { ...state.sessions[1], id: 'other-session-2' },
      { ...state.sessions[1], id: 'other-session-3' },
      { ...state.sessions[1], id: 'other-session-4' },
    );

    await page.goto('/admin');
    await expect(page.locator('#auth-current-session-list .session-item')).toHaveCount(1);
    await expect(page.locator('#auth-other-session-list .session-item')).toHaveCount(4);
    await expect(page.locator('#auth-other-session-list')).not.toHaveClass(/is-expanded/);
    await expect(page.locator('#auth-sessions-show-all')).toBeVisible();
    await page.locator('#auth-sessions-show-all').click();
    await expect(page.locator('#auth-other-session-list')).toHaveClass(/is-expanded/);
    await expect(page.locator('#auth-sessions-show-all')).toHaveText('Collapse');

    const reveal = page.locator('button[data-reveal-session-id="current-session"]');
    await reveal.focus();
    await reveal.click();
    await expect(page.locator('#session-ip-reveal-modal')).toBeVisible();
    await expect(page.locator('#session-ip-reveal-password')).toBeFocused();
    await page.keyboard.press('Shift+Tab');
    await expect(page.locator('#session-ip-reveal-submit')).toBeFocused();
    await page.keyboard.press('Escape');
    await expect(page.locator('#session-ip-reveal-modal')).toBeHidden();
    await expect(reveal).toBeFocused();

    await reveal.click();
    await page.locator('#session-ip-reveal-password').fill(password);
    await page.locator('#session-ip-reveal-submit').click();
    await expect.poll(() => state.sessionIPRevealPayload).toEqual({ current_password: password });
    await expect(page.locator('[data-session-ip-id="current-session"]')).toHaveText('192.168.1.44');
    await expect(page.locator('[data-session-ip-visibility="current-session"]')).toContainText('Full IP visible for 30 seconds');
    const hide = page.locator('button[data-hide-session-ip="current-session"]');
    await expect(hide).toHaveText('Hide now');
    await expect(hide).toBeFocused();
    await expect(page).not.toHaveURL(/192\.168\.1\.44/);
    await expect.poll(() => page.evaluate(() => JSON.stringify({
      local: { ...localStorage },
      session: { ...sessionStorage },
    }))).not.toContain('192.168.1.44');
    await expect.poll(() => page.locator('[aria-live]').allTextContents()).not.toContain('192.168.1.44');

    await hide.click();
    await expect(page.locator('[data-session-ip-id="current-session"]')).toHaveText('192.168.1.x');
    await expect(page.locator('button[data-reveal-session-id="current-session"]')).toHaveText('Reveal IP');
  });

  test('temporary session IP remasks on expiry, refresh, and document hiding', async ({ page }) => {
    const state = { sessionIPRevealSeconds: 1 };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto('/admin');

    const revealIP = async () => {
      await page.locator('button[data-reveal-session-id="current-session"]').click();
      await page.locator('#session-ip-reveal-password').fill(password);
      await page.locator('#session-ip-reveal-submit').click();
      await expect(page.locator('[data-session-ip-id="current-session"]')).toHaveText('192.168.1.44');
    };

    await revealIP();
    await expect(page.locator('[data-session-ip-id="current-session"]')).toHaveText('192.168.1.x', { timeout: 2500 });

    state.sessionIPRevealSeconds = 30;
    await revealIP();
    await page.evaluate(() => window.fetchAuthSessionStatus());
    await expect(page.locator('[data-session-ip-id="current-session"]')).toHaveText('192.168.1.x');

    await revealIP();
    await page.evaluate(() => {
      Object.defineProperty(document, 'hidden', { configurable: true, value: true });
      document.dispatchEvent(new Event('visibilitychange'));
    });
    await expect(page.locator('[data-session-ip-id="current-session"]')).toHaveText('192.168.1.x');
  });

  test('session IP reveal keeps authentication failures inside the secure dialog', async ({ page }) => {
    const state = { sessionIPRevealFailure: true };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto('/admin');

    await page.locator('button[data-reveal-session-id="current-session"]').click();
    await page.locator('#session-ip-reveal-password').fill('wrong password');
    await page.locator('#session-ip-reveal-submit').click();
    await expect(page.locator('#session-ip-reveal-error')).toHaveText('current password is invalid');
    await expect(page.locator('#session-ip-reveal-modal')).toBeVisible();
    await expect(page.locator('#session-ip-reveal-password')).toBeFocused();
    await expect(page.locator('[data-session-ip-id="current-session"]')).toHaveText('192.168.1.x');
    await expect(page.locator('body')).not.toContainText('192.168.1.44');
  });

  test('cancelling session IP reauthentication discards a late response', async ({ page }) => {
    const state = { sessionIPRevealDelayMs: 300 };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto('/admin');

    const reveal = page.locator('button[data-reveal-session-id="current-session"]');
    await reveal.click();
    await page.locator('#session-ip-reveal-password').fill(password);
    await page.locator('#session-ip-reveal-submit').click();
    await expect.poll(() => state.sessionIPRevealPayload).toEqual({ current_password: password });
    await page.locator('#session-ip-reveal-cancel').click();
    await expect(page.locator('#session-ip-reveal-modal')).toBeHidden();
    await expect(reveal).toBeFocused();
    await page.waitForTimeout(450);
    await expect(page.locator('[data-session-ip-id="current-session"]')).toHaveText('192.168.1.x');
    await expect(page.locator('body')).not.toContainText('192.168.1.44');
  });

  test('manage typed confirmations gate destructive host and audit actions', async ({ page, context }) => {
    const state = {};
    await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: 'http://127.0.0.1:8080' });
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto('/manage');
    await expect(page.locator('#manage-servers-table tbody')).toContainText('demo-host');
    await expect(page.locator('#global-key-status')).toHaveText('Configured');
    await expect(page.locator('#global-key-status')).toHaveClass(/is-configured/);
    await expect(page.locator('#upload-global-key-btn')).toHaveText('Replace Global SSH Credential');
    await expect(page.locator('#clear-global-key-btn')).toBeEnabled();
    await expect(page.locator('body')).not.toContainText('DO-NOT-RENDER-PRIVATE-KEY');
    await page.getByRole('link', { name: /Audit trail/ }).click();
    await expect(page.locator('#manage-section-audit-content')).toBeVisible();
    await expect(page.locator('#audit-table a[href="/api/reports/audit/55"]')).toBeVisible();
    await page.locator('#audit-table button[data-audit-detail="55"]').click();
    await expect(page.locator('#audit-detail-modal')).toContainText('Audit #55');
    await expect(page.locator('#audit-detail-modal')).toContainText('"scope": "security"');
    await expect(page.locator('#audit-detail-modal')).toContainText('req-55');
    await expect(page.locator('#audit-detail-report')).toHaveAttribute('href', '/api/reports/audit/55');
    await page.locator('#audit-detail-copy').click();
    await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe([
      'Audit #55',
      'Actor: admin',
      'Status: success',
      'Action: server.delete',
      'Target: server: demo-host',
      'Time: 2026-05-17 08:00:00 America/Toronto',
      'Client IP: 127.0.0.1',
      'Request ID: req-55',
      'Message: Deleted server',
      'Metadata:',
      '{',
      '  "scope": "security",',
      '  "count": 2',
      '}',
    ].join('\n'));
    await page.locator('#audit-detail-close').click();

    await page.locator('#audit-action-preset').selectOption('server.delete');
    await page.locator('#audit-from-filter').fill('2026-05-17T08:00');
    await page.locator('#audit-to-filter').fill('2026-05-17T13:00');
    await expect.poll(() => (state.auditListUrls || []).some(url => url.includes('action=server.delete') && url.includes('from=') && url.includes('to='))).toBe(true);

    await page.evaluate(() => {
      window.alert = () => {};
    });
    const deleteServerButton = page.locator('#manage-servers-table button[data-action="delete-server"][data-name="demo-host"]');
    const deleteServerMenu = deleteServerButton.locator('xpath=ancestor::details');
    const openDeleteServerMenu = async () => {
      if (!(await deleteServerMenu.evaluate(menu => menu.open))) {
        await deleteServerMenu.locator('summary').click();
      }
    };
    const auditPruneButton = page.locator('#audit-prune');
    const clearGlobalKeyButton = page.locator('#clear-global-key-btn');
    await openDeleteServerMenu();
    await dismissTypedConfirm(page, deleteServerButton);
    await dismissTypedConfirm(page, auditPruneButton);
    await page.getByRole('link', { name: /Global SSH Credential/ }).click();
    await expect(page.locator('#manage-section-global-key-content')).toBeVisible();
    await dismissTypedConfirm(page, clearGlobalKeyButton);
    await expect.poll(() => state.deleteServerCount || 0).toBe(0);
    await expect.poll(() => state.auditPruneCount || 0).toBe(0);
    await expect.poll(() => state.clearGlobalKeyCount || 0).toBe(0);

    await openDeleteServerMenu();
    await acceptTypedConfirm(page, deleteServerButton, 'demo-host');
    await acceptTypedConfirm(page, auditPruneButton, 'PRUNE');
    await expect.poll(() => state.deleteServerCount || 0).toBe(1);
    await expect.poll(() => state.auditPruneCount || 0).toBe(1);

    await acceptTypedConfirm(page, clearGlobalKeyButton, 'CLEAR GLOBAL SSH CREDENTIAL');
    await expect.poll(() => state.clearGlobalKeyCount || 0).toBe(1);
    await expect(page.locator('#global-key-status')).toHaveText('Not configured');
    await expect(page.locator('#global-key-status')).toHaveClass(/is-missing/);
    await expect(page.locator('#upload-global-key-btn')).toHaveText('Add Global SSH Credential');
    await expect(clearGlobalKeyButton).toBeDisabled();
  });

  test('manage policy override list follows live tag edits', async ({ page }) => {
    const state = {
      servers: [{ ...makeServer('demo-host'), tags: ['prod'] }],
    };
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto('/manage');
    await page.locator('#manage-servers-table button[data-action="edit-server"][data-name="demo-host"]').click();
    await expect(page.locator('#edit-trust-host-key')).toHaveCount(0);
    await expect(page.locator('#edit-known-host-state')).toHaveText('Trusted');
    await expect(page.locator('#edit-trust-known-host')).toBeHidden();
    await expect(page.locator('#edit-clear-known-host')).toBeEnabled();
    const overrides = page.locator('#edit-policy-overrides');
    await expect(overrides).toContainText('Disable "Prod security"');

    await page.locator('#edit-tags').fill('hold');
    await expect(overrides).toContainText('No tag-based scheduled policies currently match this server.');

    await page.locator('#edit-tags').fill('web');
    await expect(overrides).toContainText('Disable "Prod security"');

    state.policies = [{
      id: 10,
      name: 'Explicit server policy',
      target_tag: '',
      include_tags: [],
      exclude_tags: [],
      target_servers: ['demo-host'],
      matched_servers: ['Demo-Host'],
    }];
    state.servers = [{ ...makeServer('Demo-Host'), tags: ['misc'] }];

    await page.locator('#edit-cancel').click();
    await page.goto('/manage');
    await page.locator('#manage-servers-table button[data-action="edit-server"][data-name="Demo-Host"]').click();
    await expect(page.locator('#edit-policy-overrides')).toContainText('Disable "Explicit server policy"');
  });

  test('manage server editor locks background scrolling while its content remains scrollable', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto('/manage');
    await page.evaluate(() => window.scrollTo(0, 300));
    const editServerButton = page.locator('#manage-servers-table button[data-action="edit-server"][data-name="demo-host"]');
    await editServerButton.evaluate((element) => {
      element.addEventListener('click', () => {
        document.documentElement.dataset.testScrollBeforeModal = String(window.scrollY);
      }, { capture: true, once: true });
    });

    await editServerButton.click();
    await expect(page.locator('#edit-modal')).toHaveClass(/active/);
    await expect(page.locator('html')).toHaveClass(/manage-modal-open/);
    await expect(page.locator('body')).toHaveClass(/manage-modal-open/);
    await expect(page.locator('body')).toHaveCSS('position', 'fixed');
    const backgroundScrollTopBeforeOpen = Number.parseFloat(
      await page.locator('html').getAttribute('data-test-scroll-before-modal'),
    );
    const lockedBackgroundScrollTop = await page.evaluate(() => window.scrollY);

    const modalGrid = page.locator('#edit-modal .modal-grid');
    await expect(modalGrid).toHaveCSS('overflow-y', 'auto');
    const modalScroll = await modalGrid.evaluate((element) => {
      const before = element.scrollTop;
      element.scrollTop = before + 200;
      return { before, after: element.scrollTop };
    });
    expect(modalScroll.after).toBeGreaterThan(modalScroll.before);

    await page.mouse.move(5, 5);
    await page.mouse.wheel(0, 500);
    await page.waitForTimeout(100);
    await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(lockedBackgroundScrollTop);

    await page.locator('#edit-cancel').click();
    await expect(page.locator('html')).not.toHaveClass(/manage-modal-open/);
    await expect(page.locator('body')).not.toHaveClass(/manage-modal-open/);
    await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(backgroundScrollTopBeforeOpen);
  });

  test('successful immediate server-key upload accepts the editor credential intent', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto('/manage');
    await page.locator('#manage-servers-table button[data-action="edit-server"][data-name="demo-host"]').click();
    await page.locator('#edit-key').setInputFiles({
      name: 'id_ed25519',
      mimeType: 'application/octet-stream',
      buffer: Buffer.from('test-private-key'),
    });
    await expect(page.locator('#edit-draft-state')).toHaveText('Unsaved changes');
    await expect(page.locator('#edit-save')).toBeEnabled();

    await page.locator('#edit-upload-key').click();

    await expect.poll(() => state.uploadServerKeyCount || 0).toBe(1);
    await expect(page.locator('#edit-key-file-selection')).toHaveText('No file selected');
    await expect(page.locator('#edit-draft-state')).toHaveText('No unsaved changes');
    await expect(page.locator('#edit-save')).toBeDisabled();
  });

  test('failed replacement-key upload retains the accepted server edit and refreshes inventory', async ({ page }) => {
    const state = {
      failServerKeyUpload: true,
      servers: [makeServer('demo-host')],
    };
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto('/manage');
    await page.locator('#manage-servers-table button[data-action="edit-server"][data-name="demo-host"]').click();
    await page.locator('#edit-name').fill('renamed-host');
    await page.locator('#edit-host').fill('renamed-host.example.test');
    await page.locator('#edit-pass').fill('replacement-password');
    await page.locator('#edit-key').setInputFiles({
      name: 'invalid_ed25519',
      mimeType: 'application/octet-stream',
      buffer: Buffer.from('invalid-private-key'),
    });

    await page.locator('#edit-save').click();

    await expect.poll(() => state.serverUpdateCount || 0).toBe(1);
    await expect.poll(() => state.uploadServerKeyCount || 0).toBe(1);
    await expect.poll(() => state.inventoryLoadCount || 0).toBeGreaterThan(1);
    await expect(page.locator('#manage-servers-table tbody')).toContainText('renamed-host');
    await expect(page.locator('#edit-modal')).toHaveClass(/active/);
    await expect(page.locator('#edit-name')).toHaveValue('renamed-host');
    await expect(page.locator('#edit-pass')).toHaveValue('');
    await expect(page.locator('#edit-draft-state')).toHaveText('Unsaved changes');
    await expect(page.locator('#edit-save')).toBeEnabled();

    await page.locator('#edit-discard').click();
    await expect(page.locator('#edit-name')).toHaveValue('renamed-host');
    await expect(page.locator('#edit-draft-state')).toHaveText('No unsaved changes');
  });

  test('failed policy override save keeps the accepted server editor open and dirty', async ({ page }) => {
    const state = {
      failPolicyOverrideSave: true,
      servers: [{ ...makeServer('demo-host'), tags: ['prod'] }],
    };
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto('/manage');
    await page.locator('#manage-servers-table button[data-action="edit-server"][data-name="demo-host"]').click();
    const override = page.locator('#edit-policy-overrides input[data-policy-id="9"]');
    await expect(override).toBeVisible();
    await override.check();
    await expect(page.locator('#edit-draft-state')).toHaveText('Unsaved changes');

    await page.locator('#edit-save').click();

    await expect.poll(() => state.serverUpdateCount || 0).toBe(1);
    await expect.poll(() => state.policyOverrideSaveCount || 0).toBe(1);
    await expect(page.locator('#edit-modal')).toHaveClass(/active/);
    await expect(page.locator('#edit-draft-state')).toHaveText('Unsaved changes');
    await expect(page.locator('#edit-save')).toBeEnabled();
  });

  test('successful per-server-key creation clears the displayed filename', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto('/manage');
    await page.getByRole('link', { name: /Add Server Create an SSH target/ }).click();
    await page.locator('input[name="add-auth-method"][value="per-server-key"]').check();
    await page.locator('#name').fill('new-host');
    await page.locator('#host').fill('new-host.example.test');
    await page.locator('#user').fill('root');
    await page.locator('#trust-host-key').uncheck();
    await page.locator('#key_file').setInputFiles({
      name: 'id_ed25519',
      mimeType: 'application/octet-stream',
      buffer: Buffer.from('test-private-key'),
    });
    await expect(page.locator('#server-key-file-selection')).toHaveText('id_ed25519');

    await page.locator('#add-server-form').getByRole('button', { name: 'Add Server', exact: true }).click();

    await expect.poll(() => state.uploadServerKeyCount || 0).toBe(1);
    await expect(page.locator('#server-key-file-selection')).toHaveText('No file selected');
  });

  test('add-server validation failures render inline and focus the first invalid field', async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await stubManageApi(page);

    await page.goto('/manage');
    await page.getByRole('link', { name: /Add Server Create an SSH target/ }).click();
    const submit = page.locator('#add-server-form').getByRole('button', { name: 'Add Server', exact: true });
    await submit.click();

    await expect(page.locator('#add-server-error')).toContainText('name, host, user required');
    await expect(page.locator('#name')).toHaveAttribute('aria-invalid', 'true');
    await expect(page.locator('#host')).toHaveAttribute('aria-invalid', 'true');
    await expect(page.locator('#user')).toHaveAttribute('aria-invalid', 'true');
    await expect(page.locator('#name')).toBeFocused();

    await page.locator('#name').fill('new-host');
    await page.locator('#host').fill('new-host.example.test');
    await page.locator('#user').fill('root');
    await page.locator('#port').fill('70000');
    await submit.click();

    await expect(page.locator('#add-server-error')).toContainText('SSH port must be a whole number');
    await expect(page.locator('#port')).toHaveAttribute('aria-invalid', 'true');
    await expect(page.locator('#port')).toBeFocused();
  });

  test('manage known host controls expose trust, replace, and remove states', async ({ page }) => {
    const state = { hostKeyState: 'missing' };
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto('/manage');
    await page.locator('#manage-servers-table button[data-action="edit-server"][data-name="demo-host"]').click();
    await expect(page.locator('#edit-known-host-state')).toHaveText('Not trusted');
    await expect(page.locator('#edit-known-host-fingerprint')).toHaveText('SHA256:trusted');
    await expect(page.locator('#edit-trust-known-host')).toHaveText('Trust Host Key');
    await expect(page.locator('#edit-trust-known-host')).toBeVisible();
    await expect(page.locator('#edit-clear-known-host')).toBeDisabled();

    await page.locator('#edit-trust-known-host').click();
    await expect(page.locator('#hostkey-title')).toHaveText('Trust SSH Host Key');
    await page.locator('#hostkey-modal-trust').click();
    await expect.poll(() => state.trustHostKeyCount || 0).toBe(1);
    await expect(page.locator('#edit-known-host-state')).toHaveText('Trusted');
    await expect(page.locator('#edit-trust-known-host')).toBeHidden();
    await expect(page.locator('#edit-clear-known-host')).toBeEnabled();

    state.hostKeyState = 'changed';
    await page.locator('#edit-check-known-host').click();
    await expect(page.locator('#edit-known-host-state')).toHaveText('Key changed');
    await expect(page.locator('#edit-trust-known-host')).toHaveText('Replace Host Key');
    await page.locator('#edit-trust-known-host').click();
    await expect(page.locator('#hostkey-title')).toHaveText('Replace SSH Host Key');
    await page.locator('#hostkey-modal-trust').click();
    await expect.poll(() => state.clearKnownHostCount || 0).toBe(0);
    await expect.poll(() => state.trustHostKeyCount || 0).toBe(2);
    await expect(page.locator('#edit-known-host-state')).toHaveText('Trusted');

    await acceptTypedConfirm(page, page.locator('#edit-clear-known-host'), 'demo-host.example.test:22');
    await expect.poll(() => state.clearKnownHostCount || 0).toBe(1);
    await expect(page.locator('#edit-known-host-state')).toHaveText('Not trusted');
    await expect(page.locator('#edit-clear-known-host')).toBeDisabled();

    state.hostKeyState = 'error';
    await page.locator('#edit-check-known-host').click();
    await expect(page.locator('#edit-known-host-state')).toHaveText('Check failed');
    await expect(page.locator('#edit-clear-known-host')).toBeEnabled();
    await acceptTypedConfirm(page, page.locator('#edit-clear-known-host'), 'demo-host.example.test:22');
    await expect.poll(() => state.clearKnownHostCount || 0).toBe(2);
    await expect(page.locator('#edit-known-host-state')).toHaveText('Not trusted');
  });

  test('status metrics stay compact without secondary descriptions', async ({ page }) => {
    await ensureAuthenticatedSession(page);
    for (const viewport of [{ width: 1920, height: 1080 }, { width: 1216, height: 879 }]) {
      await page.setViewportSize(viewport);
      await page.goto('/');

      const metricState = await page.locator('.metric-strip').evaluate(element => ({
        height: element.getBoundingClientRect().height,
        timelineGap: Math.round(document.querySelector('.timeline-workspace').getBoundingClientRect().top
          - element.getBoundingClientRect().bottom),
        rowTops: [...element.querySelectorAll('.metric-item')]
          .filter(item => getComputedStyle(item).display !== 'none')
          .map(item => Math.round(item.getBoundingClientRect().top)),
        labelTops: [...element.querySelectorAll('.metric-item')]
          .filter(item => getComputedStyle(item).display !== 'none')
          .map(item => Math.round(item.querySelector('span').getBoundingClientRect().top)),
        numberTops: [...element.querySelectorAll('.metric-item')]
          .filter(item => getComputedStyle(item).display !== 'none')
          .map(item => Math.round(item.querySelector('strong').getBoundingClientRect().top)),
        visibleDescriptions: [...element.querySelectorAll('small')]
          .filter(item => getComputedStyle(item).display !== 'none')
          .map(item => item.textContent.trim()),
        visibleLegacyMetrics: [...element.querySelectorAll('.metric-hidden')]
          .filter(item => getComputedStyle(item).display !== 'none')
          .map(item => item.textContent.trim()),
      }));

      expect(metricState.visibleDescriptions).toEqual([]);
      expect(metricState.visibleLegacyMetrics).toEqual([]);
      expect(new Set(metricState.rowTops).size).toBe(1);
      expect(new Set(metricState.labelTops).size).toBe(1);
      expect(new Set(metricState.numberTops).size).toBe(1);
      expect(metricState.timelineGap).toBeLessThanOrEqual(24);
      expect(metricState.height).toBeLessThan(300);
    }
  });

  test('desktop bulk actions share one width and keep labels fully visible', async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await page.setViewportSize({ width: 1565, height: 875 });
    await page.goto('/');

    const labelLayout = await page.locator('.rail-bulk .bulk-actions').evaluate(element =>
      ['bulk-update', 'bulk-approve', 'bulk-approve-security', 'bulk-approve-kept-security', 'bulk-cancel', 'bulk-autoremove'].map(id => element.querySelector(`#${id}`)).map(button => ({
        id: button.id,
        clientWidth: button.clientWidth,
        scrollWidth: button.scrollWidth,
        clientHeight: button.clientHeight,
        scrollHeight: button.scrollHeight,
        backgroundColor: getComputedStyle(button).backgroundColor,
        borderColor: getComputedStyle(button).borderColor,
        opacity: getComputedStyle(button).opacity,
        textOverflow: getComputedStyle(button).textOverflow,
      })),
    );

    expect(labelLayout).not.toEqual([]);
    expect(new Set(labelLayout.map(button => button.clientWidth)).size, 'bulk actions must share one width').toBe(1);
    expect(new Set(labelLayout.map(button => button.clientHeight)).size, 'bulk actions must share one height').toBe(1);
    expect(new Set(labelLayout.map(button => button.backgroundColor)).size, 'disabled bulk actions must share one neutral background').toBe(1);
    expect(new Set(labelLayout.map(button => button.borderColor)).size, 'disabled bulk actions must share one neutral border').toBe(1);
    for (const button of labelLayout) {
      expect(button.scrollWidth, `${button.id} must not clip horizontally`).toBeLessThanOrEqual(button.clientWidth);
      expect(button.scrollHeight, `${button.id} must not clip vertically`).toBeLessThanOrEqual(button.clientHeight);
      expect(button.opacity, `${button.id} must keep its disabled label legible`).toBe('1');
      expect(button.textOverflow, `${button.id} must not use an ellipsis`).not.toBe('ellipsis');
    }
  });

  test('maintenance timeline uses one compact progress ring per server', async ({ page }) => {
    const servers = [
      makeServer('ring-host', 'updating', [], { tags: ['test'] }),
      makeServer('done-host', 'idle', [], { tags: ['test'], timeline_status: 'done' }),
    ];
    await stubDashboardApi(page, () => servers);
    await ensureAuthenticatedSession(page);
    await page.setViewportSize({ width: 1565, height: 875 });
    await page.goto('/');

    const row = page.locator('#servers-table tbody tr[data-name="ring-host"]');
    await expect(row.locator('.timeline-progress-ring')).toHaveCount(1);
    await expect(row.locator('.timeline-progress-ring')).toContainText('32%');
    await expect(row.locator('.timeline-progress-copy')).toContainText('Pre-checks');
    await expect(row.locator('.timeline-dot')).toHaveCount(0);
    await expect(page.locator('#servers-table thead')).toContainText('Maintenance');
    await expect(page.locator('#servers-table tbody tr[data-name="done-host"] .timeline-progress-copy')).toContainText('Last run: Done');
    await expect(page.locator('#servers-table tbody tr[data-name="done-host"] .timeline-progress-copy')).not.toContainText('Done / Error');
    const doneRingColor = await page.locator('#servers-table tbody tr[data-name="done-host"] .timeline-progress-ring').evaluate(element => getComputedStyle(element).color);
    expect(doneRingColor, 'completed maintenance must use the success color').toBe('rgb(137, 209, 133)');

    const badgeLayout = await page.evaluate(() => {
      const labels = [
        document.querySelector('#policy-summary-label'),
        document.querySelector('#failed-hosts-count'),
      ];
      labels[0].textContent = 'Policies 0';
      labels[1].textContent = '14';
      return labels.map(element => ({
        whiteSpace: getComputedStyle(element).whiteSpace,
        clientWidth: element.clientWidth,
        scrollWidth: element.scrollWidth,
        clientHeight: element.clientHeight,
        scrollHeight: element.scrollHeight,
      }));
    });
    for (const badge of badgeLayout) {
      expect(badge.whiteSpace, 'summary badges must keep their value on one line').toBe('nowrap');
      expect(badge.scrollWidth, 'summary badges must not clip horizontally').toBeLessThanOrEqual(badge.clientWidth);
      expect(badge.scrollHeight, 'summary badges must not stack value characters').toBeLessThanOrEqual(badge.clientHeight);
    }

    const busyActions = row.locator('.timeline-actions');
    await expect(busyActions.getByRole('button', { name: 'Updating…' })).toBeDisabled();
    await expect(busyActions.getByRole('button', { name: 'Logs' })).toBeEnabled();
    const busyActionLayout = await busyActions.evaluate(element => {
      const buttons = Array.from(element.querySelectorAll('button'));
      return {
        containerWidth: element.getBoundingClientRect().width,
        buttonWidths: buttons.map(button => button.getBoundingClientRect().width),
      };
    });
    expect(busyActionLayout.buttonWidths).toHaveLength(2);
    expect(Math.abs(busyActionLayout.buttonWidths[0] - busyActionLayout.buttonWidths[1])).toBeLessThanOrEqual(1);
    expect(busyActionLayout.buttonWidths[1], 'Logs must keep its compact grid width while updating').toBeLessThan(busyActionLayout.containerWidth / 2);
    const operationsNestedInTimeline = await page.locator('.operations-grid-secondary').evaluate(element => element.parentElement.classList.contains('timeline-column'));
    expect(operationsNestedInTimeline).toBe(true);

    const ringSize = await row.locator('.timeline-progress-ring').evaluate(element => ({
      width: element.getBoundingClientRect().width,
      height: element.getBoundingClientRect().height,
    }));
    expect(ringSize.width).toBeLessThanOrEqual(44);
    expect(ringSize.height).toBeLessThanOrEqual(44);

    const overflowSamples = await row.locator('.timeline-progress-ring').evaluate(async element => {
      const tableWrap = element.closest('.table-wrap');
      const samples = [];
      for (let frame = 0; frame < 12; frame += 1) {
        await new Promise(resolve => requestAnimationFrame(resolve));
        samples.push({
          clientHeight: tableWrap.clientHeight,
          scrollHeight: tableWrap.scrollHeight,
        });
      }
      return samples;
    });
    for (const sample of overflowSamples) {
      expect(sample.scrollHeight, 'the orbit must not create transient vertical overflow').toBe(sample.clientHeight);
    }

    await page.setViewportSize({ width: 1440, height: 900 });
    await page.reload();
    const responsiveTable = await page.locator('.timeline-workspace .table-wrap').evaluate(element => {
      const logs = element.querySelector('tbody tr[data-name="ring-host"] .timeline-actions button:last-child').getBoundingClientRect();
      const bounds = element.getBoundingClientRect();
      return { logsRight: logs.right, visibleRight: bounds.right };
    });
    expect(responsiveTable.logsRight, 'maintenance actions must be visible without horizontal scrolling at 1440px').toBeLessThanOrEqual(responsiveTable.visibleRight + 1);

    const triageHeaders = await page.locator('#approval-triage-table th').evaluateAll(headers => headers.map(header => ({
      clientWidth: header.clientWidth,
      scrollWidth: header.scrollWidth,
      clientHeight: header.clientHeight,
      scrollHeight: header.scrollHeight,
    })));
    for (const header of triageHeaders) {
      expect(header.scrollWidth, 'approval header must not overlap horizontally').toBeLessThanOrEqual(header.clientWidth);
      expect(header.scrollHeight, 'approval header must not clip vertically').toBeLessThanOrEqual(header.clientHeight);
    }

    await page.setViewportSize({ width: 390, height: 844 });
    await page.reload();
    const mobileHead = await page.locator('.dashboard-head').evaluate(element => {
      const description = element.querySelector('.muted').getBoundingClientRect();
      const bounds = element.getBoundingClientRect();
      return { descriptionWidth: description.width, headWidth: bounds.width };
    });
    expect(mobileHead.descriptionWidth, 'mobile dashboard description must use the available row').toBeGreaterThan(mobileHead.headWidth * 0.8);
  });

  test('pending approval actions keep cancel and complete labels visible', async ({ page }) => {
    const servers = [makeServer('approval-host', 'pending_approval', makePendingUpdates(15))];
    await stubDashboardApi(page, () => servers);
    await ensureAuthenticatedSession(page);
    await page.setViewportSize({ width: 1922, height: 1176 });
    await page.goto('/');

    const row = page.locator('#servers-table tbody tr[data-name="approval-host"]');
    await expect(row.locator('.timeline-progress-ring')).toContainText('60%');
    await expect(row.locator('button[data-action="cancel-upgrade"]')).toHaveText('Cancel');
    await expect(row.locator('button[data-action="approve-security"]')).toHaveText('Standard security (5)');
    const selectedActions = page.locator('#selected-host-panel .inspector-actions-primary');
    await expect(selectedActions.locator('button[data-action="cancel-upgrade"]')).toHaveText('Cancel');
    await expect(selectedActions.locator('button[data-action="approve-security"]')).toHaveText('Standard security (5)');

    const labelLayout = await page.locator([
      '#servers-table tbody tr[data-name="approval-host"] .timeline-actions button[data-action="approve-security"]',
      '#selected-host-panel .inspector-actions-primary button[data-action="approve-security"]',
      '#fleet-tag-list button[aria-label="Show hosts tagged untagged"] span',
    ].join(', ')).evaluateAll(elements => elements.map(element => ({
      clientWidth: element.clientWidth,
      scrollWidth: element.scrollWidth,
      clientHeight: element.clientHeight,
      scrollHeight: element.scrollHeight,
      textOverflow: getComputedStyle(element).textOverflow,
    })));
    expect(labelLayout).toHaveLength(3);
    for (const label of labelLayout) {
      expect(label.scrollWidth, 'action and tag labels must not clip horizontally').toBeLessThanOrEqual(label.clientWidth);
      expect(label.scrollHeight, 'action and tag labels must not clip vertically').toBeLessThanOrEqual(label.clientHeight);
      expect(label.textOverflow, 'action and tag labels must not use an ellipsis').not.toBe('ellipsis');
    }
  });

  test('operator pages share one responsive and accessible application shell', async ({ page }, testInfo) => {
    await ensureAuthenticatedSession(page);
    const pages = [
      ['/', '/', 'Status'],
      ['/manage', '/manage', 'Manage Servers'],
      ['/observability', '/observability', 'Observability'],
      ['/admin', '/admin', 'Admin'],
    ];
    const expectedLinks = [
      ['Status', '/'],
      ['Manage Servers', '/manage'],
      ['Observability', '/observability'],
      ['Admin', '/admin'],
    ];

    for (const viewport of [{ width: 1920, height: 1080 }, { width: 390, height: 844 }]) {
      await page.setViewportSize(viewport);
      let expectedHeaderHeight = 0;
      for (const [route, currentHref, pageLabel] of pages) {
        await page.goto(route);
        const pageFontFamily = await page.locator('body').evaluate(element => getComputedStyle(element).fontFamily);
        expect(pageFontFamily, `${route} must use the global Segoe UI font stack`).toContain('Segoe UI');
        const shell = page.locator('.app-header');
        await expect(shell).toHaveCount(1);
        await expect(shell).toHaveAttribute('aria-label', `${pageLabel} application shell`);
        const current = shell.locator('.app-nav a[aria-current="page"]');
        await expect(current).toHaveCount(1);
        await expect(current).toHaveAttribute('href', currentHref);

        const links = shell.locator('.app-nav a');
        await expect(links).toHaveCount(expectedLinks.length);
        for (let index = 0; index < expectedLinks.length; index += 1) {
          await expect(links.nth(index)).toHaveText(expectedLinks[index][0]);
          await expect(links.nth(index)).toHaveAttribute('href', expectedLinks[index][1]);
        }

        const box = await shell.boundingBox();
        expect(box).not.toBeNull();
        expect(box.x).toBeGreaterThanOrEqual(0);
        expect(box.x + box.width).toBeLessThanOrEqual(viewport.width + 1);
        if (expectedHeaderHeight === 0) expectedHeaderHeight = box.height;
        expect(Math.abs(box.height - expectedHeaderHeight)).toBeLessThanOrEqual(1);

        const hoverTarget = links.nth(currentHref === '/' ? 1 : 0);
        await hoverTarget.hover();
        await expect.poll(() => hoverTarget.evaluate(element => getComputedStyle(element).transform)).not.toBe('none');
        await expect.poll(() => hoverTarget.evaluate(element => getComputedStyle(element).boxShadow)).not.toBe('none');

        await page.mouse.move(0, 0);
        await page.keyboard.press('Tab');
        const focused = page.locator(':focus');
        await expect(focused).toBeVisible();
        const focusShadow = await focused.evaluate(element => getComputedStyle(element).boxShadow);
        expect(focusShadow).not.toBe('none');

        if (route === '/manage') {
          const screenshot = await page.screenshot({ fullPage: false });
          await testInfo.attach(`manage-${viewport.width}x${viewport.height}`, {
            body: screenshot,
            contentType: 'image/png',
          });
          if (process.env.UI_EVIDENCE_DIR) {
            fs.mkdirSync(process.env.UI_EVIDENCE_DIR, { recursive: true });
            fs.writeFileSync(
              path.join(process.env.UI_EVIDENCE_DIR, `manage-after-${viewport.width}x${viewport.height}.png`),
              screenshot,
            );
          }
        }
      }
    }

    const browser = page.context().browser();
    const noScriptContext = await browser.newContext({
      baseURL: 'http://127.0.0.1:8080',
      javaScriptEnabled: false,
    });
    await noScriptContext.addCookies(await page.context().cookies());
    const noScriptPage = await noScriptContext.newPage();
    await noScriptPage.goto('/observability');
    await noScriptPage.getByRole('link', { name: 'Manage Servers' }).click();
    await expect(noScriptPage).toHaveURL('http://127.0.0.1:8080/manage');
    await noScriptContext.close();
  });

  test('Manage Servers confines responsive overflow to data tables', async ({ page }) => {
    await ensureAuthenticatedSession(page);

    for (const viewport of [
      { width: 390, height: 844 },
      { width: 768, height: 1024 },
      { width: 1440, height: 900 },
    ]) {
      await page.setViewportSize(viewport);
      await page.goto('/manage');
      await expect(page.locator('#manage-section-nav')).toBeVisible();
      await expect(page.locator('#manage-section-directory-content')).toBeVisible();

      const layout = await page.evaluate(() => ({
        documentClientWidth: document.documentElement.clientWidth,
        documentScrollWidth: document.documentElement.scrollWidth,
        bodyScrollWidth: document.body.scrollWidth,
        sectionHeadDirections: Array.from(document.querySelectorAll('.manage-workspace-section .workspace-head'))
          .map(element => getComputedStyle(element).flexDirection),
        metrics: Array.from(document.querySelectorAll('.manage-summary .metric-item')).map(element => {
          const value = element.querySelector('strong');
          return {
            rowTop: Math.round(element.getBoundingClientRect().top),
            numberTop: Math.round(value.getBoundingClientRect().top),
            numberBackground: getComputedStyle(value).backgroundColor,
          };
        }),
        tables: Array.from(document.querySelectorAll('.manage-workspace-section .table-wrap')).map(element => ({
          clientWidth: element.clientWidth,
          scrollWidth: element.scrollWidth,
          overflowX: getComputedStyle(element).overflowX,
        })),
      }));

      expect(layout.documentScrollWidth, `${viewport.width}px document must not overflow`).toBeLessThanOrEqual(layout.documentClientWidth + 1);
      expect(layout.bodyScrollWidth, `${viewport.width}px body must not overflow`).toBeLessThanOrEqual(layout.documentClientWidth + 1);
      for (const table of layout.tables) {
        expect(['auto', 'scroll']).toContain(table.overflowX);
      }
      if (viewport.width <= 640) {
        expect(layout.sectionHeadDirections.every(direction => direction === 'column')).toBe(true);
      }
      const metricRows = new Map();
      for (const metric of layout.metrics) {
        if (!metricRows.has(metric.rowTop)) metricRows.set(metric.rowTop, new Set());
        metricRows.get(metric.rowTop).add(metric.numberTop);
      }
      expect([...metricRows.values()].every(numberTops => numberTops.size === 1)).toBe(true);
      expect(new Set(layout.metrics.map(metric => metric.numberBackground)).size).toBe(1);

      await expect(page.getByRole('link', { name: /Add Server Create an SSH target/ })).toBeVisible();
      await expect(page.getByRole('button', { name: 'Prev', exact: true })).toBeVisible();
      await expect(page.getByRole('button', { name: 'Next', exact: true })).toBeVisible();
    }
  });

  test('Manage Servers section navigation focuses the destination heading without an outline', async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await page.goto('/manage');

    await page.getByRole('link', { name: /Add Server Create an SSH target/ }).click();

    const heading = page.locator('#manage-section-add-server-heading');
    await expect(heading).toBeFocused();
    await expect(heading).toHaveCSS('outline-style', 'none');
  });

  test('Manage Servers dedicated Add Server action opens the creation workspace', async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await page.goto('/manage');

    const toggle = page.locator('[data-manage-section-toggle="add-server"]');
    const chevron = toggle.locator('.manage-section-toggle-icon');
    const chevronDirection = () => chevron.evaluate(element => {
      const matrix = new DOMMatrixReadOnly(getComputedStyle(element).transform);
      return Math.sign(matrix.b);
    });
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await expect.poll(chevronDirection).toBe(1);

    await page.getByRole('button', { name: 'Add Server', exact: true }).click();

    await expect(page.locator('#manage-section-add-server-content')).toBeVisible();
    await expect(page.locator('#manage-section-add-server-heading')).toBeFocused();
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect.poll(chevronDirection).toBe(-1);
  });

  test('Manage Servers summary distinguishes missing authentication from host trust issues', async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, {
      hasGlobalKey: false,
      servers: [
        makeServer('missing-auth', 'idle', [], { host_key_status: 'missing' }),
        makeServer('trusted-password', 'idle', [], { has_password: true, host_key_status: 'trusted' }),
        makeServer('untrusted-key', 'idle', [], { has_key: true, host_key_status: 'missing' }),
      ],
    });
    await page.goto('/manage');

    await expect(page.locator('#manage-summary-missing-auth')).toHaveText('1');
    await expect(page.locator('#manage-summary-host-trust')).toHaveText('2');
  });
});
