import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { vi } from 'vitest'
import { Policies } from './Policies'
import { scopeLabel } from '../format'
import { chooseOption, namespacePage } from '../test/autocomplete'

const policy = {
  cpuSafetyMargin: 0.3, memorySafetyMargin: 0.25, limitSafetyMargin: 0.5, minimumCpuMilli: 50, minimumMemoryBytes: 67108864,
  maximumCpuMilli: 1000000, maximumMemoryBytes: 1099511627776, maximumReduction: 0.2, minimumSamples: 30, highConfidenceSamples: 500,
  cpuPercentile: 0.95, memoryPercentile: 0.99, cooldownHours: 24, requiredObservations: 1, minimumChangeRatio: 0.05,
}
const record = {
  id: 'pol_00000000000000000000000000000001', name: 'shop-safe', builtIn: false, description: 'Protect checkout', priority: 10, enabled: true, version: 3, policy,
  scope: { level: 'workload', namespace: 'shop', kind: 'Deployment', workload: 'checkout' }, createdAt: '2026-11-13T10:00:00Z', createdBy: 'alice', updatedAt: '2026-11-13T11:00:00Z', updatedBy: 'bob',
}
const list = { items: [{ name: 'balanced', builtIn: true, default: true, description: 'The default.', policy }, record] }

function renderPage(fetch: ReturnType<typeof vi.fn>) {
  vi.stubGlobal('fetch', fetch)
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<QueryClientProvider client={client}><MemoryRouter><Policies title="Policies" description="d" /></MemoryRouter></QueryClientProvider>)
}

const json = (body: unknown, status = 200) => ({ ok: status < 400, status, headers: new Headers(), json: async () => body })

// Namespace and workload choices come from live inventory.
function inventory(url: string) {
  if (url.includes('/api/v1/namespaces')) return json(namespacePage('payments', 'shop'))
  if (url.includes('/api/v1/workloads')) {
    return json({
      items: [{ id: 'shop/Deployment/checkout', namespace: 'shop', kind: 'Deployment', name: 'checkout', containers: [{ name: 'api' }, { name: 'proxy' }] }],
      pagination: { page: 1, pageSize: 200, total: 1 }, status: { available: true },
    })
  }
  return undefined
}

// Choosing from live-inventory dropdowns takes several seconds in jsdom.
const dropdownTimeout = 15_000

describe('Policies', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('lists policies with their scope and resolves the effective policy', async () => {
    const fetch = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (String(url).endsWith('/api/v1/policies:resolve')) {
        expect(JSON.parse(String(init?.body))).toEqual({ namespace: 'shop', kind: 'Deployment', workload: 'checkout', container: 'api' })
        return json({ source: 'persistent', matchedPolicy: record, policy, candidates: [record, { ...record, name: 'shop-wide' }], ambiguous: false })
      }
      return inventory(String(url)) ?? json(list)
    })
    renderPage(fetch)
    const table = await screen.findByLabelText('Policies', {}, { timeout: 5000 })
    expect(table).toHaveTextContent('shop-safe')
    expect(table).toHaveTextContent('shop / Deployment/checkout')
    expect(table).toHaveTextContent('v3 · bob')
    const resolve = screen.getByRole('button', { name: 'Resolve' })
    expect(resolve).toBeDisabled()
    await chooseOption(screen.getByRole('combobox', { name: 'Namespace' }), 'shop')
    // Selecting the workload also selects its first container.
    await chooseOption(screen.getByRole('combobox', { name: 'Workload' }), 'shop / Deployment / checkout', 'checkout')
    await waitFor(() => expect(resolve).toBeEnabled())
    fireEvent.click(resolve)
    const resolution = await screen.findByLabelText('Resolution', {}, { timeout: 5000 })
    expect(resolution).toHaveTextContent('Applies: shop-safe')
    expect(resolution).toHaveTextContent('Also applicable, in order: shop-wide')
  }, dropdownTimeout)

  it('shows an ambiguity conflict from the API when creating a policy', async () => {
    const fetch = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (init?.method === 'POST' && String(url).endsWith('/api/v1/policies')) {
        return json({ error: { code: 'policy_ambiguous', message: 'another enabled policy has the same scope and priority; change the priority so precedence is explicit' } }, 409)
      }
      return inventory(String(url)) ?? json(list)
    })
    renderPage(fetch)
    // The button enables once the built-in profiles have loaded.
    await screen.findByLabelText('Policies', {}, { timeout: 5000 })
    fireEvent.click(screen.getByRole('button', { name: /New policy/ }))
    const dialog = await screen.findByRole('dialog', {}, { timeout: 5000 })
    expect(within(dialog).getByRole('button', { name: 'Create' })).toBeDisabled()
    fireEvent.change(within(dialog).getByRole('textbox', { name: 'Name' }), { target: { value: 'duplicate' } })
    await chooseOption(within(dialog).getByRole('combobox', { name: 'Namespace' }), 'shop')
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create' }))
    await waitFor(() => expect(within(dialog).getByText(/same scope and priority/)).toBeInTheDocument(), { timeout: 5000 })
  }, dropdownTimeout)

  it('describes scopes', () => {
    expect(scopeLabel({ level: 'global' })).toBe('All namespaces')
    expect(scopeLabel({ level: 'namespace', namespace: 'shop', cluster: 'prod' })).toBe('shop · cluster prod')
    expect(scopeLabel({ level: 'container', namespace: 'shop', kind: 'Deployment', workload: 'api', container: 'app' })).toBe('shop / Deployment/api / app')
  })
})
