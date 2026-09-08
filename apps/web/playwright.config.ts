import { defineConfig, devices } from "@playwright/test";

/**
 * End-to-end configuration.
 *
 * The suite runs against a stack that is already up (Postgres, the control
 * API, and the seeded paper account). It does not start the backend itself:
 * a test run that silently boots its own database is a test run whose failures
 * are about the harness rather than the application.
 *
 * VANTAGE_E2E_PASSWORD must be supplied by the environment. There is no
 * default, and no credential is committed: a test file with a working password
 * in it is a credential in the repository.
 */

const baseURL = process.env.VANTAGE_E2E_BASE_URL ?? "http://localhost:3000";

export default defineConfig({
  testDir: "./tests/e2e",
  fullyParallel: false, // The suite shares one paper account and its ledger.
  forbidOnly: !!process.env.CI,
  retries: 0, // A flaky trading test is a defect, not something to retry away.
  workers: 1,
  reporter: process.env.CI ? [["github"], ["list"]] : [["list"]],
  timeout: 45_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    video: "off",
    // The terminal is a desktop application; a 1280-wide viewport is the
    // narrowest layout the design system supports without collapsing panels.
    viewport: { width: 1600, height: 900 },
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
  webServer: process.env.VANTAGE_E2E_START_WEB
    ? {
        command: "npm run dev",
        url: baseURL,
        reuseExistingServer: true,
        timeout: 120_000,
      }
    : undefined,
});
