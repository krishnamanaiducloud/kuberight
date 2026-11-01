import { render, screen } from '@testing-library/react'
import { BacktestTable, TrendChips } from './Backtest'

describe('BacktestTable', () => {
  it('compares current and recommended exceedance and keeps unset settings undefined', () => {
    render(<BacktestTable backtest={{
      cpuCurrent: { overRequest: 0.62, overLimit: 0.0004 },
      cpuRecommended: { overRequest: 0.048, overLimit: 0 },
      memoryCurrent: { overRequest: 0.1 },
      memoryRecommended: { overRequest: 0.01 },
    }} />)
    expect(screen.getByText('62.0%')).toBeInTheDocument()
    expect(screen.getByText('4.8%')).toBeInTheDocument()
    expect(screen.getByText('<0.1%')).toBeInTheDocument()
    expect(screen.getAllByText('—')).toHaveLength(2)
  })
})

describe('TrendChips', () => {
  it('shows only significant trends', () => {
    const flat = { slopePerDay: 0, growthPerDayPercent: 0, rSquared: 0, significant: false, projectedUplift: 0 }
    const { container } = render(<TrendChips trends={{ cpu: flat, memory: flat }} />)
    expect(container).toBeEmptyDOMElement()
    render(<TrendChips trends={{ cpu: flat, memory: { slopePerDay: 2e7, growthPerDayPercent: 6.5, rSquared: 0.93, significant: true, projectedUplift: 1e8 } }} />)
    expect(screen.getByText('Trend: Memory +6.5%/day (R² 0.93)')).toBeInTheDocument()
  })
})
