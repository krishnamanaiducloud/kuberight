import { fireEvent, render, screen } from '@testing-library/react'
import { vi } from 'vitest'
import type { Policy, PolicyProfile } from '../api'
import { PolicyTuner } from './PolicyTuner'

const policy: Policy = {
  cpuSafetyMargin: 0.3, memorySafetyMargin: 0.25, limitSafetyMargin: 0.5, minimumCpuMilli: 50, minimumMemoryBytes: 67108864,
  maximumCpuMilli: 1e9, maximumMemoryBytes: 1e18, maximumReduction: 0.2, minimumSamples: 30, highConfidenceSamples: 500,
  cpuPercentile: 0.95, memoryPercentile: 0.99,
}
const profiles: PolicyProfile[] = [
  { name: 'conservative', description: 'Careful.', default: false, policy: { ...policy, cpuPercentile: 0.99, maximumReduction: 0.1 } },
  { name: 'balanced', description: 'The default.', default: true, policy },
  { name: 'aggressive', description: 'Lean.', default: false, policy: { ...policy, cpuPercentile: 0.9, maximumReduction: 0.4 } },
]

describe('PolicyTuner', () => {
  it('switches profiles without marking the policy custom', () => {
    const onChange = vi.fn()
    render(<PolicyTuner profiles={profiles} profile="balanced" policy={policy} customized={false} onChange={onChange} />)
    expect(screen.getByText('The default.')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'aggressive' }))
    expect(onChange).toHaveBeenCalledWith('aggressive', profiles[2].policy, false)
  })

  it('marks fine-tuned policies as custom', () => {
    const onChange = vi.fn()
    render(<PolicyTuner profiles={profiles} profile="balanced" policy={policy} customized onChange={onChange} />)
    expect(screen.getByText('Custom policy based on balanced.')).toBeInTheDocument()
    fireEvent.click(screen.getByText('Fine-tune'))
    fireEvent.keyDown(screen.getByRole('slider', { name: 'Maximum reduction per change' }), { key: 'ArrowRight' })
    expect(onChange).toHaveBeenCalledWith('balanced', { ...policy, maximumReduction: 0.25 }, true)
  })
})
