import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { vi } from 'vitest'
import type { Assessment, CorrelationResponse } from '../api'
import { AssessmentPanel } from '../components/AssessmentPanel'
import { Correlation } from './Correlation'

const start = Date.parse('2026-10-15T00:00:00Z')
const series = (resource: 'cpu' | 'memory', value: (index: number) => number) => ({
  resource, unit: resource === 'cpu' ? 'millicores' as const : 'bytes' as const,
  points: Array.from({ length: 24 }, (_, index) => ({ timestamp: new Date(start + index * 3_600_000).toISOString(), value: value(index) })),
  quality: { issues: [], coverage: 1, expectedSamples: 24, acceptedSamples: 24 },
})

const assessment: Assessment = {
  status: 'hold',
  statusReasons: ['ResourceQuota team: requests.cpu needs 600m more but only 100m remains'],
  riskScore: 0.52,
  current: { cpuRequest: '1', cpuLimit: '2', memoryRequest: '1Gi', memoryLimit: '2Gi' },
  recommended: { cpuRequest: '800m', cpuLimit: '1600m', memoryRequest: '820Mi', memoryLimit: '1640Mi' },
  safetyMargins: { cpu: { base: 0.3, variability: 0.05, burst: 0.1, uncertainty: 0, criticality: 0, total: 0.45 }, memory: { base: 0.25, variability: 0, burst: 0, uncertainty: 0, criticality: 0, total: 0.25 } },
  validation: [{ window: 'last 1.8d', samples: 500, cpuOverRequest: 0, memoryOverRequest: 0, memoryOverLimit: 0, passed: true }],
  outOfSample: { performed: true, trainSamples: 1400, testSamples: 600, cpuOverRequest: 0.01, memoryOverRequest: 0, memoryOverLimit: 0, passed: true, detail: 'sized from the first 1400 samples, the recommendation held on the 600 later samples' },
  qos: { current: 'Burstable', recommended: 'Burstable', protected: false },
  checks: [
    { name: 'LimitRange defaults', status: 'pass', detail: 'within the container bounds' },
    { name: 'ResourceQuota team', status: 'fail', detail: 'requests.cpu needs 600m more but only 100m remains' },
    { name: 'Scheduling', status: 'unknown', detail: 'node capacity was not available' },
  ],
  autoscaling: { hpa: { targetUtilization: 70, currentUtilization: 30, projectedUtilization: 38, currentReplicas: 3, currentDesiredReplicas: 2, projectedReplicas: 2, unboundedReplicas: 2, minReplicas: 2, maxReplicas: 10, detail: 'average utilization moves from 30% to 38% of the request' } },
  forecast: { source: 'ml', horizon: '7.0d', cpuMilli: 450, cpuUpperMilli: 520, memoryBytes: 400 << 20, intervalLevel: 0.9, empiricalCoverage: 0.91, calibrated: true, usedForSizing: false },
  expectedSavings: { cpuMilli: 600, memoryBytes: 612 << 20 },
  validFor: '12h0m0s',
  validForSeconds: 43_200,
  explanation: ['CPU is sized from P95 usage (300m) plus a 45% safety margin; memory from P99 (600Mi) plus 25%.'],
}

const response: CorrelationResponse = {
  status: { available: true, source: 'prometheus' },
  signals: {
    start: new Date(start).toISOString(), end: new Date(start + 23 * 3_600_000).toISOString(), step: '1h0m0s',
    cpu: series('cpu', (index) => 300 + index * 10), memory: series('memory', () => 500 << 20),
    events: [{ timestamp: new Date(start + 12 * 3_600_000).toISOString(), kind: 'rollout', detail: 'api-7d9f' }], warnings: [],
  },
  current: { name: 'app', cpuRequestMilli: 1000, cpuLimitMilli: 2000, memoryRequestBytes: 1 << 30, memoryLimitBytes: 2 << 30 },
  report: {
    start: new Date(start).toISOString(), end: new Date(start + 23 * 3_600_000).toISOString(), stepSeconds: 3600, points: 24,
    correlationStart: new Date(start).toISOString(), coverage: { cpu: 1, memory: 1 },
    pairs: [
      { x: 'cpu', y: 'latency', r: 0.81, spearman: 0.78, n: 24, effectiveN: 20, lower: 0.6, upper: 0.92, pValue: 1e-6, significant: true, strength: 'strong', direction: 'positive', bestLagSteps: 1, bestLagSeconds: 3600, bestLagR: 0.86, leads: 'cpu', summary: 'Cpu and latency show a strong positive association.' },
      { x: 'cpu', y: 'memory', r: 0.05, spearman: 0.02, n: 24, effectiveN: 6, lower: -0.7, upper: 0.75, pValue: 0.9, significant: false, strength: 'negligible', direction: 'none', bestLagSteps: 0, bestLagSeconds: 0, bestLagR: 0.05, summary: 'No reliable association between cpu and memory.' },
    ],
    changePoints: [{ series: 'cpu', timestamp: new Date(start + 12 * 3_600_000).toISOString(), meanBefore: 300, meanAfter: 480, relativeChange: 0.6, score: 9, nearestEvent: { timestamp: new Date(start + 12 * 3_600_000).toISOString(), kind: 'rollout' } }],
    events: [{ timestamp: new Date(start + 12 * 3_600_000).toISOString(), kind: 'rollout', detail: 'api-7d9f' }],
    eventImpacts: [{ event: { timestamp: new Date(start + 12 * 3_600_000).toISOString(), kind: 'rollout' }, series: 'cpu', before: 300, after: 480, beforeSamples: 6, afterSamples: 6, relativeChange: 0.6, material: true }],
    seasonality: {},
    findings: [{ severity: 'info', title: 'A rollout coincides with a usage shift', detail: 'cpu level changed +60%.', evidence: 'mean 300 to 480' }],
    caveats: ['Correlation measures association only.'],
  },
  forecast: {
    available: true, resource: 'cpu', unit: 'millicores', nominalCoverage: 0.9, calibrated: true,
    overall: { samples: 72, mae: 12, rmse: 15, smape: 0.04, coverage: 0.93, meanIntervalWidth: 0.2, baselineMAE: 20, skill: 0.4 },
    points: Array.from({ length: 6 }, (_, index) => ({ timestamp: new Date(start + index * 3_600_000).toISOString(), actual: 300, predicted: 310, lower: 280, upper: 340, baseline: 300 })),
  },
  recommendation: {
    id: 'rec_1', namespace: 'shop', kind: 'Deployment', workload: 'api', container: 'app',
    cpuRequestMilli: 800, memoryRequestBytes: 820 << 20, cpuLimitMilli: 1600, memoryLimitBytes: 1640 << 20,
    currentCpuRequestMilli: 1000, currentMemoryRequestBytes: 1 << 30, currentCpuLimitMilli: 2000, currentMemoryLimitBytes: 2 << 30,
    cpuStatisticsMilli: { p50: 300, p90: 340, p95: 360, p99: 400, max: 420 }, memoryStatisticsBytes: { p50: 1, p90: 1, p95: 1, p99: 1, max: 1 },
    confidence: 'high', confidenceScore: 0.9, risk: 'medium', burstable: false,
    dataQuality: { cpuSampleCount: 2000, memorySampleCount: 2000, minimumRequired: 30, sufficient: true, issues: [] },
    reason: 'P95 CPU', warnings: ['changing CPU requests can change HPA utilization and replica behavior'], applied: false,
    createdAt: new Date(start).toISOString(), source: 'prometheus', reviewStatus: 'approved', assessment,
  },
  verification: { recommendationId: 'rec_1', status: 'regressed', applied: true, checks: [{ name: 'OOM kills', before: 0, after: 2, regressed: true, detail: '2 OOM kill(s) after the change, 0 before' }], reasons: ['2 OOM kill(s) after the change, 0 before'], checkedAt: new Date(start).toISOString() },
  warnings: [],
  configured: { latency: true, requestRate: false },
}

describe('Correlation analysis', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows correlations with intervals, events, forecast accuracy, evidence, and before/after', async () => {
    vi.stubGlobal('fetch', vi.fn().mockImplementation(async (url: string) => ({
      ok: true, status: 200, headers: new Headers(),
      json: async () => (url.includes('/api/v1/correlation') ? response : { items: [], status: { available: true } }),
    })))
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={['/correlation?namespace=shop&kind=Deployment&workload=api&container=app&window=24h']}>
          <Correlation title="Correlation analysis" description="d" />
        </MemoryRouter>
      </QueryClientProvider>,
    )
    expect(await screen.findByText('Association, not causation', {}, { timeout: 5000 })).toBeInTheDocument()
    expect(screen.getByText('CPU ↔ Latency')).toBeInTheDocument()
    expect(screen.getByText('strong positive')).toBeInTheDocument()
    expect(screen.getByText('CPU leads by 1.0 h')).toBeInTheDocument()
    expect(screen.getByText('not reliable')).toBeInTheDocument()
    expect(screen.getByText('93% inside the 90% interval')).toBeInTheDocument()
    expect(screen.getByText('Beats the seasonal baseline by 40%')).toBeInTheDocument()
    expect(screen.getByText('regressed')).toBeInTheDocument()
    expect(screen.getByText('A rollout coincides with a usage shift')).toBeInTheDocument()
    expect(screen.getByText('Safety warnings')).toBeInTheDocument()
    expect(screen.getByText(/Set KUBERIGHT_REQUEST_RATE_QUERY/)).toBeInTheDocument()
  })

  it('asks for a container before loading anything', () => {
    const client = new QueryClient()
    render(<QueryClientProvider client={client}><MemoryRouter><Correlation title="Correlation analysis" description="d" /></MemoryRouter></QueryClientProvider>)
    expect(screen.getByText(/Choose a workload container/)).toBeInTheDocument()
  })
})

describe('AssessmentPanel', () => {
  it('states the verdict, the failed check, quantities, and the evidence', () => {
    render(<AssessmentPanel assessment={assessment} />)
    expect(screen.getByText(/Hold · risk 52\/100 · valid for 12h0m0s/)).toBeInTheDocument()
    expect(screen.getByText('ResourceQuota team · Fail')).toBeInTheDocument()
    expect(screen.getByText('Scheduling · Unknown')).toBeInTheDocument()
    expect(screen.getByText('800m')).toBeInTheDocument()
    expect(screen.getByText('CPU margin 45%')).toBeInTheDocument()
    expect(screen.getByText(/91% held-out coverage/)).toBeInTheDocument()
    expect(screen.getByText(/HPA: average utilization/)).toBeInTheDocument()
    expect(screen.getByText(/held on the 600 later samples/)).toBeInTheDocument()
  })
})
