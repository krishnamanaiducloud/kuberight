import { render, screen, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { vi } from 'vitest'
import { Overview } from './Overview'

const baseOverview = {
  version: 'test', mode: 'recommend', workloads: 148, clusters: 1, generatedAt: '2026-11-19T10:00:00Z',
  inventory: { available: true, source: 'kubernetes-api' },
  recommendations: { total: 3, pending: 2, approved: 1, rejected: 0, source: 'memory' },
  dependencies: [],
}

function respond(overview: unknown, hotspots: unknown) {
  vi.stubGlobal('fetch', vi.fn().mockImplementation(async (url: string) => ({
    ok: true, status: 200, headers: new Headers(),
    json: async () => (String(url).includes('/api/v1/hotspots') ? hotspots : overview),
  })))
}

function renderOverview() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={client}><MemoryRouter><Overview /></MemoryRouter></QueryClientProvider>)
}

describe('Overview workload health', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('counts observed findings and hotspots', async () => {
    respond({
      ...baseOverview,
      health: {
        available: true, pods: 412, workloadsWithPods: 140, healthyWorkloads: 131, workloadsNeedingAttention: 9, nodesWithProblems: 0,
        findings: { OOMKilled: 3, CrashLoopBackOff: 2, RepeatedRestarts: 4, InsufficientMemory: 1 },
      },
    }, { items: [], pagination: { page: 1, pageSize: 5, total: 5 }, status: { available: true, source: 'hotspots' } })
    renderOverview()
    const region = await screen.findByRole('region', { name: 'Workload health' })
    const card = (label: string) => within(region).getByText(label).closest('a')!
    expect(await within(card('CPU hotspots')).findByText('5')).toBeInTheDocument()
    expect(within(card('Healthy workloads')).getByText('131 of 140')).toBeInTheDocument()
    expect(within(card('Need attention')).getByText('9')).toBeInTheDocument()
    expect(within(card('OOM kills')).getByText('3')).toBeInTheDocument()
    expect(within(card('Restart anomalies')).getByText('6')).toBeInTheDocument()
    expect(within(card('Unschedulable pods')).getByText('1')).toBeInTheDocument()
    expect(card('OOM kills')).toHaveAttribute('href', '/health')
  })

  it('shows unavailable counts as unknown, not zero', async () => {
    respond({ ...baseOverview, health: { available: false, reason: 'pod inventory is unavailable or access was denied', pods: 0, workloadsWithPods: 0, healthyWorkloads: 0, workloadsNeedingAttention: 0, nodesWithProblems: 0, findings: {} } },
      { items: [], pagination: { page: 1, pageSize: 0, total: 0 }, status: { available: false, source: 'hotspots', reason: 'CPU hotspots require the Kubernetes inventory and Prometheus' } })
    renderOverview()
    const region = await screen.findByRole('region', { name: 'Workload health' })
    expect(within(region).queryByText('0')).not.toBeInTheDocument()
    expect(within(region).getAllByText('pod inventory is unavailable or access was denied')).toHaveLength(6)
    expect(await within(region).findByText('CPU hotspots require the Kubernetes inventory and Prometheus')).toBeInTheDocument()
  })
})
