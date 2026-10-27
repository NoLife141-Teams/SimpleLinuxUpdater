const { defineConfig } = require("@playwright/test");
const os = require("node:os");
const path = require("node:path");
module.exports = defineConfig({
    reporter: process.env.CI
        ? [
              ["list"],
              [
                  "html",
                  {
                      open: "never",
                      outputFolder:
                          process.env.PLAYWRIGHT_HTML_OUTPUT_DIR ||
                          path.join(os.tmpdir(), "slu-browser-report"),
                  },
              ],
          ]
        : "list",
    testDir: "./tests/e2e",
    globalSetup: require.resolve("./tests/e2e/support/global-setup"),
    outputDir:
        process.env.SLU_E2E_OUTPUT_DIR ||
        path.join(os.tmpdir(), "slu-browser-results"),
    timeout: 60000,
    expect: { timeout: 10000 },
    fullyParallel: true,
    workers: process.env.CI ? 2 : undefined,
    retries: process.env.CI ? 1 : 0,
    use: { headless: true, reducedMotion: "reduce", trace: "on-first-retry" },
});
