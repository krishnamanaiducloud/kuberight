import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { vi } from 'vitest'
import { RecommendationDrawer } from './RecommendationDrawer'

const record = {
  id: 'rec_0123456789abcdef0123456789abcdef', cluster: 'prod', namespace: 'shop', kind: 'Deployment', workload: 'checkout', container: 'api', replicas: 3,
  cpuRequestMilli: 800, memoryRequestBytes: 805306368, cpuLimitMilli: 1600, memoryLimitBytes: 0,
  currentCpuRequestMilli: 1000, currentMemoryRequestBytes: 1073741824, currentCpuLimitMilli: 2000, currentMemoryLimitBytes: 0,
  cpuStatisticsMilli: { p50: 300, p90: 320, p95: 330, p99: 340, max: 350 },
  memoryStatisticsBytes: { p50: 4e8, p90: 4e8, p95: 4e8, p99: 4e8, max: 4e8 },
  confidence: 'high', confidenceScore: 0.82, risk: 'low', burstable: false,
  dataQuality: { cpuSampleCount: 600, memorySampleCount: 600, minimumRequired: 30, sufficient: true, issues: [] },
  reason: 'P95 CPU and P99 memory', warnings: ['memory limit is absent and no new limit was introduced'], applied: false,
  createdAt: '2026-10-19T12:00:00Z', createdBy: 'alice', source: 'prometheus', reviewStatus: 'pending',
  provenance: {
    workloadUid: 'uid-checkout', workloadGeneration: 3, workloadResourceVersion: '88', sourceSpecFingerprint: 'a'.repeat(64),
    queryFingerprint: 'b'.repeat(64), policyFingerprint: 'c'.repeat(64), optimizerVersion: '2.0.0',
    observationStart: '2026-10-12T12:00:00Z', observationEnd: '2026-10-19T12:00:00Z', expiresAt: '2099-10-20T12:00:00Z',
  },
}

function respond(body: unknown, status = 200) {
  return { ok: status < 400, status, headers: new Headers(), json: async () => body }
}

function renderDrawer() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={client}><RecommendationDrawer id={record.id} onClose={() => {}} /></QueryClientProvider>)
}

describe('RecommendationDrawer', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('explains the recommendation and submits an approval', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(respond(record))
      .mockResolvedValueOnce(respond({ ...record, reviewStatus: 'approved', reviewedBy: 'bob', reviewedAt: '2026-10-20T12:00:00Z' }))
    vi.stubGlobal('fetch', fetchMock)
    renderDrawer()
    expect(await screen.findByText('checkout / api', {}, { timeout: 5000 })).toBeInTheDocument()
    expect(screen.getAllByText('-20.0%')).toHaveLength(2)
    expect(screen.getByText('-25.0%')).toBeInTheDocument()
    expect(screen.getByText(/memory limit is absent/)).toBeInTheDocument()
    expect(screen.getByText('Evidence valid')).toBeInTheDocument()
    // Rejection requires a comment.
    expect(screen.getByRole('button', { name: 'Reject' })).toBeDisabled()

    fireEvent.click(screen.getByRole('button', { name: 'Approve' }))
    expect(await screen.findByText(/Approved by/, {}, { timeout: 5000 })).toBeInTheDocument()
    const [url, init] = fetchMock.mock.calls[1]
    expect(url).toBe(`/api/v1/recommendations/${record.id}/review`)
    expect(JSON.parse(init.body)).toEqual({ decision: 'approve', comment: '' })
    expect(screen.getByText(/kubectl -n shop set resources deployment\/checkout -c api --requests=cpu=800m,memory=768Mi --limits=cpu=1600m$/)).toBeInTheDocument()
  })

  it('surfaces a separation-of-duties refusal', async () => {
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(respond(record))
      .mockResolvedValueOnce(respond({ error: { code: 'separation_of_duties', message: 'a recommendation must be reviewed by an identity other than the one that created it' } }, 403)))
    renderDrawer()
    fireEvent.click(await screen.findByRole('button', { name: 'Approve' }, { timeout: 5000 }))
    await waitFor(() => expect(screen.getByText(/reviewed by an identity other than/)).toBeInTheDocument(), { timeout: 5000 })
  })

  it('disables approval when Prometheus evidence has expired', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(respond({
      ...record,
      provenance: { ...record.provenance, expiresAt: '2020-01-01T00:00:00Z' },
    })))
    renderDrawer()
    expect(await screen.findByText('Evidence expired', {}, { timeout: 5000 })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Approve' })).toBeDisabled()
    expect(screen.getByText(/Approval is disabled/)).toBeInTheDocument()
  })
})
