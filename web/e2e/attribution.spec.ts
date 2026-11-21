import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page, type TestInfo } from '@playwright/test'

// Full-page accessibility scans and screenshots of the expanded panels are
// slow on a loaded machine.
test.describe.configure({ timeout: 90_000 })

// The reported workload: default/netshoot, one container, no requests set.
const workload = {
  cluster: 'kind', namespace: 'default', kind: 'Deployment', name: 'netshoot', desiredReplicas: 1, readyReplicas: 1, health: 'healthy',
  containers: [{ name: 'netshoot', cpuRequestMilli: 0, memoryRequestBytes: 0, cpuLimitMilli: 0, memoryLimitBytes: 0 }],
}
const start = Date.UTC(2026, 10, 20, 0, 0, 0)
const series = (resource: string, unit: string, value: number) => ({
  resource, unit, quality: { issues: [], coverage: 1, expectedSamples: 48, acceptedSamples: 48 },
  points: Array.from({ length: 48 }, (_, index) => ({ timestamp: new Date(start + index * 1_800_000).toISOString(), value: value + (index % 4) })),
})

const attribution = {
  window: '24h0m0s', step: '5m0s',
  identity: {
    kind: 'Deployment', name: 'netshoot', namespace: 'default', uid: '96d2e881-c4a9-4393-b3bb-34837df30b0b', createdAt: '2026-11-19T04:36:42Z', expectedReplicas: 1,
    owners: [{ kind: 'ReplicaSet', name: 'netshoot-697fd6796', uid: '5c8f0620-0ef5-418a-8f92-19583f01b377', createdAt: '2026-11-19T04:36:42Z', replicas: 1 }],
    pods: [{ name: 'netshoot-697fd6796-42dmb', uid: 'a1fd6710-fbcf-491d-a9e4-a5effa0acd91', createdAt: '2026-11-19T04:36:42Z', phase: 'Running', containers: ['netshoot'] }],
    resolutionIssues: [],
  },
  report: {
    start: '2026-11-20T00:00:00Z', end: '2026-11-21T00:00:00Z', step: '5m0s', workloadUid: '96d2e881-c4a9-4393-b3bb-34837df30b0b', workloadCreatedAt: '2026-11-19T04:36:42Z',
    owners: [{ kind: 'ReplicaSet', name: 'netshoot-697fd6796', uid: '5c8f0620-0ef5-418a-8f92-19583f01b377', verified: true, recorded: true, createdAt: '2026-11-19T04:36:42Z', replicas: 1 }],
    pods: [
      { pod: 'netshoot-697fd6796-42dmb', uid: 'a1fd6710-fbcf-491d-a9e4-a5effa0acd91', ownerKind: 'ReplicaSet', owner: 'netshoot-697fd6796', live: true, createdAt: '2026-11-19T04:36:45Z', createdFrom: 'kube_pod_start_time', hasUsage: true, attributed: true, reason: 'attributed' },
      { pod: 'netshoot-6b9c7f5d4-x8k2p', uid: '11111111-2222-3333-4444-555555555555', ownerKind: 'ReplicaSet', owner: 'netshoot-6b9c7f5d4', live: false, createdAt: '2026-11-18T10:00:00Z', createdFrom: 'kube_pod_start_time', hasUsage: true, attributed: false, reason: 'created at 2026-11-18T10:00:00Z, before the current workload object (2026-11-19T04:36:42Z): a same-name predecessor\'s or an adopted pod' },
    ],
    usagePods: 2, attributedPods: 1, unattributedPods: 1,
    families: [
      { name: 'container_cpu_usage_seconds_total', purpose: 'CPU usage (cAdvisor)', required: true, series: 2 },
      { name: 'kube_pod_owner', purpose: 'links each pod UID to its controller', required: true, series: 2 },
      { name: 'kube_pod_created', purpose: 'pod creation time', required: true, alternative: 'kube_pod_start_time', series: 0 },
      { name: 'kube_pod_start_time', purpose: 'pod start time, used when kube_pod_created is not exported (OpenShift)', required: true, alternative: 'kube_pod_created', series: 2 },
      { name: 'container_cpu_cfs_throttled_periods_total', purpose: 'CPU throttling (exported only for containers with a CPU limit)', required: false, series: 0 },
    ],
    missingFamilies: [], coveragePercent: 100, eligible: true, outcome: 'attributed', reason: 'every expected replica is attributed to the workload UID with CPU and memory history',
    coverage: { complete: true, issues: [], discoveredReplicas: 1, revisions: 1, unattributedSteps: 0, ambiguousSteps: 0, cpuReplicaCoverage: 1, memoryReplicaCoverage: 1 },
  },
}

type Mock = { signalsUrls: string[]; generate: (container: string) => { status: number; body: unknown } }

async function mockApi(page: Page, mock: Mock) {
  await page.route('**/api/v1/**', async (route) => {
    const url = new URL(route.request().url())
    let status = 200
    let body: unknown
    switch (url.pathname) {
      case '/api/v1/version': body = { version: '0.1.0-dev', mode: 'recommend' }; break
      case '/api/v1/workloads': body = { items: [workload], pagination: { page: 1, pageSize: 200, total: 1 }, status: { available: true, source: 'kubernetes-api' } }; break
      case '/api/v1/attribution': body = attribution; break
      case '/api/v1/metrics/signals': {
        mock.signalsUrls.push(url.search)
        const custom = url.searchParams.has('start')
        body = {
          status: { available: true, source: 'prometheus' },
          signals: { start: new Date(start).toISOString(), end: new Date(start + 86_400_000).toISOString(), step: '5m0s', cpu: series('cpu', 'millicores', 3), memory: series('memory', 'bytes', 3e6), events: [], warnings: [] },
          correlations: {}, insights: [], latencyConfigured: false, step: '5m0s',
          range: custom
            ? { requestedStart: url.searchParams.get('start'), requestedEnd: url.searchParams.get('end'), dataStart: new Date(start).toISOString(), note: 'Prometheus has samples for this container from 2026-11-20T00:00:00Z; the earlier part of the range is before the workload ran or beyond the data source\'s retention' }
            : { requestedStart: new Date(start).toISOString(), requestedEnd: new Date(start + 86_400_000).toISOString(), dataStart: new Date(start).toISOString() },
        }
        break
      }
      case '/api/v1/recommendations:generate': {
        const request = JSON.parse(route.request().postData() ?? '{}') as { container: string }
        const result = mock.generate(request.container)
        status = result.status
        body = result.body
        break
      }
      default: body = { items: [], pagination: { page: 1, pageSize: 25, total: 0 }, status: { available: true, source: 'browser-smoke' } }
    }
    await route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
  })
}

function captureRuntimeFailures(page: Page) {
  const failures: string[] = []
  page.on('console', (message) => {
    // A deliberately mocked 422 is logged by the browser as a failed resource.
    if (message.type() === 'error' && !message.text().includes('status of 422')) failures.push(`console: ${message.text()}`)
  })
  page.on('pageerror', (error) => failures.push(`page: ${error.message}`))
  return failures
}

async function expectNoHorizontalOverflow(page: Page) {
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  expect(overflow, 'page must not scroll horizontally').toBeLessThanOrEqual(1)
}

async function expectAccessible(page: Page) {
  const result = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze()
  const blocking = result.violations.filter((violation) => ['critical', 'serious'].includes(violation.impact ?? ''))
  expect(blocking, JSON.stringify(blocking, null, 2)).toEqual([])
}

async function attach(page: Page, testInfo: TestInfo, name: string) {
  await testInfo.attach(name, { body: await page.screenshot({ fullPage: true }), contentType: 'image/png' })
}

test('explains how netshoot usage is attributed through the UID chain', async ({ page }, testInfo) => {
  const failures = captureRuntimeFailures(page)
  await mockApi(page, { signalsUrls: [], generate: () => ({ status: 500, body: {} }) })
  await page.goto('/workloads/default/Deployment/netshoot')
  await expect(page.getByRole('heading', { level: 1, name: 'netshoot' })).toBeVisible()
  await page.getByRole('button', { name: /Metric attribution/ }).click()
  await expect(page.getByText('every expected replica is attributed to the workload UID with CPU and memory history')).toBeVisible()
  await expect(page.getByText('96d2e881-c4a9-4393-b3bb-34837df30b0b')).toBeVisible()
  await expect(page.getByText('API owner UID')).toBeVisible()
  await expect(page.getByText(/before the current workload object/)).toBeVisible()
  await expect(page.getByText('Not exported; kube_pod_start_time used')).toBeVisible()
  await expect(page.getByText('100.0%')).toBeVisible()
  await expectNoHorizontalOverflow(page)
  await expectAccessible(page)
  await attach(page, testInfo, 'attribution-panel')
  expect(failures).toEqual([])
})

test('reports each analysis outcome instead of a generic skip', async ({ page }, testInfo) => {
  const failures = captureRuntimeFailures(page)
  const multi = { ...workload, containers: [
    { name: 'netshoot', cpuRequestMilli: 0, memoryRequestBytes: 0, cpuLimitMilli: 0, memoryLimitBytes: 0 },
    { name: 'sidecar', cpuRequestMilli: 100, memoryRequestBytes: 64 << 20, cpuLimitMilli: 0, memoryLimitBytes: 0 },
    { name: 'legacy', cpuRequestMilli: 100, memoryRequestBytes: 64 << 20, cpuLimitMilli: 0, memoryLimitBytes: 0 },
  ] }
  const record = (container: string, status: string, cpu: number) => ({
    id: `rec_${container}`, namespace: 'default', kind: 'Deployment', workload: 'netshoot', container, risk: 'low', reviewStatus: 'pending',
    currentCpuRequestMilli: 100, cpuRequestMilli: cpu, currentMemoryRequestBytes: 64 << 20, memoryRequestBytes: 64 << 20, assessment: { status },
  })
  await mockApi(page, {
    signalsUrls: [],
    generate: (container) => ({
      netshoot: { status: 422, body: { error: { code: 'insufficient_history', message: 'insufficient usage history: the current workload has existed for less than one 5m0s observation step' } } },
      sidecar: { status: 200, body: record('sidecar', 'no_change', 100) },
      legacy: { status: 422, body: { error: { code: 'metric_attribution_incomplete', message: 'workload metric attribution is incomplete: usage exists for pods matching the workload\'s names' } } },
    } as Record<string, { status: number; body: unknown }>)[container],
  })
  await page.route('**/api/v1/workloads?**', (route) => route.fulfill({ json: { items: [multi], pagination: { page: 1, pageSize: 200, total: 1 }, status: { available: true, source: 'kubernetes-api' } } }))
  await page.goto('/workloads/default/Deployment/netshoot')
  await page.getByRole('button', { name: 'Analyze all containers' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('button', { name: 'Analyze' }).click()
  await expect(dialog.getByText('3 of 3 containers analyzed')).toBeVisible()
  const results = dialog.getByRole('table', { name: 'Analysis results' })
  await expect(results.getByText('Already right-sized', { exact: true })).toBeVisible()
  await expect(results.getByText('Insufficient usage history', { exact: true })).toBeVisible()
  await expect(results.getByText('Incomplete workload attribution', { exact: true })).toBeVisible()
  const outcomes = dialog.getByLabel('Analysis outcomes')
  await expect(outcomes.getByText('1 already right-sized')).toBeVisible()
  await expect(outcomes.getByText('1 insufficient usage history')).toBeVisible()
  await expect(dialog.getByText(/skipped/)).toHaveCount(0)
  await expectNoHorizontalOverflow(page)
  await attach(page, testInfo, 'analysis-outcomes')
  expect(failures).toEqual([])
})

test('queries a custom range in UTC and explains a range beyond retention', async ({ page }, testInfo) => {
  const failures = captureRuntimeFailures(page)
  const mock: Mock = { signalsUrls: [], generate: () => ({ status: 500, body: {} }) }
  await mockApi(page, mock)
  await page.goto('/workloads/default/Deployment/netshoot')
  await expect(page.getByRole('heading', { level: 1, name: 'netshoot' })).toBeVisible()
  await page.getByLabel('Range').click()
  await page.getByRole('option', { name: '365 days' }).click()
  await expect.poll(() => mock.signalsUrls.some((search) => search.includes('window=8760h'))).toBe(true)
  await page.getByLabel('Range').click()
  await page.getByRole('option', { name: 'Custom range…' }).click()
  const form = page.getByRole('form', { name: 'Custom time range' })
  // An end before the start is rejected in the browser.
  await form.getByLabel('Start').fill('2026-11-15T10:00')
  await form.getByLabel('End').fill('2026-11-15T09:00')
  await expect(page.getByText('The end must be after the start.')).toBeVisible()
  await expect(form.getByRole('button', { name: 'Apply' })).toBeDisabled()
  await form.getByLabel('End').fill('2026-11-15T10:30')
  await form.getByRole('button', { name: 'Apply' }).click()
  await expect.poll(() => mock.signalsUrls.at(-1) ?? '').toContain('start=')
  const last = new URLSearchParams(mock.signalsUrls.at(-1))
  expect(last.get('start')).toMatch(/Z$/)
  expect(Date.parse(last.get('end')!) - Date.parse(last.get('start')!)).toBe(30 * 60_000)
  expect(last.has('window')).toBe(false)
  await expect(page.getByText(/beyond the data source's retention/)).toBeVisible()
  await expect(page.getByText(/queries run in UTC/)).toBeVisible()
  await expect(page).toHaveURL(/from=.*to=/)
  await expectNoHorizontalOverflow(page)
  await attach(page, testInfo, 'custom-range')
  expect(failures).toEqual([])
})
