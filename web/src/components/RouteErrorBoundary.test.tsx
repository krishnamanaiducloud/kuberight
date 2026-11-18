import { render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { RouteErrorBoundary } from './RouteErrorBoundary'

function Broken(): never {
  throw new Error('decisions is undefined')
}

describe('RouteErrorBoundary', () => {
  afterEach(() => vi.restoreAllMocks())

  it('contains a failing view and recovers on the next route', () => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
    const { rerender } = render(<RouteErrorBoundary resetKey="/automation"><Broken /></RouteErrorBoundary>)
    expect(screen.getByRole('heading', { level: 1, name: 'This view could not be displayed' })).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('decisions is undefined')

    rerender(<RouteErrorBoundary resetKey="/savings"><p>Savings</p></RouteErrorBoundary>)
    expect(screen.getByText('Savings')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })
})
