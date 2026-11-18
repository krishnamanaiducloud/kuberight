import { defineConfig, devices } from '@playwright/test'

const baseURL = process.env.KUBERIGHT_E2E_BASE_URL
if (!baseURL) throw new Error('KUBERIGHT_E2E_BASE_URL is required for live browser tests')

export default defineConfig({
  testDir: './e2e-live',
  fullyParallel: false,
  forbidOnly: true,
  retries: process.env.CI ? 2 : 0,
  workers: 1,
  reporter: [
    ['list'],
    ['html', { outputFolder: 'playwright-live-report', open: 'never' }],
  ],
  outputDir: 'test-results-live',
  use: {
    baseURL,
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
    video: 'retain-on-failure',
  },
  projects: [{ name: 'live-chromium', use: { ...devices['Desktop Chrome'] } }],
})
