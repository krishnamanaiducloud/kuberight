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
  cpuPressureRatio: 0.01,
  cpuPressurePeakRatio: 0.03,
  memoryPressurePeakRatio: 0.002,
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
    maximumMemoryBytes: 1_099_511_627_776,
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
    optimizerVersion: '0.1.0-rc.4',
    observationStart: '2026-10-27T14:00:00Z',
    observationEnd: '2026-11-03T14:00:00Z',
    expiresAt: '2030-11-04T14:00:00Z',
  },
  candidates: [
    { profile: 'conservative', selected: false, cpuRequestMilli: 720, memoryRequestBytes: 536_870_912, cpuLimitMilli: 1_000, memoryLimitBytes: 805_306_368, estimatedCpuChangePercent: -10, estimatedMemoryChangePercent: -33, status: 'recommend', backtest: { cpuCurrent: {}, cpuRecommended: { overRequest: 0 }, memoryCurrent: {}, memoryRecommended: { overRequest: 0 } }, outOfSample: { performed: true, trainSamples: 1_386, testSamples: 594, cpuOverRequest: 0, memoryOverLimit: 0, passed: true, detail: '' } },
    { profile: 'balanced', selected: true, cpuRequestMilli: 360, memoryRequestBytes: 402_653_184, cpuLimitMilli: 700, memoryLimitBytes: 536_870_912, estimatedCpuChangePercent: -55, estimatedMemoryChangePercent: -50, status: 'recommend', backtest: { cpuCurrent: {}, cpuRecommended: { overRequest: 0.01 }, memoryCurrent: {}, memoryRecommended: { overRequest: 0 } }, outOfSample: { performed: true, trainSamples: 1_386, testSamples: 594, cpuOverRequest: 0.012, memoryOverLimit: 0, passed: true, detail: '' } },
    { profile: 'aggressive', selected: false, cpuRequestMilli: 330, memoryRequestBytes: 402_653_184, cpuLimitMilli: 600, memoryLimitBytes: 469_762_048, estimatedCpuChangePercent: -59, estimatedMemoryChangePercent: -50, status: 'hold', backtest: { cpuCurrent: {}, cpuRecommended: { overRequest: 0.06 }, memoryCurrent: {}, memoryRecommended: { overRequest: 0 } }, outOfSample: { performed: true, trainSamples: 1_386, testSamples: 594, cpuOverRequest: 0.071, memoryOverLimit: 0, passed: false, detail: '' } },
  ],
  reviewStatus: 'pending',
} as const

async function mockControlPlane(page: Page) {
  await page.route('**/api/v1/**', async (route) => {
    const url = new URL(route.request().url())
    let body: unknown
    switch (url.pathname) {
      case '/api/v1/version':
        body = { version: '0.1.0-rc.4', mode: 'recommend' }
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
      case '/api/v1/capacity':
        body = {
          status: { available: true, source: 'kubernetes-api' },
          nodes: { total: 6, schedulable: 6 },
          allocatable: { cpuMilli: 23_520, memoryBytes: 90_194_313_216 },
          current: { nodesNeeded: 6, unplacedPods: 0, cpuRequestedMilli: 18_400, memoryRequestedBytes: 38_654_705_664, noEligibleNodePods: 0, podRulePods: 0, pinnedPods: 2, unplacedWorkloads: [] },
          recommended: { nodesNeeded: 4, unplacedPods: 0, cpuRequestedMilli: 11_200, memoryRequestedBytes: 27_917_287_424, noEligibleNodePods: 0, unplacedWorkloads: [] },
          reclaimableNodes: 2,
          containersWithRecommendations: 22,
          autoscaling: { karpenter: true, clusterAutoscaler: false, nodePools: [{ name: 'default', consolidationPolicy: 'WhenEmptyOrUnderutilized', disruptionBudgets: ['10% nodes'] }], warnings: [] },
          nodeGroups: [
            { name: 'default', provider: 'karpenter', membership: 'verified', evidence: 'label karpenter.sh/nodepool', nodes: 4, schedulableNodes: 4, instanceTypes: ['m6i.xlarge'], capacityTypes: ['spot'], cpuMilli: 15_680, memoryBytes: 60_129_542_144, currentNodesNeeded: 4, recommendedNodesNeeded: 2, reclaimableNodes: 2, realization: 'Karpenter consolidates underutilized nodes in this NodePool within its disruption budgets.' },
            { name: 'shape:3920m-15360Mi', provider: 'none', membership: 'inferred', evidence: 'no node-group label; grouped by node shape', nodes: 2, schedulableNodes: 2, instanceTypes: [], capacityTypes: [], cpuMilli: 7_840, memoryBytes: 30_064_771_072, currentNodesNeeded: 2, recommendedNodesNeeded: 2, reclaimableNodes: 0, realization: 'No node group was identified for these nodes; drain and remove them manually.' },
          ],
          realization: 'Karpenter consolidates underutilized nodes automatically once the lower requests are applied.',
          evictionBlockers: ['PodDisruptionBudget payments/ledger allows no voluntary evictions; 2 pods stay on their nodes'],
          costTiers: {
            available: true, currency: 'USD', priceSource: 'configured',
            requestReduction: { monthly: 524.6, detail: 'Lower requests across every replica, priced at unit prices for 730 hours. Requests alone do not change the bill.' },
            feasibleNodeSavings: { nodes: 2, monthly: 321.2, detail: 'The allocatable CPU and memory of the nodes the packing frees, after node-group minimums.' },
            billing: { measured: false, detail: 'Cloud billing is not ingested.' },
          },
          assumptions: ['Pods are packed by requests with first-fit-decreasing onto existing node shapes, largest first.'],
        }
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
  await expect(page.getByRole('dialog').getByText('Evidence valid')).toBeVisible()
  await expect(page.getByText('CPU throttled 2.0% · peak 8.0%')).toBeVisible()
  await expect(page.getByText('CPU waiting 1.0% · peak 3.0%')).toBeVisible()
  await expect(page.getByText('Memory stalled peak 0.2%')).toBeVisible()
  const alternatives = page.getByRole('table', { name: 'Alternative configurations by profile' })
  await expect(alternatives.getByText('Recommended')).toBeVisible()
  await expect(alternatives.getByText('7.1%')).toBeVisible()
  await expect(page.getByText('Rare spikes')).toBeVisible()
  await expectNoSeriousAccessibilityViolations(page)
  await attachScreenshot(page, testInfo, 'recommendation-evidence')
  expect(failures).toEqual([])
})

test('shows node impact per node group with membership evidence', async ({ page }, testInfo) => {
  const failures = captureRuntimeFailures(page)
  await page.goto('/efficiency')
  const groups = page.getByRole('table', { name: 'Node impact by node group' })
  await expect(groups).toBeVisible()
  await expect(groups.getByText('Karpenter NodePool · m6i.xlarge · spot')).toBeVisible()
  await expect(groups.getByText('4 → 2')).toBeVisible()
  await expect(groups.getByText('Inferred')).toBeVisible()
  await expect(page.getByText('2 pods stay on their nodes because a PodDisruptionBudget allows no evictions', { exact: false })).toBeVisible()
  await expect(page.getByText('PodDisruptionBudget payments/ledger allows no voluntary evictions; 2 pods stay on their nodes.')).toBeVisible()
  const tiers = page.getByLabel('Savings by evidence')
  await expect(tiers.getByText('Feasible from removing 2 nodes')).toBeVisible()
  await expect(tiers.getByText('$321.20/month')).toBeVisible()
  await expect(tiers.getByText('Not measured')).toBeVisible()
  await expectNoSeriousAccessibilityViolations(page)
  await attachScreenshot(page, testInfo, 'node-groups')
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

// Every primary view, audited against WCAG 2.0/2.1 A and AA at desktop and
// mobile widths with the mocked control plane.
const auditedRoutes = ['/', '/efficiency', '/hotspots', '/health', '/clusters', '/namespaces', '/workloads', '/utilization', '/correlation',
  '/recommendations/cpu', '/recommendations/memory', '/approvals', '/fleet', '/savings', '/forecasting',
  '/autoscalers', '/policies', '/gitops', '/audit', '/settings']

for (const route of auditedRoutes) {
  test(`has no serious accessibility violations on ${route}`, async ({ page }) => {
    const failures = captureRuntimeFailures(page)
    await page.goto(route)
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible()
    await page.waitForLoadState('networkidle')
    await expectNoSeriousAccessibilityViolations(page)
    expect(failures).toEqual([])
  })
}
