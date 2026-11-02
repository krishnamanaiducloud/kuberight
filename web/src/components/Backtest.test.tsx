import { render, screen } from '@testing-library/react'
import { BacktestTable, CpuBehaviorChips, TrendChips } from './Backtest'

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
    // Without nearLimit values every limit-pressure cell is undefined.
    expect(screen.getAllByText('—')).toHaveLength(4)
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

describe('CpuBehaviorChips', () => {
  it('flags peak throttling, pinned limits, and recurring spikes', () => {
    render(<CpuBehaviorChips throttling={0.02} throttlingPeak={0.41} pinned spikes={{ classification: 'recurring', count: 4, spikesPerDay: 1, medianDurationMinutes: 15, peakToMedian: 4.2, threshold: 500, recurringHourUtc: 9 }} />)
    expect(screen.getByText('CPU throttled 2.0% · peak 41.0%').closest('.MuiChip-root')).toHaveClass('MuiChip-colorWarning')
    expect(screen.getByText('Pinned at CPU limit')).toBeInTheDocument()
    expect(screen.getByText('Spikes recur daily ~09:00 UTC, 4.2× median')).toBeInTheDocument()
  })

  it('renders nothing without throttling or spike data', () => {
    const { container } = render(<CpuBehaviorChips spikes={{ classification: 'none', count: 0, spikesPerDay: 0, medianDurationMinutes: 0, peakToMedian: 0, threshold: 0 }} />)
    expect(container).toBeEmptyDOMElement()
  })
})

describe('BacktestTable limit pressure', () => {
  it('reports near-limit share and the memory OOM line', () => {
    render(<BacktestTable backtest={{
      cpuCurrent: { overRequest: 0.5, nearLimit: 0.12 },
      cpuRecommended: { overRequest: 0.05, nearLimit: 0 },
      memoryCurrent: { overRequest: 0.1, overLimit: 0, nearLimit: 0.02 },
      memoryRecommended: { overRequest: 0.01, overLimit: 0, nearLimit: 0 },
    }} />)
    expect(screen.getByText('12.0%')).toBeInTheDocument()
    expect(screen.getByText(/would be OOM-killed\): 0%/)).toBeInTheDocument()
  })
})
