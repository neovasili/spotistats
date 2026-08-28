import { defineConfig, devices } from '@playwright/test'

/**
 * Playwright smoke suite.
 *
 * This covers the one thing Vitest and a curl check cannot: whether the app actually RENDERS.
 * The unit suite mounts components against fixtures, and the deploy's curl check proves the
 * origin returns 200 — neither notices that React failed to mount, the snapshot fetch 404'd,
 * or a chart threw on real data. Every one of those serves a perfectly valid 200 with an empty
 * page.
 *
 * SMOKE_BASE_URL picks the target:
 *   a local dev server (default)  make web-dev, then make smoke
 *   the deployed site             SMOKE_BASE_URL=https://spotistats.neovasili.com make smoke
 *
 * The default is deliberately LOCAL. This is a public repository, and a suite that defaults to
 * production means every contributor who types `make smoke` loads someone else's live site.
 * deploy.yml sets SMOKE_BASE_URL explicitly, which is the one place production is the target.
 *
 * It is NOT wired into ci.yml: a browser download plus a real-browser run is the most expensive
 * check in the suite, and there is no deployed build to point it at on a pull request.
 */
const baseURL = process.env.SMOKE_BASE_URL ?? 'http://localhost:5173'

export default defineConfig({
  testDir: './e2e',
  // Against production these are read-only checks, so parallelism is safe and much faster.
  fullyParallel: true,
  // A smoke suite that passes on the second attempt has still told you something is flaky, but
  // a single network blip should not fail a deploy check.
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? 'github' : 'list',
  timeout: 30_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL,
    // Artefacts only for failures: a passing smoke run should leave nothing behind.
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})
