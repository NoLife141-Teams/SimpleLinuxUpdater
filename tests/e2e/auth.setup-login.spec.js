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
      ['pending_approval', 'Pending approval'],
      ['prechecks', 'Pre-checks'],
      ['apt_update', 'APT update'],
      ['upgrade', 'Upgrade'],
      ['postchecks', 'Post-checks'],
      ['done_error', 'Done / Error'],
    ];
    const statusMap = {
      pending_approval: ['pending_approval', 'waiting', 12],
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
    await page.route('**/api/app-settings/timezone', async route => {
      if (route.request().method() === 'PUT') {
        state.timezoneSave = await route.request().postDataJSON();
      }
      return fulfillJson(route, {
        timezone: 'America/Toronto',
        resolved_timezone: 'America/Toronto',
        editable_timezone: state.timezoneSave?.timezone || 'America/Toronto',
      });
    });
    await page.route('**/api/auth/sessions', async route => {
      if (route.request().method() === 'DELETE') {
        state.sessionClearCount = (state.sessionClearCount || 0) + 1;
        return fulfillJson(route, { deleted: 2 });
      }
      return fulfillJson(route, { session_count: 2 });
    });
    await page.route('**/api/auth/password', async route => {
      state.passwordPayload = await route.request().postDataJSON();
      return fulfillJson(route, { ok: true });
    });
    await page.route('**/api/notifications/settings', async route => {
      if (route.request().method() === 'PUT') {
        state.notificationPayload = await route.request().postDataJSON();
        return fulfillJson(route, {
          enabled: state.notificationPayload.enabled,
          webhook_url: state.notificationPayload.webhook_url,
          event_types: state.notificationPayload.event_types,
          supported_events: ['update.complete', 'schedule.run.failed', 'schedule.run.skipped', 'backup.restore'],
          last_delivery: null,
        });
      }
      return fulfillJson(route, {
        enabled: false,
        webhook_url: '',
        event_types: ['update.complete', 'schedule.run.failed', 'schedule.run.skipped', 'backup.restore'],
        supported_events: ['update.complete', 'schedule.run.failed', 'schedule.run.skipped', 'backup.restore'],
        last_delivery: null,
      });
    });
    await page.route('**/api/notifications/test', async route => {
      state.notificationTestCount = (state.notificationTestCount || 0) + 1;
      return fulfillJson(route, {
        last_delivery: {
          event_type: 'notification.test',
          action: 'notification.test',
          target_name: 'webhook',
          success: true,
          attempts: 1,
          status_code: 202,
          delivered_at: '2026-05-17T12:00:00Z',
        },
      });
    });
    await page.route('**/api/metrics/token', route => fulfillJson(route, { enabled: true, token: 'test-token' }));
    await page.route('**/api/backup/status', route => fulfillJson(route, {
      db_path: '/tmp/simplelinuxupdater.db',
      backup_supported: true,
      known_hosts_path: '/tmp/known_hosts',
    }));
    await page.route('**/api/backup/restore', async route => {
      state.restoreCount = (state.restoreCount || 0) + 1;
      return fulfillJson(route, { restored: true, sessions_invalidated: false });
    });
    await page.route('**/api/backup/verify', async route => {
      state.verifyCount = (state.verifyCount || 0) + 1;
      return fulfillJson(route, {
        valid: true,
        manifest_files: 3,
        known_hosts_included: false,
        created_at: '2026-05-17T06:00:00Z',
      });
    });
    await page.route('**/api/update-policies/settings', route => fulfillJson(route, {
      timezone: 'America/Toronto',
      resolved_timezone: 'America/Toronto',
      global_blackouts: [],
    }));
    await page.route('**/api/update-policies/runs?*', route => fulfillJson(route, {
      timezone: 'America/Toronto',
      items: [{
        id: 7,
        policy_name: 'Nightly security',
        server_name: 'srv-web-01',
        status: 'succeeded',
        summary: 'completed',
        job_id: 'job-report-1',
        scheduled_for_utc: '2026-05-17T06:00:00Z',
      }],
    }));
    await page.route('**/api/update-policies/calendar?*', route => fulfillJson(route, {
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
    }));
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
      return fulfillJson(route, {
        matched_servers: [
          { name: 'srv-web-01', tags: ['prod', 'web'] },
          { name: 'srv-web-02', tags: ['prod'] },
        ],
        excluded_servers: [
          { name: 'srv-db-01', tags: ['prod', 'db'], reason: 'excluded_tag' },
        ],
        disabled_by_override: [],
        warnings: ['Explicit server "srv-missing" is not in the current inventory.'],
      });
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
    await page.route('**/api/servers', route => fulfillJson(route, state.servers || [makeServer('demo-host')]));
    await page.route('**/api/servers/*', async route => {
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
    await expect(page.locator('#kpi-success-rate')).toHaveText('75.00%');
    await expect(page.locator('#error-banner')).toContainText('Health trends is unavailable (HTTP 503)');
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
    await expect(page.locator('#bulk-review-modal')).toBeVisible();
    await expect(page.locator('#bulk-review-modal')).toContainText('demo-host');
    await expect(page.locator('#bulk-review-modal')).not.toContainText('Server key');
    await expect(page.locator('#bulk-review-modal')).toContainText('standard security update');
    await expect(page.locator('#bulk-review-modal')).toContainText('runner-host');
    await expect(page.locator('#bulk-review-modal')).not.toContainText('Password');
    await expect(page.locator('#bulk-review-modal th')).toHaveText([
      'Server',
      'Planned action',
      'Server',
      'Why skipped',
    ]);
    await expect(page.locator('#bulk-review-modal')).toContainText('Not waiting for approval');
    await expect.poll(() => state.approveSecurity || 0).toBe(0);
    await page.locator('#bulk-review-confirm').click();
    await expect(page.locator('#typed-confirm-modal')).toBeVisible();
    await page.locator('#typed-confirm-input').fill('WRONG');
    await expect(page.locator('#typed-confirm-submit')).toBeDisabled();
    await page.locator('#typed-confirm-cancel').click();
    await expect.poll(() => state.approveSecurity || 0).toBe(0);
    await page.locator('#bulk-approve-security').click();
    await expect(page.locator('#bulk-review-modal')).toBeVisible();
    await page.locator('#bulk-review-confirm').click();
    await expect(page.locator('#typed-confirm-modal')).toBeVisible();
    await page.locator('#typed-confirm-input').fill('BULK APPROVE SECURITY');
    await page.locator('#typed-confirm-submit').click();
    await expect.poll(() => state.approveSecurity || 0).toBe(1);

    await page.locator('#approval-triage-table button[data-action="approve-all"][data-name="demo-host"]').click();
    await page.locator('#approval-triage-table button[data-action="approve-security"][data-name="demo-host"]').click();
    await page.locator('#approval-triage-table button[data-action="cancel-upgrade"][data-name="demo-host"]').click();
    await expect.poll(() => state.approveAll || 0).toBe(1);
    await expect.poll(() => state.approveSecurity || 0).toBe(2);
    await expect.poll(() => state.cancel || 0).toBe(1);

    await page.locator('#servers-table tbody button[data-action="open-drawer"][data-tab="pending"]').click();
    const pendingPanel = page.locator('#drawer-panel-pending');
    await expect(pendingPanel).toHaveClass(/active/);
    await expect(pendingPanel.locator('tbody tr')).toHaveCount(80);
    await expect.poll(() => pendingPanel.evaluate(el => el.scrollHeight - el.clientHeight)).toBeGreaterThan(0);

    await pendingPanel.evaluate(el => { el.scrollTop = 520; });
    const beforeRefresh = await pendingPanel.evaluate(el => el.scrollTop);
    expect(beforeRefresh).toBeGreaterThan(0);

    servers = [
      makeServer('demo-host', 'pending_approval', makePendingUpdates(80).map(update => ({ ...update, cve_state: 'ready' })), { tags: ['prod'] }),
      servers[1],
    ];
    await page.evaluate(() => window.fetchServers());

    await expect.poll(() => pendingPanel.evaluate(el => el.scrollTop)).toBeGreaterThanOrEqual(beforeRefresh - 1);
  });

  test('bulk action review gates typed confirmations, partial failures, and safe non-typed actions', async ({ page }) => {
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
    await expect(page.locator('#bulk-review-modal')).toContainText('ok-sec-host');
    await expect(page.locator('#bulk-review-modal')).toContainText('Hidden by current filter or page');
    await page.locator('#bulk-review-cancel').click();
    await expect.poll(() => state.updateIdle || 0).toBe(0);
    await page.locator('#auth-filter').selectOption('');
    await page.locator('#servers-table tbody tr[data-name="ok-sec-host"] .row-select').uncheck();

    await page.locator('#bulk-update').click();
    await page.locator('#bulk-review-confirm').click();
    await expect(page.locator('#typed-confirm-modal')).toBeVisible();
    await page.locator('#typed-confirm-input').fill('BULK UPDATE');
    await page.locator('#typed-confirm-submit').click();
    await expect.poll(() => state.updateIdle || 0).toBe(1);

    await page.locator('#servers-table tbody tr[data-name="idle-host"] .row-select').uncheck();
    await page.locator('#servers-table tbody tr[data-name="ok-sec-host"] .row-select').check();
    await page.locator('#bulk-approve').click();
    await expect(page.locator('#bulk-review-modal')).toBeVisible();
    await page.locator('#bulk-review-cancel').click();
    await expect(page.locator('#bulk-review-modal')).not.toBeVisible();
    await expect.poll(() => state.approveStandard || 0).toBe(0);
    await page.locator('#bulk-approve').click();
    await page.locator('#bulk-review-confirm').click();
    await expect(page.locator('#typed-confirm-modal')).toBeVisible();
    await page.locator('#typed-confirm-input').fill('WRONG');
    await expect(page.locator('#typed-confirm-submit')).toBeDisabled();
    await page.locator('#typed-confirm-cancel').click();
    await expect.poll(() => state.approveStandard || 0).toBe(0);

    await page.locator('#bulk-approve-kept-security').click();
    await expect(page.locator('#bulk-review-modal')).toContainText('Package removals will be confirmed');
    await page.locator('#bulk-review-confirm').click();
    await expect(page.locator('#typed-confirm-modal')).toBeVisible();
    await page.locator('#typed-confirm-input').fill('BULK APPROVE KEPT SECURITY');
    await page.locator('#typed-confirm-submit').click();
    await expect.poll(() => state.approveKept || 0).toBe(1);

    await page.locator('#servers-table tbody tr[data-name="fail-sec-host"] .row-select').check();
    await page.locator('#servers-table tbody tr[data-name="no-sec-host"] .row-select').check();
    await page.locator('#bulk-approve-security').click();
    await expect(page.locator('#bulk-review-modal')).toContainText('ok-sec-host');
    await expect(page.locator('#bulk-review-modal')).toContainText('fail-sec-host');
    await expect(page.locator('#bulk-review-modal')).toContainText('no-sec-host');
    await expect(page.locator('#bulk-review-modal')).toContainText('No standard security updates eligible');
    await page.locator('#bulk-review-confirm').click();
    await page.locator('#typed-confirm-input').fill('BULK APPROVE SECURITY');
    await page.locator('#typed-confirm-submit').click();
    await expect.poll(() => state.approveSecurityOk || 0).toBe(1);
    await expect.poll(() => state.approveSecurityFail || 0).toBe(1);
    await expect.poll(() => state.approveSecuritySkipped || 0).toBe(0);
    const failureNotice = page.locator('#app-feedback-region[role="alert"]');
    await expect(failureNotice).toContainText('fail-sec-host: backend down');
    await expect(failureNotice).not.toContainText('no-sec-host: backend down');

    await page.locator('#servers-table tbody tr[data-name="fail-sec-host"] .row-select').uncheck();
    await page.locator('#servers-table tbody tr[data-name="no-sec-host"] .row-select').uncheck();
    await page.locator('#bulk-cancel').click();
    await page.locator('#bulk-review-confirm').click();
    await expect(page.locator('#typed-confirm-modal')).not.toBeVisible();
    await expect.poll(() => state.cancelOk || 0).toBe(1);

    await page.locator('#servers-table tbody tr[data-name="ok-sec-host"] .row-select').uncheck();
    await page.locator('#servers-table tbody tr[data-name="idle-host"] .row-select').check();
    await page.locator('#bulk-autoremove').click();
    await page.locator('#bulk-review-confirm').click();
    await expect(page.locator('#typed-confirm-modal')).not.toBeVisible();
    await expect.poll(() => state.autoremoveIdle || 0).toBe(1);

    await page.locator('#refresh-all-facts').click();
    await expect(page.locator('#bulk-review-modal')).toBeVisible();
    await page.locator('#bulk-review-confirm').click();
    await expect(page.locator('#typed-confirm-modal')).not.toBeVisible();
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
    await expect(page.locator('#maintenance-calendar-list')).toContainText('Nightly security');
    await expect(page.locator('#maintenance-calendar-list')).toContainText('Allowed 02:00');
    await expect(page.locator('#maintenance-calendar-list')).toContainText('global 23:00-03:00 overnight');
    await expect(page.locator('#scheduled-runs-table a[href="/api/reports/jobs/job-report-1"]')).toBeVisible();
    await page.locator('#scheduled-runs-table button[data-action="job-detail"][data-job-id="job-report-1"]').click();
    await expect(page.locator('#job-detail-modal')).toContainText('Job job-report-1');
    await expect(page.locator('#job-detail-modal')).toContainText('Complete');
    await expect(page.locator('#job-detail-modal')).toContainText('"max_attempts": 3');
    await expect(page.locator('#job-detail-modal')).toContainText('upgrade completed');
    await expect(page.locator('#job-detail-copy-logs')).toBeVisible();
    await expect(page.locator('#job-detail-report')).toHaveAttribute('href', '/api/reports/jobs/job-report-1');
    await page.locator('#job-detail-close').click();

    await page.locator('#notification-enabled').check();
    await page.locator('#notification-webhook-url').fill('https://hooks.example.test/simplelinuxupdater');
    await page.locator('#notification-event-schedule-skipped').uncheck();
    await page.locator('#notification-save').click();
    await expect.poll(() => state.notificationPayload).toMatchObject({
      enabled: true,
      webhook_url: 'https://hooks.example.test/simplelinuxupdater',
      event_types: ['update.complete', 'schedule.run.failed', 'backup.restore'],
    });
    await expect(page.locator('#notification-status')).toContainText('Notification settings saved');
    await page.locator('#notification-test').click();
    await expect.poll(() => state.notificationTestCount || 0).toBe(1);
    await expect(page.locator('#notification-last-delivery')).toContainText('notification.test');

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

  test('admin typed confirmations gate restore and policy deletion', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);

    await page.goto('/admin');
    await page.locator('#backup-restore-file').setInputFiles({
      name: 'backup.slubkp',
      mimeType: 'application/octet-stream',
      buffer: Buffer.from('fake-backup'),
    });
    await page.locator('#backup-restore-passphrase').fill('LongPassphrase123');
    await dismissTypedConfirm(page, page.locator('#backup-restore-btn'));
    await expect.poll(() => state.restoreCount || 0).toBe(0);

    await page.evaluate(() => { window.alert = () => {}; });
    await page.locator('#backup-restore-file').setInputFiles({
      name: 'backup.slubkp',
      mimeType: 'application/octet-stream',
      buffer: Buffer.from('fake-backup'),
    });
    await page.locator('#backup-restore-passphrase').fill('LongPassphrase123');
    await page.locator('#backup-verify-btn').click();
    await expect.poll(() => state.verifyCount || 0).toBe(1);
    await expect(page.locator('#backup-status')).toContainText('Backup verified: 3 manifest file(s)');
    await acceptTypedConfirm(page, page.locator('#backup-restore-btn'), 'RESTORE');
    await expect.poll(() => state.restoreCount || 0).toBe(1);

    const deletePolicyButton = page.locator('#scheduled-policy-table button[data-action="delete-policy"][data-id="12"]');
    await dismissTypedConfirm(page, deletePolicyButton);
    await expect.poll(() => state.policyDeleteCount || 0).toBe(0);

    await acceptTypedConfirm(page, deletePolicyButton, 'Nightly security');
    await expect.poll(() => state.policyDeleteCount || 0).toBe(1);
  });

  test('admin password change sends payload and session clear requires typed confirmation', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);

    await page.goto('/admin');
    await expect(page.locator('#auth-session-status')).toContainText('2 server-side session');
    await page.locator('#auth-current-password').fill(password);
    await page.locator('#auth-new-password').fill(changedPassword);
    await page.locator('#auth-confirm-password').fill(changedPassword);
    await page.locator('#auth-password-save').click();
    await expect.poll(() => state.passwordPayload).toEqual({
      current_password: password,
      new_password: changedPassword,
      confirm_password: changedPassword,
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

  test('manage typed confirmations gate destructive host and audit actions', async ({ page, context }) => {
    const state = {};
    await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: 'http://127.0.0.1:8080' });
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto('/manage');
    await expect(page.locator('#manage-servers-table tbody')).toContainText('demo-host');
    await expect(page.locator('#global-key-status')).toHaveText('Configured');
    await expect(page.locator('#global-key-status')).toHaveClass(/is-configured/);
    await expect(page.locator('#upload-global-key-btn')).toHaveText('Replace Global Key');
    await expect(page.locator('#clear-global-key-btn')).toBeEnabled();
    await expect(page.locator('body')).not.toContainText('DO-NOT-RENDER-PRIVATE-KEY');
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
    const auditPruneButton = page.locator('#audit-prune');
    const clearGlobalKeyButton = page.locator('#clear-global-key-btn');
    await dismissTypedConfirm(page, deleteServerButton);
    await dismissTypedConfirm(page, auditPruneButton);
    await dismissTypedConfirm(page, clearGlobalKeyButton);
    await expect.poll(() => state.deleteServerCount || 0).toBe(0);
    await expect.poll(() => state.auditPruneCount || 0).toBe(0);
    await expect.poll(() => state.clearGlobalKeyCount || 0).toBe(0);

    await acceptTypedConfirm(page, deleteServerButton, 'demo-host');
    await acceptTypedConfirm(page, auditPruneButton, 'PRUNE');
    await expect.poll(() => state.deleteServerCount || 0).toBe(1);
    await expect.poll(() => state.auditPruneCount || 0).toBe(1);

    await acceptTypedConfirm(page, clearGlobalKeyButton, 'CLEAR GLOBAL KEY');
    await expect.poll(() => state.clearGlobalKeyCount || 0).toBe(1);
    await expect(page.locator('#global-key-status')).toHaveText('Not configured');
    await expect(page.locator('#global-key-status')).toHaveClass(/is-missing/);
    await expect(page.locator('#upload-global-key-btn')).toHaveText('Add Global Key');
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
});
