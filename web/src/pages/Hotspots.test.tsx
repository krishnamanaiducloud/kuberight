import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { vi } from 'vitest'
import { Hotspots } from './Hotspots'

describe('Hotspots', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('ranks containers with severity labels and advice', async () => {
    vi.stubGlobal('fetch', vi.fn().mockImplementation(async (url: string) => ({
      ok: true, status: 200, headers: new Headers(),
      json: async () => url.includes('/namespaces')
        ? { items: [{ name: 'shop' }], status: { available: true } }
        : {
          items: [{
            namespace: 'shop', kind: 'Deployment', workload: 'api', container: 'app', pods: 2,
            throttlingRatio: 0.08, throttlingPeakRatio: 0.6, spikeRatio: 2, cpuPeakMilli: 990, cpuRequestMilli: 500, cpuLimitMilli: 1000,
            pinnedAtLimit: true, severity: 'critical', issues: ['throttled in 8.0% of periods overall and 60.0% in peak intervals'],
            advice: 'Raise or remove the CPU limit before any CPU reduction.',
          }],
          summary: { throttled: 1, pinned: 1, spiky: 0, window: '24h0m0s', podsUnmatched: 0 },
          status: { available: true, source: 'kubernetes-api+prometheus' },
        },
    })))
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/?namespace=shop']}><Hotspots title="CPU hotspots" description="d" /></MemoryRouter></QueryClientProvider>)
    expect(await screen.findByText('Critical', {}, { timeout: 5000 })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'api' })).toHaveAttribute('href', '/workloads/shop/Deployment/api')
    expect(screen.getByText('60.0%')).toBeInTheDocument()
    expect(screen.getByText('pinned')).toBeInTheDocument()
    expect(screen.getByText(/Raise or remove the CPU limit/)).toBeInTheDocument()
  })
})
