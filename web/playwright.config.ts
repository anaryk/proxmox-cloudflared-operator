import { defineConfig, devices } from '@playwright/test'

// The browser tests of the interface; one Chromium project for now. The
// performance page (e2e/perf) is built for production and served by Vite.
const port = 4174

export default defineConfig({
  testDir: 'e2e',
  outputDir: 'test-results/playwright',
  workers: 1,
  timeout: 180_000,
  reporter: 'list',
  use: { baseURL: `http://127.0.0.1:${port}` },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } } }],
  webServer: {
    command:
      'node_modules/.bin/vite build --config e2e/perf/vite.config.ts --logLevel warn && ' +
      `node_modules/.bin/vite preview --config e2e/perf/vite.config.ts --host 127.0.0.1 --port ${port} --strictPort`,
    url: `http://127.0.0.1:${port}`,
    reuseExistingServer: false,
    timeout: 120_000,
  },
})
