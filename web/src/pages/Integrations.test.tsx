import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { vi } from 'vitest'
import { App } from '../App'
import { Integrations } from './Integrations'

function respond(body: unknown) {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, status: 200, headers: new Headers(), json: async () => body }))
}

function renderWithClient(ui: React.ReactElement, path = '/') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[path]}>{ui}</MemoryRouter></QueryClientProvider>)
}

describe('Integrations metric coverage', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows missing and unknown metrics as such, never as zero usage', async () => {
    respond({
      items: [{ name: 'Prometheus', configured: true, healthy: true, critical: false, detail: 'usage history is queryable' }],
      pagination: { page: 1, pageSize: 1, total: 1 },
      status: { available: true, source: 'runtime_configuration' },
      metricCoverage: [
        { signal: 'CPU usage', metric: 'container_cpu_usage_seconds_total', source: 'cAdvisor (kubelet)', state: 'available', series: 1412, usedFor: 'CPU recommendations' },
        { signal: 'CPU throttling', metric: 'container_cpu_cfs_throttled_periods_total', source: 'cAdvisor (kubelet)', state: 'unavailable', series: 0, usedFor: 'throttling evidence' },
        { signal: 'Node conditions', metric: 'kube_node_status_condition', source: 'kube-state-metrics', state: 'unknown', series: 0, usedFor: 'node pressure', note: 'node series carry no namespace' },
      ],
    })
    renderWithClient(<Integrations title="Integrations" description="Live health" />)
    expect(await screen.findByRole('heading', { name: 'Prometheus metric coverage' })).toBeInTheDocument()
    expect(screen.getByText('1,412 series')).toBeInTheDocument()
    expect(screen.getByText('Missing')).toBeInTheDocument()
    expect(screen.getByText('Unknown')).toBeInTheDocument()
    expect(screen.getByText(/node series carry no namespace/)).toBeInTheDocument()
    expect(screen.queryByText('0 series')).not.toBeInTheDocument()
  })

  it('omits the coverage card when Prometheus is not configured', async () => {
    respond({
      items: [{ name: 'Prometheus', configured: false, healthy: false, critical: false, detail: 'not configured' }],
      pagination: { page: 1, pageSize: 1, total: 1 },
      status: { available: true, source: 'runtime_configuration' },
    })
    renderWithClient(<Integrations title="Integrations" description="Live health" />)
    expect(await screen.findByText('not configured')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Prometheus metric coverage' })).not.toBeInTheDocument()
  })
})

describe('landing view', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('opens on CPU recommendations', async () => {
    respond({ items: [], pagination: { page: 1, pageSize: 25, total: 0 }, status: { available: true, source: 'memory' } })
    renderWithClient(<Routes><Route path="*" element={<App />} /></Routes>, '/')
    expect(await screen.findByRole('heading', { level: 1, name: 'CPU recommendations' })).toBeInTheDocument()
  })
})
