import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'

function captureRuntimeFailures(page: Page) {
  const failures: string[] = []
  page.on('console', (message) => {
    if (message.type() === 'error') failures.push(`console: ${message.text()}`)
  })
  page.on('pageerror', (error) => failures.push(`page: ${error.message}`))
  return failures
}

test('uses the deployed API without any sign-in and renders live Kubernetes inventory', async ({ page }, testInfo) => {
  const failures = captureRuntimeFailures(page)
  const response = await page.request.get('/api/v1/version')
  expect(response.status()).toBe(200)
  expect(await response.json()).toMatchObject({ mode: 'recommend' })

  await page.goto('/')
  await expect(page).toHaveURL(/\/recommendations\/cpu$/)
  await expect(page.getByRole('heading', { level: 1, name: 'CPU recommendations' })).toBeVisible()
  await page.goto('/overview')
  await expect(page.getByRole('heading', { level: 1, name: 'Rightsize with evidence, not instinct.' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Workload health' })).toBeVisible()
  await expect(page.getByText('recommend mode', { exact: true })).toBeVisible()
  await expect(page.getByText(/unconfirmed/i)).toHaveCount(0)
  await expect(page.getByRole('button', { name: /connect/i })).toHaveCount(0)
  await page.goto('/workloads?scope=all')
  await expect(page.getByRole('heading', { level: 1, name: 'Workloads' })).toBeVisible()
  await expect(page.getByText('kuberight', { exact: true }).first()).toBeVisible()

  const accessibility = await new AxeBuilder({ page })
    .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'])
    .analyze()
  const blocking = accessibility.violations.filter((violation) => ['critical', 'serious'].includes(violation.impact ?? ''))
  expect(blocking, JSON.stringify(blocking, null, 2)).toEqual([])
  await testInfo.attach('live-workloads', { body: await page.screenshot({ fullPage: true }), contentType: 'image/png' })
  expect(failures).toEqual([])
})

test('serves the KubeRight favicon set from the deployed image', async ({ page }) => {
  const missing: string[] = []
  page.on('response', (response) => {
    if (response.status() === 404) missing.push(response.url())
  })
  await page.goto('/overview')
  await expect(page).toHaveTitle('KubeRight | Kubernetes Resource Intelligence')
  for (const [file, type] of Object.entries({
    'favicon.ico': 'image/x-icon',
    'favicon.svg': 'image/svg+xml',
    'favicon-16x16.png': 'image/png',
    'favicon-32x32.png': 'image/png',
    'apple-touch-icon.png': 'image/png',
    'android-chrome-192x192.png': 'image/png',
    'android-chrome-512x512.png': 'image/png',
    'site.webmanifest': 'application/manifest+json',
  })) {
    const response = await page.request.get(file)
    expect(response.status(), file).toBe(200)
    expect(response.headers()['content-type'], file).toBe(type)
  }
  const absent = await page.request.get('favicon-64x64.png')
  expect(absent.status()).toBe(404)
  expect(absent.headers()['content-type']).not.toMatch(/html/)
  expect(missing).toEqual([])
})
