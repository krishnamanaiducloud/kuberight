import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { vi } from 'vitest'
import { Fleet } from './Fleet'

describe('Fleet', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows durable scan progress and queues a namespace-scoped scan', async () => {
    const fetchMock = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (init?.method === 'POST') {
        return { ok: true, status: 202, headers: new Headers(), json: async () => ({ id: 'fsn_created', status: 'queued' }) }
      }
      if (url.includes('/fleet/schedules')) {
        return {
          ok: true, status: 200, headers: new Headers(),
          json: async () => ({
            items: [{ id: 'fsc_one', name: 'nightly evidence', enabled: true, intervalSeconds: 86_400, scope: { namespaces: ['shop'] }, windowSeconds: 604_800, nextRunAt: '2026-11-04T02:00:00Z', version: 2 }],
            pagination: { page: 1, pageSize: 100, total: 1 }, status: { available: true },
          }),
        }
      }
      return {
        ok: true, status: 200, headers: new Headers(),
        json: async () => ({
          items: [{ id: 'fsn_one', scheduledFor: '2026-11-03T02:00:00Z', scope: { namespaces: ['shop'] }, windowSeconds: 604_800, status: 'running', expected: 10, processed: 6, succeeded: 5, failed: 1, attempt: 2, maxAttempts: 3, lastError: 'one target unavailable' }],
          pagination: { page: 1, pageSize: 100, total: 1 }, status: { available: true },
        }),
      }
    })
    vi.stubGlobal('fetch', fetchMock)
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })
    render(<QueryClientProvider client={client}><MemoryRouter><Fleet title="Fleet operations" description="Continuous evidence scans" /></MemoryRouter></QueryClientProvider>)

    expect(await screen.findByText('nightly evidence')).toBeInTheDocument()
    expect(screen.getByText('6/10 · 5 succeeded · 1 failed')).toBeInTheDocument()
    expect(screen.getByText('one target unavailable')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'New scan' }))
    fireEvent.change(screen.getByLabelText('Namespace (optional)'), { target: { value: 'payments' } })
    fireEvent.click(screen.getByRole('button', { name: 'Queue scan' }))

    await waitFor(() => expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'POST')).toBe(true))
    const call = fetchMock.mock.calls.find(([, init]) => init?.method === 'POST')!
    expect(call[0]).toBe('/api/v1/fleet/scans')
    expect(JSON.parse(call[1].body)).toMatchObject({ scope: { namespaces: ['payments'] }, windowSeconds: 604_800, maxAttempts: 3 })
    expect(new Headers(call[1].headers).get('Idempotency-Key')).toMatch(/^ui-/)
  })
})
