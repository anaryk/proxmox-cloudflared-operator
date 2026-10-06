import { defineConfig, devices, type Project } from '@playwright/test'

// The browser suite, e2e/*.spec.ts, runs against the real pco web with the
// fakes of hack/ behind it (e2e/fixtures.ts; make ui-e2e builds them), in
// Chromium, Firefox and WebKit at the size of a desktop and of a phone, and
// in Chromium in both colour schemes. Chromium decides; the others are
// advisory until they have been green for two weeks. e2e/perf measures the
// flow map in Chromium alone, one test at a time.
const sizes = {
  desktop: { width: 1440, height: 900 },
  phone: { width: 375, height: 812 },
}

// The first project of the suite is the one that runs the specs whose
// requests no browser makes.
const first = 'chromium-desktop-light'
const withoutBrowser = ['csrf.spec.ts']

// Playwright's Firefox now and then never sees a navigation of the page
// finish, though the page loads and runs: a test of Firefox gets a second
// try, and one that passes on it is reported as flaky, not hidden.
const retries: Record<string, number> = { firefox: 1 }

function suite(name: string, device: (typeof devices)[string], colorScheme?: 'light' | 'dark'): Project[] {
  return Object.entries(sizes).map(([size, viewport]) => {
    const project = [name, size, colorScheme].filter(Boolean).join('-')
    return {
      name: project,
      testIgnore: ['perf/**', ...(project === first ? [] : withoutBrowser)],
      retries: retries[name] ?? 0,
      use: { ...device, viewport, ...(colorScheme && { colorScheme }) },
    }
  })
}

export default defineConfig({
  testDir: 'e2e',
  outputDir: 'test-results/playwright',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  timeout: 90_000,
  reporter: 'list',
  use: {
    // pco web's certificate is made for the test and trusted by nothing.
    ignoreHTTPSErrors: true,
    trace: 'retain-on-failure',
  },
  projects: [
    ...suite('chromium', devices['Desktop Chrome'], 'light'),
    ...suite('chromium', devices['Desktop Chrome'], 'dark'),
    ...suite('firefox', devices['Desktop Firefox']),
    ...suite('webkit', devices['Desktop Safari']),
    {
      name: 'perf',
      testDir: 'e2e/perf',
      workers: 1,
      timeout: 180_000,
      use: { ...devices['Desktop Chrome'], viewport: sizes.desktop },
    },
  ],
})
