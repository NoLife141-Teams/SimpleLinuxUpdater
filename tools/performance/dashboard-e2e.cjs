// Own the empty test installation explicitly. Playwright's webServer process
// group cleanup must not obscure the browser exit status or leave SSE alive.
const { spawn } = require('node:child_process');
const fs = require('node:fs/promises');
const path = require('node:path');
const [binaryArg, outputArg] = process.argv.slice(2);
if (!binaryArg || !outputArg) throw new Error('Pass a Go test binary and output directory');
const binary = path.resolve(binaryArg), output = path.resolve(outputArg);
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const quote = value => `'${value.replaceAll("'", "'\\''")}'`;
(async () => {
  await fs.mkdir(output, { recursive: true });
  const temporary = await fs.mkdtemp('/tmp/slu-dashboard-e2e-');
  const ready = path.join(temporary, 'ready.json');
  const wrapper = path.join(temporary, 'identity-server');
  const config = path.join(temporary, 'playwright.config.cjs');
  await fs.writeFile(wrapper, `#!/usr/bin/env bash\nset -euo pipefail\nexport SLU_SCALING_E2E_LISTEN="\${DEBIAN_UPDATER_LISTEN_ADDR:?}"\nunset SLU_SCALING_E2E_READY\nexec ${quote(binary)} -test.run='^TestDashboardScalingE2EServe$' -test.timeout=15m\n`, { mode: 0o700 });
  await fs.writeFile(config, `const config = require(${JSON.stringify(path.resolve('playwright.config.js'))});\nconfig.webServer = undefined;\nconfig.testDir = ${JSON.stringify(path.resolve('tests/e2e'))};\nconfig.outputDir = ${JSON.stringify(path.join(output, 'test-results'))};\nmodule.exports = config;\n`);
  const log = await fs.open(path.join(output, 'server.log'), 'w');
  const server = spawn(binary, ['-test.run=^TestDashboardScalingE2EServe$', '-test.timeout=15m'], { env: { ...process.env, GOMAXPROCS: '2', SLU_SCALING_E2E_LISTEN: '127.0.0.1:8080', SLU_SCALING_E2E_READY: ready }, stdio: ['ignore', log.fd, log.fd] });
  const serverExit = new Promise(resolve => server.once('exit', (code, signal) => resolve({ code, signal })));
  let browserExit;
  try {
    let ownsListener = false;
    for (let attempt = 0; attempt < 300; attempt++) {
      try { const fact = JSON.parse(await fs.readFile(ready, 'utf8')); ownsListener = fact.pid === server.pid && fact.listen === '127.0.0.1:8080'; } catch (error) { if (error.code !== 'ENOENT') throw error; }
      if (ownsListener) break;
      if (server.exitCode !== null) throw new Error(`Test server startup failed ${server.exitCode}`);
      await sleep(100);
    }
    if (!ownsListener) throw new Error('Test server did not establish its owned listener');
    const child = spawn(process.execPath, [path.resolve('node_modules/@playwright/test/cli.js'), 'test', '--config', config, '--workers=1'], { env: { ...process.env, SLU_IDENTITY_TEST_BINARY: wrapper }, stdio: 'inherit' });
    browserExit = await new Promise(resolve => child.once('exit', (code, signal) => resolve({ code, signal })));
  } finally {
    server.kill('SIGTERM');
    const timeout = setTimeout(() => server.kill('SIGKILL'), 10000);
    const serverResult = await serverExit; clearTimeout(timeout);
    await log.close(); await fs.rm(temporary, { recursive: true, force: true });
    await fs.writeFile(path.join(output, 'lifecycle.json'), JSON.stringify({ playwright: browserExit, server: serverResult }, null, 2) + '\n');
    if (browserExit?.code !== 0 || serverResult.code !== 0) throw new Error(`Validation failed: ${JSON.stringify({ browserExit, serverResult })}`);
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
