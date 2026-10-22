import { defineConfig } from '@playwright/test';
for (const name of ['BROWSER_BASE_URL', 'BROWSER_HTTPS_URL', 'BROWSER_FIXTURE_URL', 'BROWSER_FIXTURE_TOKEN', 'BROWSER_GATEWAY_KEY', 'BROWSER_NETWORK_PROXY', 'BROWSER_ATTACKER_URL']) {
  if (!process.env[name]) throw new Error(`Missing browser lane prerequisite: ${name}; run TestBrowserJourneys`);
}
export default defineConfig({
  testDir: '.', testMatch: '**/*.spec.ts', fullyParallel: false, workers: 1,
  retries: 0, forbidOnly: true, timeout: 45000,
  expect: { timeout: 10000 }, outputDir: 'test-results',
  reporter: [['list'], ['html', { outputFolder: 'playwright-report', open: 'never' }]],
  use: { browserName: 'chromium', baseURL: process.env.BROWSER_BASE_URL, ignoreHTTPSErrors: true, launchOptions: { proxy: { server: process.env.BROWSER_NETWORK_PROXY!, bypass: '<-loopback>' } }, proxy: { server: process.env.BROWSER_NETWORK_PROXY!, bypass: '<-loopback>' }, trace: 'retain-on-failure', screenshot: 'only-on-failure' },
});
