import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { vi } from 'vitest'
import { DataPage } from './DataPage'
import { navigation } from '../navigation'

describe('DataPage', () => {
  it('shows an honest empty state instead of sample data', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: true,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({ items: [], total: 0, status: { available: true, source: 'kubernetes', asOf: '2026-10-17T12:00:00Z' } }),
    }))
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(<QueryClientProvider client={client}><DataPage title="Clusters" description="Cluster inventory" endpoint="/api/v1/clusters" /></QueryClientProvider>)
    expect(await screen.findByText('No verified records')).toBeInTheDocument()
    expect(screen.getByText(/will not substitute demo data/i)).toBeInTheDocument()
    vi.unstubAllGlobals()
  })

  it('uses a recommendation sort accepted by the API contract', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({ items: [], pagination: { page: 1, pageSize: 25, total: 0 }, status: { available: true, source: 'in_process_history' } }),
    })
    vi.stubGlobal('fetch', fetchMock)
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(<QueryClientProvider client={client}><DataPage title="CPU recommendations" description="CPU" endpoint="/api/v1/recommendations" /></QueryClientProvider>)
    expect(await screen.findByText('No verified records')).toBeInTheDocument()
    expect(fetchMock.mock.calls[0][0]).toContain('sort=createdAt')
    expect(fetchMock.mock.calls[0][0]).not.toContain('sort=name')
    vi.unstubAllGlobals()
  })

  it('shows CPU resource values on the CPU recommendation view', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: true,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({
        items: [{
          id: 'rec_0123456789abcdef0123456789abcdef', namespace: 'shop', workload: 'api', container: 'app',
          currentCpuRequestMilli: 500, cpuRequestMilli: 250, currentCpuLimitMilli: 1000, cpuLimitMilli: 500,
          currentMemoryRequestBytes: 536870912, memoryRequestBytes: 402653184,
          currentMemoryLimitBytes: 1073741824, memoryLimitBytes: 805306368,
          confidence: 'high', risk: 'low', createdAt: '2026-10-19T12:00:00Z',
        }],
        pagination: { page: 1, pageSize: 25, total: 1 },
        status: { available: true, source: 'in_process_history' },
      }),
    }))
    const cpu = navigation.find((item) => item.path === '/recommendations/cpu')!
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(<QueryClientProvider client={client}><DataPage title={cpu.label} description={cpu.description} endpoint={cpu.endpoint!} columnKeys={cpu.columnKeys} /></QueryClientProvider>)
    expect(await screen.findByRole('columnheader', { name: /^cpu request milli$/i })).toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: /^current cpu request milli$/i })).toBeInTheDocument()
    expect(screen.queryByRole('columnheader', { name: /memory request bytes/i })).not.toBeInTheDocument()
    expect(screen.getByText('low').closest('.MuiChip-root')).toHaveClass('MuiChip-colorSuccess')
    vi.unstubAllGlobals()
  })

  it('uses an honest unavailable savings endpoint', () => {
    expect(navigation.find((item) => item.path === '/savings')?.endpoint).toBe('/api/v1/savings')
    expect(navigation.find((item) => item.path === '/recommendations/memory')?.columnKeys).toContain('memoryRequestBytes')
  })
})
