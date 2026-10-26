// Capture presentation smoke evidence without changing the timed benchmark.
const { chromium } = require('@playwright/test');
const { spawn } = require('node:child_process');
const fs = require('node:fs/promises');
const path = require('node:path');
const assert = require('node:assert/strict');
const [binary, output] = process.argv.slice(2);
if (!binary || !output) throw new Error('Pass the Go test binary and output directory');
(async () => {
  await fs.mkdir(output, { recursive: true });
  const dir = await fs.mkdtemp('/tmp/slu-dashboard-screenshots-');
  const log = await fs.open(path.join(dir, 'server.log'), 'w');
  const child = spawn(path.resolve(binary), ['-test.run=^TestDashboardScalingServe$', '-test.timeout=2m'], { cwd: process.cwd(), env: { ...process.env, GOMAXPROCS: '2', SLU_SCALING_SERVE_DIR: dir, SLU_SCALING_SIZE: '100' }, stdio: ['ignore', log.fd, log.fd] });
  const exited = new Promise(resolve => child.once('exit', code => resolve(code)));
  let fixture, browser;
  try {
    for (let attempt = 0; attempt < 300 && !fixture; attempt++) {
      try { fixture = JSON.parse(await fs.readFile(path.join(dir, 'server.json'), 'utf8')); } catch (error) { if (error.code !== 'ENOENT') throw error; }
      if (child.exitCode !== null) throw new Error(`Fixture exited ${child.exitCode}`);
      if (!fixture) await new Promise(resolve => setTimeout(resolve, 100));
    }
    if (!fixture) throw new Error('Fixture startup timeout');
    browser = await chromium.launch({ headless: true });
    for (const [name, viewport] of [['desktop', { width: 1440, height: 1000 }], ['mobile', { width: 390, height: 844 }]]) {
      const context = await browser.newContext({ viewport, reducedMotion: 'reduce' });
      try {
        await context.addCookies([{ name: fixture.cookie.Name, value: fixture.cookie.Value, url: fixture.url }]);
        const page = await context.newPage();
        await page.clock.setFixedTime(new Date(fixture.manifest.clock));
        await page.goto(fixture.url);
        await page.waitForFunction(() => document.querySelector('#metric-total-hosts')?.textContent === '100' && document.querySelector('#servers-table tbody tr[data-name]'));
        await page.locator('#search').fill('synthetic-0001');
        await page.waitForFunction(() => document.querySelectorAll('#servers-table tbody tr[data-name]').length === 1);
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, `${name} horizontal overflow`);
        await page.screenshot({ path: path.join(output, `${name}.png`), fullPage: true });
      } finally { await context.close(); }
    }
  } finally {
    if (browser) await browser.close();
    if (fixture) await fetch(fixture.url + '/__scaling/stop').catch(() => {}); else child.kill('SIGTERM');
    const timer = setTimeout(() => child.kill('SIGKILL'), 10000);
    const code = await exited; clearTimeout(timer);
    await log.close(); await fs.rm(dir, { recursive: true, force: true });
    if (code !== 0) throw new Error(`Fixture exited ${code}`);
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
