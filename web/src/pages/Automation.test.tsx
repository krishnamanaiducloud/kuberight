import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { vi } from 'vitest'
import type { AutomationReport } from '../api'
import { Automation } from './Automation'

const resources = { cpuRequestMilli: 1500, cpuLimitMilli: 2500, memoryRequestBytes: 3 * 1024 ** 3, memoryLimitBytes: 4 * 1024 ** 3 }
const report = (enabled: boolean): AutomationReport => ({
  mode: enabled ? 'apply' : 'recommend', enabled, dryRun: true, windowOpen: true, maxChangesPerRun: 5, evaluatedAt: '2026-11-05T10:00:00Z',
  evaluated: 2, eligible: 1, applied: 0, rolledBack: 0, deferred: 0, failed: 0,
  decisions: [
    { recommendationId: 'rec_a', namespace: 'shop', kind: 'Deployment', workload: 'checkout', container: 'api', action: 'apply', outcome: 'planned', reasons: [], current: resources, desired: { ...resources, cpuRequestMilli: 750 } },
    { recommendationId: 'rec_b', namespace: 'payments', kind: 'Deployment', workload: 'ledger', container: 'app', action: 'skip', reasons: ['Argo CD manages the workload and would revert the change'], current: resources, desired: resources },
  ],
})

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<QueryClientProvider client={client}><MemoryRouter><Automation title="Automation" description="d" /></MemoryRouter></QueryClientProvider>)
}

describe('Automation', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the plan and every withheld reason when automation is off', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, status: 200, headers: new Headers(), json: async () => report(false) }))
    renderPage()
    expect(await screen.findByText('Automation is off', {}, { timeout: 5000 })).toBeInTheDocument()
    expect(screen.getByLabelText('Changes')).toHaveTextContent('checkout / api')
    expect(screen.getByLabelText('Changes')).toHaveTextContent('→ 750m / 2.5 cores · 3.0 GiB / 4.0 GiB')
    expect(screen.getByLabelText('Withheld')).toHaveTextContent('Argo CD manages the workload')
    expect(screen.queryByRole('button', { name: /Run now/ })).not.toBeInTheDocument()
  })

  it('requires confirmation before running in apply mode', async () => {
    const fetch = vi.fn().mockImplementation(async (_url: string, init?: RequestInit) => ({
      ok: true, status: 200, headers: new Headers(),
      json: async () => (init?.method === 'POST' ? { ...report(true), dryRun: false, applied: 1, decisions: [{ ...report(true).decisions[0], outcome: 'applied' }] } : report(true)),
    }))
    vi.stubGlobal('fetch', fetch)
    renderPage()
    fireEvent.click(await screen.findByRole('button', { name: /Run now/ }, { timeout: 5000 }))
    expect(screen.getByText('Apply 1 changes?')).toBeInTheDocument()
    expect(fetch.mock.calls.some(([, init]) => init?.method === 'POST')).toBe(false)
    fireEvent.click(screen.getByRole('button', { name: 'Apply' }))
    await waitFor(() => expect(fetch.mock.calls.some(([url, init]) => String(url).endsWith('/api/v1/automation:run') && init?.method === 'POST')).toBe(true))
    expect(await screen.findByText('Applied', { selector: 'span' }, { timeout: 5000 })).toBeInTheDocument()
  })
})
