const { test, expect } = require('@playwright/test');

test.describe.serial('setup and login flows', () => {
  const username = 'admin';
  const password = 'StrongPass1234';
  const changedPassword = 'NewStrongPass123';

  async function signIn(page) {
    await page.locator('#username').fill(username);
    await page.locator('#password').fill(password);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page).toHaveURL('http://127.0.0.1:8080/');
    await expect(page.locator('#logout-btn')).toBeVisible();
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
    await page.goto('/login');

    const status = await page.evaluate(async () => {
      const response = await fetch('/api/auth/status', { cache: 'no-store' });
      return response.json();
    });

    if (!status.authenticated) {
      const endpoint = status.setup_required ? '/api/auth/setup' : '/api/auth/login';
      const candidates = [password, changedPassword];
      let result = { ok: false, status: 0, payload: {} };
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
        if (result.ok) break;
      }

      expect(result, `${endpoint} should create an authenticated test session`).toMatchObject({ ok: true });
    }

    await page.goto('/');
    await expect(page.locator('#logout-btn')).toBeVisible();
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
        timeline: makeTimeline(server.status),
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
          can_approve_all: server.status === 'pending_approval',
          can_approve_security: server.status === 'pending_approval' && securityUpdates > 0,
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

  async function stubDashboardApi(page, getServers) {
    await page.route('**/api/servers', route => fulfillJson(route, getServers()));
    await page.route('**/api/keys/global', route => fulfillJson(route, { has_key: false }));
    await page.route('**/api/audit-events*', route => fulfillJson(route, { items: [] }));
    await page.route('**/api/observability/summary*', route => fulfillJson(route, { totals: { updates_total: 0, success_rate_pct: 0 } }));
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
        return fulfillJson(route, { ok: true });
      }
      return fulfillJson(route, { has_key: true });
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

  test('pending updates drawer keeps scroll position after server refresh', async ({ page }) => {
    let servers = [
      makeServer('demo-host', 'pending_approval', makePendingUpdates(80), { tags: ['prod'] }),
      makeServer('runner-host', 'updating', [], {
        tags: ['web'],
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

    await page.locator('#select-all').check();
    await page.locator('#bulk-approve-security').click();
    await expect(page.locator('#bulk-review-modal')).toBeVisible();
    await expect(page.locator('#bulk-review-modal')).toContainText('demo-host');
    await expect(page.locator('#bulk-review-modal')).toContainText('runner-host: No standard security updates eligible');
    await expect.poll(() => state.approveSecurity || 0).toBe(0);
    await page.locator('#bulk-review-confirm').click();
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

  test('manage typed confirmations gate destructive host and audit actions', async ({ page }) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto('/manage');
    await expect(page.locator('#manage-servers-table tbody')).toContainText('demo-host');
    await expect(page.locator('#audit-table a[href="/api/reports/audit/55"]')).toBeVisible();
    await page.locator('#audit-table button[data-audit-detail="55"]').click();
    await expect(page.locator('#audit-detail-modal')).toContainText('Audit #55');
    await expect(page.locator('#audit-detail-modal')).toContainText('"scope": "security"');
    await expect(page.locator('#audit-detail-modal')).toContainText('req-55');
    await expect(page.locator('#audit-detail-report')).toHaveAttribute('href', '/api/reports/audit/55');
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
  });

  test('manage policy override list follows live tag edits', async ({ page }) => {
    const state = {
      servers: [{ ...makeServer('demo-host'), tags: ['prod'] }],
    };
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto('/manage');
    await page.locator('#manage-servers-table button[data-action="edit-server"][data-name="demo-host"]').click();
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
});
