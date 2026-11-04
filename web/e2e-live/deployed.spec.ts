import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'

const token = process.env.KUBERIGHT_E2E_TOKEN
if (!token) throw new Error('KUBERIGHT_E2E_TOKEN is required for live browser tests')

function captureRuntimeFailures(page: Page) {
  const failures: string[] = []
  page.on('console', (message) => {
    if (message.type() === 'error') failures.push(`console: ${message.text()}`)
  })
  page.on('pageerror', (error) => failures.push(`page: ${error.message}`))
  return failures
}

test.beforeEach(async ({ page }) => {
  await page.addInitScript((bearerToken) => {
    window.sessionStorage.setItem('kuberight.token', bearerToken)
  }, token)
})

test('uses the deployed API and renders live Kubernetes inventory', async ({ page }, testInfo) => {
  const failures = captureRuntimeFailures(page)
  const response = await page.request.get('/api/v1/version', {
    headers: { Authorization: `Bearer ${token}` },
  })
  expect(response.status()).toBe(200)
  expect(await response.json()).toMatchObject({ mode: 'recommend' })

  await page.goto('/')
  await expect(page.getByRole('heading', { level: 1, name: 'Rightsize with evidence, not instinct.' })).toBeVisible()
  await page.getByRole('link', { name: 'Workloads' }).click()
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
