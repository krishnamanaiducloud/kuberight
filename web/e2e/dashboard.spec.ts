import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page, type TestInfo } from '@playwright/test'

const recommendationId = 'rec_0123456789abcdef0123456789abcdef'

const recommendation = {
  id: recommendationId,
  cluster: 'production-us',
  namespace: 'shop',
  kind: 'Deployment',
  workload: 'checkout',
  container: 'api',
  replicas: 3,
  cpuRequestMilli: 360,
  memoryRequestBytes: 402_653_184,
  cpuLimitMilli: 700,
  memoryLimitBytes: 536_870_912,
  currentCpuRequestMilli: 800,
  currentMemoryRequestBytes: 805_306_368,
  currentCpuLimitMilli: 1_000,
  currentMemoryLimitBytes: 1_073_741_824,
  cpuStatisticsMilli: { p50: 170, p90: 240, p95: 280, p99: 330, max: 460 },
  memoryStatisticsBytes: { p50: 220_200_960, p90: 283_115_520, p95: 304_087_040, p99: 335_544_320, max: 356_515_840 },
  confidence: 'high',
  confidenceScore: 0.94,
  risk: 'low',
  burstable: true,
  cpuThrottlingRatio: 0.02,
  cpuThrottlingPeakRatio: 0.08,
  cpuLimitPinned: false,
  spikes: { classification: 'rare', count: 2, spikesPerDay: 0.3, medianDurationMinutes: 5, peakToMedian: 2.7, threshold: 380 },
  trends: {
    cpu: { slopePerDay: 2, growthPerDayPercent: 0.8, rSquared: 0.12, significant: false, projectedUplift: 0 },
    memory: { slopePerDay: 1_048_576, growthPerDayPercent: 0.4, rSquared: 0.2, significant: false, projectedUplift: 0 },
  },
  backtest: {
    cpuCurrent: { overRequest: 0, overLimit: 0, nearLimit: 0 },
    cpuRecommended: { overRequest: 0.01, overLimit: 0, nearLimit: 0 },
    memoryCurrent: { overRequest: 0, overLimit: 0, nearLimit: 0 },
    memoryRecommended: { overRequest: 0, overLimit: 0, nearLimit: 0 },
  },
  dataQuality: { cpuSampleCount: 1_980, memorySampleCount: 1_976, minimumRequired: 30, sufficient: true, issues: [] },
  estimatedCpuChangePercent: -55,
  estimatedMemoryChangePercent: -50,
  reason: 'P95 demand plus calibrated safety margins covers the observed workload.',
  warnings: [],
  applied: false,
  effectivePolicy: {
    cpuSafetyMargin: 0.2,
    memorySafetyMargin: 0.2,
    limitSafetyMargin: 0.25,
    minimumCpuMilli: 10,
    minimumMemoryBytes: 16_777_216,
    maximumCpuMilli: 1_000_000_000,
    maximumMemoryBytes: 2_305_843_009_213_693_951,
    maximumReduction: 0.5,
    minimumSamples: 30,
    highConfidenceSamples: 120,
    cpuPercentile: 0.95,
    memoryPercentile: 0.99,
  },
  createdAt: '2026-11-03T14:00:00Z',
  createdBy: 'platform-engineer',
  source: 'prometheus',
  provenance: {
    workloadUid: 'fd4f3c40-2e49-45d7-bbe8-e9f8c50b2444',
    workloadGeneration: 17,
    workloadResourceVersion: '88210',
    sourceSpecFingerprint: 'a'.repeat(64),
    queryFingerprint: 'b'.repeat(64),
    policyFingerprint: 'c'.repeat(64),
    optimizerVersion: '0.1.0-rc.2',
    observationStart: '2026-10-27T14:00:00Z',
    observationEnd: '2026-11-03T14:00:00Z',
    expiresAt: '2030-11-04T14:00:00Z',
  },
  reviewStatus: 'pending',
} as const

async function mockControlPlane(page: Page) {
  await page.route('**/api/v1/**', async (route) => {
    const url = new URL(route.request().url())
    let body: unknown
    switch (url.pathname) {
      case '/api/v1/version':
        body = { version: '0.1.0-rc.2', mode: 'recommend' }
        break
      case '/api/v1/overview':
        body = {
          clusters: 2,
          workloads: 148,
          mode: 'recommend',
          generatedAt: '2026-11-03T15:00:00Z',
          inventory: { available: true, source: 'kubernetes-api' },
          recommendations: { total: 34, pending: 7, approved: 22, rejected: 5, source: 'postgresql' },
          estimatedMonthlySavings: 2_480,
          currency: 'USD',
          dependencies: [],
        }
        break
      case '/api/v1/recommendations':
        body = { items: [recommendation], pagination: { page: 1, pageSize: 25, total: 1 }, status: { available: true, source: 'postgresql' } }
        break
      case `/api/v1/recommendations/${recommendationId}`:
        body = recommendation
        break
      default:
        body = { items: [], pagination: { page: 1, pageSize: 25, total: 0 }, status: { available: true, source: 'browser-smoke' } }
    }
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })
  })
}

function captureRuntimeFailures(page: Page) {
  const failures: string[] = []
  page.on('console', (message) => {
    if (message.type() === 'error') failures.push(`console: ${message.text()}`)
  })
  page.on('pageerror', (error) => failures.push(`page: ${error.message}`))
  return failures
}

async function expectNoSeriousAccessibilityViolations(page: Page) {
  const result = await new AxeBuilder({ page })
    .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'])
    .analyze()
  const blocking = result.violations.filter((violation) => ['critical', 'serious'].includes(violation.impact ?? ''))
  expect(blocking, JSON.stringify(blocking, null, 2)).toEqual([])
}

async function attachScreenshot(page: Page, testInfo: TestInfo, name: string) {
  await testInfo.attach(name, { body: await page.screenshot({ fullPage: true }), contentType: 'image/png' })
}

test.beforeEach(async ({ page }) => {
  await mockControlPlane(page)
})

test('renders verified fleet posture without browser or accessibility failures', async ({ page }, testInfo) => {
  const failures = captureRuntimeFailures(page)
  await page.goto('/')
  await expect(page.getByRole('heading', { level: 1, name: 'Rightsize with evidence, not instinct.' })).toBeVisible()
  await expect(page.getByText('148')).toBeVisible()
  await expect(page.getByText('7')).toBeVisible()
  await expectNoSeriousAccessibilityViolations(page)
  await attachScreenshot(page, testInfo, 'overview')
  expect(failures).toEqual([])
})

test('opens a recommendation and exposes provenance and uncertainty evidence', async ({ page }, testInfo) => {
  const failures = captureRuntimeFailures(page)
  await page.goto('/recommendations/cpu?scope=all')
  await expect(page.getByRole('heading', { level: 1, name: 'CPU recommendations' })).toBeVisible()
  const workload = page.getByText('checkout', { exact: true })
  await expect(workload).toHaveCount(1)
  await workload.click()
  await expect(page.getByRole('heading', { name: 'Recommendation decision' })).toBeVisible()
  await expect(page.getByText('Evidence valid')).toBeVisible()
  await expect(page.getByText('CPU throttled 2.0% · peak 8.0%')).toBeVisible()
  await expect(page.getByText('Rare spikes')).toBeVisible()
  await expectNoSeriousAccessibilityViolations(page)
  await attachScreenshot(page, testInfo, 'recommendation-evidence')
  expect(failures).toEqual([])
})

test('keeps primary navigation usable at desktop and mobile widths', async ({ page }) => {
  const failures = captureRuntimeFailures(page)
  await page.goto('/')
  const navigation = page.getByRole('navigation', { name: 'Primary navigation' })
  if (!await navigation.isVisible()) {
    await page.getByRole('button', { name: 'Open navigation' }).click()
  }
  await expect(navigation).toBeVisible()
  await page.getByRole('link', { name: 'CPU hotspots' }).click()
  await expect(page.getByRole('heading', { level: 1, name: 'CPU hotspots' })).toBeVisible()
  expect(failures).toEqual([])
})
