import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { vi } from 'vitest'
import { Savings } from './Savings'

describe('Savings lifecycle', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('separates modeled lifecycle stages from unavailable realized savings', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      headers: new Headers(),
      json: async () => ({
        items: [{
          id: 'rec_1', namespace: 'shop', workload: 'checkout', container: 'api', replicas: 2,
          reviewStatus: 'approved', stage: 'verified', stale: false, approvable: true, costAvailable: true,
          currentMonthlyCost: 30, recommendedMonthlyCost: 17.66, theoreticalMonthlyChange: 12.34,
          verificationStatus: 'verified', configurationObserved: true, evidenceComplete: true,
          currency: 'USD', createdAt: '2026-10-15T00:00:00Z',
        }],
        pagination: { page: 1, pageSize: 25, total: 1 },
        status: { available: true, source: 'configured_unit_prices' },
        summary: {
          theoreticalMonthlyChange: 20,
          recommendedMonthlyChange: 18,
          approvedMonthlyChange: 15,
          externallyObservedMonthlyChange: 12.34,
          verifiedMonthlyChange: 12.34,
          estimatedMonthlySavings: 18,
          approvedMonthlySavings: 15,
          currency: 'USD', recommendations: 1, pricedRecommendations: 1,
          truncated: false, verificationOutcomesTruncated: false,
          basis: 'recorded requests and replicas',
          realized: { available: false, reason: 'Billing allocation and a counterfactual baseline are not configured.' },
        },
      }),
    }))
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <Savings title="Savings lifecycle" description="d" />
        </MemoryRouter>
      </QueryClientProvider>,
    )

    expect(await screen.findByText(/Realized infrastructure savings are unavailable/i, {}, { timeout: 5000 })).toBeInTheDocument()
    expect(screen.getByText(/Billing allocation and a counterfactual baseline/)).toBeInTheDocument()
    expect(screen.getByText(/Values are a lifecycle funnel, not additive totals/)).toBeInTheDocument()
    expect(screen.getAllByText('Verified').length).toBeGreaterThanOrEqual(2)
    expect(screen.getAllByText('$12.34').length).toBeGreaterThanOrEqual(2)
    expect(screen.queryByText(/realized monthly savings: \$12.34/i)).not.toBeInTheDocument()
  })
})
