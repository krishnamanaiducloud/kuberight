import { render, screen } from '@testing-library/react'
import { StabilityPanel } from './StabilityPanel'
import type { Stability } from '../api'

const base: Stability = {
  minimumChangeRatio: 0.05, suppressedChanges: [], cooldownHours: 24, cooldownActive: false, directionChanges: 0, oscillating: false,
  requiredObservations: 1, consistentObservations: 1, reductionsHeld: [], changed: true,
}

describe('stability panel', () => {
  it('shows quiet controls when nothing was held', () => {
    render(<StabilityPanel stability={base} />)
    const controls = screen.getByLabelText('Stability controls')
    expect(controls).toHaveTextContent('Minimum change 5%')
    expect(controls).toHaveTextContent('Cooldown 24h · not active')
    expect(controls).toHaveTextContent('No oscillation')
    expect(controls).toHaveTextContent('Observations 1 of 1')
    expect(screen.queryByText(/Reductions held/)).not.toBeInTheDocument()
  })

  it('explains held reductions, the cooldown, and suppressed changes', () => {
    render(<StabilityPanel stability={{
      ...base, cooldownActive: true, cooldownEndsAt: '2026-11-02T10:00:00Z', lastResourceChangeAt: '2026-11-01T10:00:00Z',
      oscillating: true, directionChanges: 2, requiredObservations: 2, consistentObservations: 1, changed: false,
      reductionsHeld: ['CPU: requests or limits changed recently.'], suppressedChanges: ['memory request 1Gi to 1000Mi is below the 5% minimum meaningful change'],
    }} />)
    expect(screen.getByLabelText('Stability controls')).toHaveTextContent(/Cooldown until/)
    expect(screen.getByText(/Oscillating · 2 direction changes/)).toBeInTheDocument()
    expect(screen.getByText('Observations 1 of 2')).toBeInTheDocument()
    expect(screen.getByText(/Reductions held: CPU: requests or limits changed recently./)).toBeInTheDocument()
    expect(screen.getByText(/Not changed: memory request 1Gi to 1000Mi/)).toBeInTheDocument()
    expect(screen.getByText(/Requests or limits last changed/)).toBeInTheDocument()
  })
})
