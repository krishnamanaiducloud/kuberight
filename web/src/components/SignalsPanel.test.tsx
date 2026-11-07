import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { vi } from 'vitest'
import { SignalsPanel } from './SignalsPanel'

const start = Date.UTC(2026, 9, 1)
const series = (resource: string, value: (index: number) => number) => ({
  resource, unit: 'x', quality: { issues: [], coverage: 1, expectedSamples: 12, acceptedSamples: 12 },
  points: Array.from({ length: 12 }, (_, index) => ({ timestamp: new Date(start + index * 300_000).toISOString(), value: value(index) })),
})

describe('SignalsPanel', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows insights, correlations, markers, and an event table', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: true, status: 200, headers: new Headers(),
      json: async () => ({
        status: { available: true, source: 'prometheus' },
        signals: {
          start: new Date(start).toISOString(), end: new Date(start + 3_600_000).toISOString(), step: '5m0s',
          cpu: series('cpu', (index) => 200 + index * 10), memory: series('memory', () => 3e8), throttling: series('throttling', (index) => index * 0.01),
          events: [{ timestamp: new Date(start + 1_800_000).toISOString(), kind: 'oom_kill', count: 2 }], warnings: [],
        },
        correlations: { cpuThrottling: 0.97 },
        insights: [{ severity: 'critical', title: 'Containers were OOM-killed', detail: '2 OOM kill(s) in the window.' }],
        latencyConfigured: false,
      }),
    }))
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const { container } = render(<QueryClientProvider client={client}><SignalsPanel subject={{ namespace: 'shop', kind: 'Deployment', workload: 'api', container: 'app' }} usageWindow="1h" /></QueryClientProvider>)
    expect(await screen.findByText('Containers were OOM-killed', {}, { timeout: 5000 })).toBeInTheDocument()
    expect(screen.getByText('CPU ↔ throttling: r = 0.97')).toBeInTheDocument()
    expect(screen.getByText('OOM kill ×2')).toBeInTheDocument()
    expect(screen.getAllByText('CPU throttling').length).toBeGreaterThan(0)
    // One marker per chart: CPU, throttling, and memory.
    expect(container.querySelectorAll('circle[fill="#d03b3b"]')).toHaveLength(3)
    expect(screen.getByText(/Set KUBERIGHT_LATENCY_QUERY/)).toBeInTheDocument()
  })
})
