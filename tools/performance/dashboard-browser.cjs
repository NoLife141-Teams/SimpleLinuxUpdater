// Real Chromium against the test-only Go fixture; no HTTP responses are mocked.
// Usage: node tools/performance/dashboard-browser.cjs /tmp/slu-475-test /tmp/results
const { chromium } = require('@playwright/test');
const { spawn } = require('node:child_process');
const fs = require('node:fs/promises');
const path = require('node:path');
const assert = require('node:assert/strict');

const [binary, output] = process.argv.slice(2);
if (!binary || !output) throw new Error('Pass an absolute Go test binary and output directory');
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
async function ready(file, child) {
  for (let i = 0; i < 300; i++) {
    try { return JSON.parse(await fs.readFile(file, 'utf8')); } catch (error) { if (error.code !== 'ENOENT') throw error; }
    if (child.exitCode !== null) throw new Error(`Fixture exited ${child.exitCode}`);
    await sleep(100);
  }
  throw new Error('Fixture startup timeout');
}
const quantiles = values => {
  const sorted = [...values].sort((a, b) => a - b);
  return { n: sorted.length, p50_ms: sorted[Math.ceil(sorted.length * .5) - 1], p95_ms: sorted[Math.ceil(sorted.length * .95) - 1] };
};
async function settledPaint(page) { await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))); }
async function timed(page, run) { const at = performance.now(); await run(); await settledPaint(page); return performance.now() - at; }
const fleetPaths = ['/api/servers', '/api/dashboard/summary'];
const scopedStatusPath = '/api/status/changed-hosts';
const refreshPaths = new Set([...fleetPaths, scopedStatusPath, '/api/keys/global', '/api/audit-events', '/api/observability/summary', '/api/update-policies']);
const statusTimingPhases = ['wait', 'collect', 'project', 'compact', 'encode'];
function fleetResponses(page) {
  return fleetPaths.map(path => page.waitForResponse(response => new URL(response.url()).pathname === path));
}
async function fleetBytes(responses) {
  return Object.fromEntries(await Promise.all(responses.map(async (pending, index) => {
    const response = await pending;
    const [{ responseBodySize }, body] = await Promise.all([response.request().sizes(), response.body()]);
    assert.ok(responseBodySize > 0 && body.length > 0, `missing body size for ${fleetPaths[index]}`);
    return [fleetPaths[index], { encoded: responseBodySize, decoded: body.length }];
  })));
}

async function measureRequests(requests) {
  const paths = {};
  for (const request of requests) {
    const pathname = new URL(request.url()).pathname;
    const response = await request.response();
    assert.ok(response?.ok(), `refresh failed for ${pathname}`);
    const { responseBodySize } = await request.sizes();
    const item = paths[pathname] ??= { requests: 0, encoded: 0 };
    item.requests++;
    item.encoded += responseBodySize;
  }
  return paths;
}

async function measureHostRefresh(browser, fixture) {
  const results = {};
  const snapshotsIdle = () => Object.values(window.statusPageInteraction.getView().sync.streams).every(stream => !stream.inFlight);
  for (const tabCount of [1, 3]) {
    const trials = [];
    for (let iteration = -1; iteration < 5; iteration++) {
      const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce' });
      try {
        await context.addCookies([{ name: fixture.cookie.Name, value: fixture.cookie.Value, url: fixture.url }]);
        const pages = await Promise.all(Array.from({ length: tabCount }, () => context.newPage()));
        const errors = [];
        let measured = null;
        await Promise.all(pages.map(async page => {
          await page.addInitScript(scopedPath => {
            const fetch = window.fetch.bind(window);
            window.fetch = async (...args) => {
              const response = await fetch(...args);
              if (new URL(response.url).pathname === scopedPath) {
                const json = response.json.bind(response);
                response.json = async () => {
                  const payload = await json();
                  window.scalingStatusReceipt = {
                    parsedAt: performance.now(),
                    serverTiming: response.headers.get('Server-Timing'),
                  };
                  return payload;
                };
              }
              return response;
            };
          }, scopedStatusPath);
          page.on('pageerror', error => errors.push(String(error)));
          page.on('request', request => {
            if (measured && refreshPaths.has(new URL(request.url()).pathname)) measured.push(request);
          });
          await page.clock.setFixedTime(new Date(fixture.manifest.clock));
          const connected = page.waitForResponse(response => new URL(response.url()).pathname === '/api/dashboard/events');
          await page.goto(fixture.url, { waitUntil: 'domcontentloaded' });
          await connected;
          await page.locator('#servers-table tr[data-name="synthetic-0003"]').waitFor();
        }));
        const sample = {};
        for (const scenario of ['single', 'burst']) {
          await Promise.all(pages.map(page => page.waitForFunction(snapshotsIdle, null, { polling: 50 })));
          await Promise.all(pages.map(page => page.evaluate(() => { window.scalingStatusReceipt = null; })));
          measured = [];
          const at = performance.now();
          const change = await context.request.post(fixture.url + '/__scaling/hosts?burst=' + Number(scenario === 'burst'));
          assert.ok(change.ok());
          const states = await change.json();
          const rendered = await Promise.all(pages.map(async page => {
            const observed = await page.waitForFunction(changed => {
              if (!Object.entries(changed).every(([name, state]) =>
                document.querySelector(`#servers-table tr[data-name="${name}"] .status-pill`)?.textContent.trim().toLowerCase() === state
              )) return false;
              const receipt = window.scalingStatusReceipt;
              if (!receipt) return { latencyAfterResponseMs: null, serverTiming: null };
              return {
                latencyAfterResponseMs: performance.now() - receipt.parsedAt,
                serverTiming: receipt.serverTiming,
              };
            }, states, { polling: 20 });
            const latencyMs = performance.now() - at;
            const timing = await observed.jsonValue();
            await observed.dispose();
            assert.ok(Number.isFinite(timing.latencyAfterResponseMs) && timing.latencyAfterResponseMs >= 0,
              `${scenario} across ${tabCount} tab(s) rendered without a scoped response timing`);
            const serverMs = Object.fromEntries(
              Array.from(timing.serverTiming?.matchAll(/(\w+);dur=([\d.]+)/g) ?? [], ([, name, value]) => [name, Number(value)]),
            );
            for (const phase of statusTimingPhases) {
              assert.ok(Number.isFinite(serverMs[phase]), `scoped response missing Server-Timing phase ${phase}`);
            }
            return { latencyMs, afterResponseMs: timing.latencyAfterResponseMs, serverMs };
          }));
          await Promise.all(pages.map(page => page.waitForFunction(snapshotsIdle, null, { polling: 50 })));
          await sleep(200);
          const byPath = await measureRequests(measured);
          measured = null;
          assert.equal(byPath[scopedStatusPath]?.requests, tabCount, 'expected one scoped response per tab');
          for (const path of fleetPaths) {
            assert.equal(byPath[path], undefined, `successful scoped refresh reread ${path}`);
          }
          sample[scenario] = {
            latency_ms: rendered.map(sample => sample.latencyMs),
            after_response_ms: rendered.map(sample => sample.afterResponseMs),
            server_ms: rendered.map(sample => sample.serverMs),
            requests: Object.values(byPath).reduce((sum, item) => sum + item.requests, 0),
            encoded_bytes: Object.values(byPath).reduce((sum, item) => sum + item.encoded, 0),
            by_path: byPath,
          };
        }
        if (iteration === 4) {
          const reconnected = pages.map(page => Promise.all([
            page.waitForResponse(response => new URL(response.url()).pathname === '/api/dashboard/events', { timeout: 15000 }),
            ...fleetResponses(page),
          ]));
          assert.ok((await context.request.get(fixture.url + '/__scaling/disconnect')).ok());
          for (const responses of await Promise.all(reconnected)) {
            assert.ok(responses.every(response => response.ok()), 'reconnect failed to reopen SSE or refresh the full fleet');
          }
          sample.reconnected_tabs = pages.length;
        }
        if (iteration >= 0) trials.push(sample);
        assert.deepEqual(errors, []);
      } finally {
        await context.close();
      }
    }
    results[tabCount] = trials;
  }
  return results;
}

(async () => {
  await fs.mkdir(output, { recursive: true });
  const browser = await chromium.launch({ headless: true });
  try {
    const targeted = process.env.SLU_SCALING_TARGETED === '1';
    for (const size of targeted ? [100, 1000] : [10, 100, 1000]) {
      const fixtureDir = await fs.mkdtemp(path.join(output, `fixture-${size}-`));
      const log = await fs.open(path.join(output, `browser-server-${size}.log`), 'w');
      const child = spawn(path.resolve(binary), ['-test.run=^TestDashboardScalingServe$', '-test.timeout=5m'], {
        cwd: process.cwd(), env: { ...process.env, GOMAXPROCS: '2', SLU_SCALING_SERVE_DIR: fixtureDir, SLU_SCALING_SIZE: String(size) }, stdio: ['ignore', log.fd, log.fd],
      });
      const exited = new Promise(resolve => child.once('exit', (code, signal) => resolve({ code, signal })));
      let fixture;
      const samples = [], errors = [];
      try {
        fixture = await ready(path.join(fixtureDir, 'server.json'), child);
        if (targeted) {
          const samples = await measureHostRefresh(browser, fixture);
          await fs.writeFile(path.join(output, `host-refresh-${size}.json`), JSON.stringify({
            manifest: fixture.manifest, browser: browser.version(), samples,
          }, null, 2) + '\n');
          continue;
        }
        for (let iteration = -1; iteration < 5; iteration++) {
          const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce' });
          await context.addCookies([{ name: fixture.cookie.Name, value: fixture.cookie.Value, url: fixture.url }]);
          await context.tracing.start({ screenshots: true, snapshots: true, sources: true });
          await context.addInitScript(() => {
            window.scalingLongTasks = [];
            performance.setResourceTimingBufferSize(10000);
            new PerformanceObserver(list => window.scalingLongTasks.push(...list.getEntries().map(e => e.duration))).observe({ type: 'longtask', buffered: true });
          });
          const page = await context.newPage();
          await page.clock.setFixedTime(new Date(fixture.manifest.clock));
          const cdp = await context.newCDPSession(page);
          await cdp.send('Performance.enable');
          await cdp.send('Profiler.enable');
          await cdp.send('Profiler.setSamplingInterval', { interval: 1000 });
          await cdp.send('Profiler.start');
          page.on('pageerror', error => errors.push(String(error)));
          const requests = [], active = new Map(), maxActive = new Map();
          const refreshRequests = [];
          let reconnectPeak = 0, logRequestsInFlight = 0, measureReconnect = false;
          let sseConnections = 0;
          const jobLogs = path => path.startsWith('/api/jobs/') && path.endsWith('/logs');
          page.on('request', req => {
            const url = new URL(req.url());
            if (url.pathname === '/api/dashboard/events') sseConnections++;
            if (refreshPaths.has(url.pathname)) refreshRequests.push(req);
            if (jobLogs(url.pathname)) {
              logRequestsInFlight++;
              if (measureReconnect) reconnectPeak = Math.max(reconnectPeak, logRequestsInFlight);
            }
            if (url.pathname.startsWith('/api/') && !url.pathname.endsWith('/events')) {
              requests.push(url.pathname + url.search);
              const n = (active.get(url.pathname) || 0) + 1;
              active.set(url.pathname, n);
              maxActive.set(url.pathname, Math.max(maxActive.get(url.pathname) || 0, n));
            }
          });
          const finished = req => {
            const pathname = new URL(req.url()).pathname;
            if (jobLogs(pathname)) logRequestsInFlight--;
            if (active.has(pathname)) active.set(pathname, active.get(pathname) - 1);
          };
          page.on('requestfinished', finished); page.on('requestfailed', finished);
          page.on('response', res => { if (res.status() >= 400) errors.push(`HTTP ${res.status()} ${res.url()}`); });
          try {
            const initialResponses = fleetResponses(page);
            const initial = await timed(page, async () => {
              await page.goto(fixture.url, { waitUntil: 'domcontentloaded' });
              await page.locator('#metric-total-hosts').filter({ hasText: String(size) }).waitFor();
              await page.locator('#servers-table tbody tr[data-name]').first().waitFor();
            });
            await Promise.all(initialResponses.map(async response => (await response).finished()));
            const initialBytes = await fleetBytes(initialResponses);
            assert.equal(await page.locator('#metric-total-hosts').innerText(), String(size));
            const filter = await timed(page, async () => {
              await page.locator('#search').fill('synthetic-0001');
              await page.waitForFunction(() => document.querySelectorAll('#servers-table tbody tr[data-name]').length === 1);
            });
            const select = await timed(page, () => page.locator('.row-select[data-name="synthetic-0001"]').check());
            await page.locator('#search').fill('synthetic-0002');
            await page.waitForFunction(() => document.querySelector('#servers-table tbody tr[data-name]')?.dataset.name === 'synthetic-0002');
            assert.equal(await page.locator('#metric-total-hosts').innerText(), String(size));
            const hiddenSelectionHint = await page.locator('#bulk-action-hint').innerText();
            assert.match(hiddenSelectionHint, /1/);
            await page.locator('#search').fill('synthetic-0001');
            await page.waitForFunction(() => document.querySelector('.row-select[data-name="synthetic-0001"]')?.checked);
            const detail = await timed(page, async () => {
              await page.locator('#servers-table button[data-action="open-drawer"][data-name="synthetic-0001"][data-tab="pending"]').click();
              await page.locator('#status-drawer[aria-hidden="false"]').waitFor();
              await page.getByText('synthetic-pkg-00', { exact: true }).first().waitFor();
              await page.locator('#drawer-tab-logs').click();
              await page.waitForFunction(() => document.querySelector('#drawer-logs')?.textContent.includes('D'.repeat(100)));
            });
            await page.locator('#status-drawer-close').click();
            const changed = fleetResponses(page);
            const refresh = await timed(page, async () => {
              await context.request.get(fixture.url + '/__scaling/event');
              await Promise.all(changed.map(async response => (await response).finished()));
            });
            const refreshBytes = await fleetBytes(changed);
            await page.locator('#search').fill('synthetic-0003');
            const statusPill = page.locator('#servers-table tr[data-name="synthetic-0003"] .status-pill').first();
            const target = (await statusPill.innerText()).toLowerCase() === 'idle' ? 'done' : 'idle';
            const snapshotsIdle = () => Object.values(window.statusPageInteraction.getView().sync.streams).every(stream => !stream.inFlight);
            await page.waitForFunction(snapshotsIdle, null, { polling: 100 });
            const beforeBurst = refreshRequests.length;
            const burstRender = await timed(page, async () => {
              const emitted = context.request.get(fixture.url + '/__scaling/burst');
              await statusPill.filter({ hasText: target }).waitFor();
              assert.ok((await emitted).ok());
            });
            // Let the final event in the burst flush and include all its queued reads.
            await sleep(200);
            await page.waitForFunction(snapshotsIdle, null, { polling: 100 });
            const burstRequests = await measureRequests(refreshRequests.slice(beforeBurst));
            await page.locator('#search').fill('synthetic-0001');
            let reconnects = 0;
            const reconnectSamples = [];
            if (iteration === 4) {
              for (let cycle = 0; cycle < 3; cycle++) {
                const before = requests.length;
                reconnectPeak = 0;
                measureReconnect = true;
                const reopened = page.waitForResponse(res => new URL(res.url()).pathname === '/api/dashboard/events', { timeout: 15000 });
                await context.request.get(fixture.url + '/__scaling/disconnect');
                await reopened;
                // Wait until the log burst has drained before another cycle.
                await sleep(300);
                const expectedTargets = fixture.manifest.active + fixture.manifest.approvals;
                let reconnectRequests, targets;
                for (let attempts = 0; attempts < 600; attempts++) {
                  reconnectRequests = requests.slice(before);
                  targets = [...new Set(reconnectRequests.filter(url => url.includes('/logs?')).map(url => url.split('/logs?')[0]))];
                  if (logRequestsInFlight === 0 && targets.length >= expectedTargets) break;
                  await sleep(100);
                }
                assert.equal(logRequestsInFlight, 0, 'reconnect log requests timed out');
                assert.ok(targets.length >= expectedTargets, 'reconnect missed active or approval log targets');
                measureReconnect = false;
                reconnectSamples.push({ requests: reconnectRequests, targets, peak_log_requests: reconnectPeak });
                reconnects++;
              }
            }
            const metrics = Object.fromEntries((await cdp.send('Performance.getMetrics')).metrics.map(m => [m.name, m.value]));
            const browserFacts = await page.evaluate(() => ({ dom_nodes: document.getElementsByTagName('*').length, rows: document.querySelectorAll('#servers-table tbody tr[data-name]').length, long_tasks_ms: window.scalingLongTasks, resources: performance.getEntriesByType('resource').filter(r => r.name.includes('/api/')).map(r => ({ path: new URL(r.name).pathname, encoded_bytes: r.encodedBodySize, decoded_bytes: r.decodedBodySize, duration_ms: r.duration })) }));
            if (iteration >= 0) samples.push({ initial, filter, select, detail, refresh, burstRender, burstRequests, initial_bytes: initialBytes, refresh_bytes: refreshBytes, reconnects, sse_connections: sseConnections, reconnect_samples: reconnectSamples, requests, max_concurrent_by_path: Object.fromEntries(maxActive), hidden_selection_hint: hiddenSelectionHint, metrics, ...browserFacts });
          } catch (error) {
            await page.screenshot({ path: path.join(output, `failure-${size}.png`) });
            throw error;
          } finally {
            const { profile } = await cdp.send('Profiler.stop');
            if (iteration === 4) await fs.writeFile(path.join(output, `cpu-browser-${size}.json`), JSON.stringify(profile));
            await context.tracing.stop(iteration === 4 ? { path: path.join(output, `chromium-${size}.zip`) } : {});
            await context.close();
          }
        }
        const stats = Object.fromEntries(['initial', 'filter', 'select', 'detail', 'refresh', 'burstRender'].map(name => [name, quantiles(samples.map(s => s[name]))]));
        await fs.writeFile(path.join(output, `browser-${size}.json`), JSON.stringify({ manifest: fixture.manifest, browser: browser.version(), viewport: '1440x1000', iterations: 5, warmup: 1, stats, errors, samples }, null, 2) + '\n');
        assert.deepEqual(errors, []);
      } finally {
        if (fixture) await fetch(fixture.url + '/__scaling/stop').catch(() => {});
        const result = await Promise.race([exited, sleep(10000).then(() => null)]);
        if (!result) { child.kill('SIGKILL'); await exited; throw new Error('Fixture failed to stop'); }
        await log.close(); await fs.rm(fixtureDir, { recursive: true, force: true });
        if (result.code !== 0) throw new Error(`Fixture exit: ${JSON.stringify(result)}`);
      }
    }
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
