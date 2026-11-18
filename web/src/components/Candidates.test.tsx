import { render, screen, within } from '@testing-library/react'
import type { Candidate } from '../api'
import { CandidatesTable } from './Candidates'

const replay = { cpuCurrent: {}, cpuRecommended: { overRequest: 0.02 }, memoryCurrent: {}, memoryRecommended: { overRequest: 0 } }
const candidate = (profile: Candidate['profile'], cpu: number, overRequest: number, selected = false): Candidate => ({
  profile, selected, cpuRequestMilli: cpu, memoryRequestBytes: 402_653_184, cpuLimitMilli: 0, memoryLimitBytes: 536_870_912,
  estimatedCpuChangePercent: (cpu / 800 - 1) * 100, estimatedMemoryChangePercent: -50, status: 'recommend', backtest: replay,
  outOfSample: { performed: true, trainSamples: 700, testSamples: 300, cpuOverRequest: overRequest, memoryOverLimit: 0, passed: true, detail: '' },
})

describe('CandidatesTable', () => {
  it('compares each profile and marks the one that produced the recommendation', () => {
    render(<CandidatesTable candidates={[candidate('conservative', 720, 0.004), candidate('balanced', 640, 0.012, true), candidate('aggressive', 480, 0.061)]} />)
    const rows = screen.getAllByRole('row')
    expect(within(rows[1]).getByText('Conservative')).toBeInTheDocument()
    expect(within(rows[1]).getByText('0.4%')).toBeInTheDocument()
    expect(within(rows[2]).getByText('Recommended')).toBeInTheDocument()
    expect(within(rows[3]).getByText('6.1%')).toBeInTheDocument()
    expect(within(rows[3]).getByText(/-40%/)).toBeInTheDocument()
    expect(screen.queryAllByText('Recommended')).toHaveLength(1)
  })

  it('shows the risk of each candidate and why it is held', () => {
    render(<CandidatesTable candidates={[candidate('balanced', 640, 0.012, true), { ...candidate('aggressive', 480, 0.061), status: 'hold', riskScore: 71.6, statusReasons: ['held-out CPU exceeded the request in 6.1% of samples'] }]} />)
    const rows = screen.getAllByRole('row')
    expect(within(rows[2]).getByText('72')).toBeInTheDocument()
    expect(within(rows[2]).getByText('hold')).toBeInTheDocument()
    expect(within(rows[2]).getByText('held-out CPU exceeded the request in 6.1% of samples')).toBeInTheDocument()
    expect(within(rows[1]).getByText('—')).toBeInTheDocument()
  })

  it('renders nothing without candidates', () => {
    const { container } = render(<CandidatesTable candidates={[]} />)
    expect(container).toBeEmptyDOMElement()
  })
})
