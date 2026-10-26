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

(async () => {
  await fs.mkdir(output, { recursive: true });
  const browser = await chromium.launch({ headless: true });
  try {
    for (const size of [10, 100, 1000]) {
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
          let sseConnections = 0;
          page.on('request', req => { const url = new URL(req.url()); if(url.pathname === '/api/dashboard/events') sseConnections++; if (url.pathname.startsWith('/api/') && !url.pathname.endsWith('/events')) { requests.push(url.pathname + url.search); const n = (active.get(url.pathname) || 0) + 1; active.set(url.pathname, n); maxActive.set(url.pathname, Math.max(maxActive.get(url.pathname) || 0, n)); } });
          const finished = req => { const pathname = new URL(req.url()).pathname; if (active.has(pathname)) active.set(pathname, active.get(pathname) - 1); };
          page.on('requestfinished', finished); page.on('requestfailed', finished);
          page.on('response', res => { if (res.status() >= 400) errors.push(`HTTP ${res.status()} ${res.url()}`); });
          try {
            const initial = await timed(page, async () => {
              await page.goto(fixture.url, { waitUntil: 'domcontentloaded' });
              await page.locator('#metric-total-hosts').filter({ hasText: String(size) }).waitFor();
              await page.locator('#servers-table tbody tr[data-name]').first().waitFor();
            });
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
              await page.locator('#drawer-tab-logs').click();
              await page.waitForFunction(() => document.querySelector('#drawer-logs')?.textContent.includes('D'.repeat(100)));
            });
            await page.locator('#status-drawer-close').click();
            const refresh = await timed(page, async () => {
              const changed = page.waitForResponse(res => new URL(res.url()).pathname === '/api/servers');
              await context.request.get(fixture.url + '/__scaling/event');
              await (await changed).finished();
            });
            let reconnects = 0;
            const reconnectSamples = [];
            if (iteration === 4) {
              for (let cycle = 0; cycle < 3; cycle++) {
                const before = requests.length;
                const reopened = page.waitForResponse(res => new URL(res.url()).pathname === '/api/dashboard/events', { timeout: 15000 });
                await context.request.get(fixture.url + '/__scaling/disconnect');
                await reopened;
                // Wait until the log burst has drained before another cycle.
                await sleep(300);
                for(let attempts=0; attempts<600 && [...active].some(([url,count]) => url.includes('/logs') && count>0); attempts++) await sleep(100);
                assert.equal([...active].some(([url,count]) => url.includes('/logs') && count>0), false, 'reconnect log requests timed out');
                reconnectSamples.push({ requests: requests.slice(before), targets: [...new Set(requests.slice(before).filter(url => url.includes('/logs?')).map(url => url.split('/logs?')[0]))] });
                reconnects++;
              }
            }
            const metrics = Object.fromEntries((await cdp.send('Performance.getMetrics')).metrics.map(m => [m.name, m.value]));
            const browserFacts = await page.evaluate(() => ({ dom_nodes: document.getElementsByTagName('*').length, rows: document.querySelectorAll('#servers-table tbody tr[data-name]').length, long_tasks_ms: window.scalingLongTasks, resources: performance.getEntriesByType('resource').filter(r => r.name.includes('/api/')).map(r => ({ path: new URL(r.name).pathname, encoded_bytes: r.encodedBodySize, decoded_bytes: r.decodedBodySize, duration_ms: r.duration })) }));
            if (iteration >= 0) samples.push({ initial, filter, select, detail, refresh, reconnects, sse_connections: sseConnections, reconnect_samples: reconnectSamples, requests, max_concurrent_by_path: Object.fromEntries(maxActive), hidden_selection_hint: hiddenSelectionHint, metrics, ...browserFacts });
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
        const stats = Object.fromEntries(['initial', 'filter', 'select', 'detail', 'refresh'].map(name => [name, quantiles(samples.map(s => s[name]))]));
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
