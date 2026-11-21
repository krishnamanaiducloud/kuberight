import { analysisOutcomes, outcomeOf } from './analysisOutcome'
import type { RecommendationRecord } from './api'

const record = (status?: string) => ({ assessment: status ? { status } : undefined }) as unknown as RecommendationRecord

describe('analysis outcomes', () => {
  it('reports a right-sized workload as a success, not a skipped analysis', () => {
    expect(outcomeOf({ state: 'done', record: record('no_change') })).toBe('no_change')
    expect(analysisOutcomes.no_change.color).toBe('success')
    expect(analysisOutcomes.no_change.label).toBe('Already right-sized')
  })

  it('distinguishes every assessment status', () => {
    expect(outcomeOf({ state: 'done', record: record('recommend') })).toBe('created')
    expect(outcomeOf({ state: 'done', record: record('hold') })).toBe('held')
    expect(outcomeOf({ state: 'done', record: record('insufficient_data') })).toBe('insufficient_history')
    expect(outcomeOf({ state: 'done', record: record('unsupported') })).toBe('unsupported')
    // Records stored before the assessment existed were real recommendations.
    expect(outcomeOf({ state: 'done', record: record() })).toBe('created')
  })

  it('maps API error codes to the reason the analysis was skipped', () => {
    expect(outcomeOf({ state: 'failed', code: 'metric_attribution_incomplete' })).toBe('incomplete_attribution')
    expect(outcomeOf({ state: 'failed', code: 'no_usage_data' })).toBe('missing_metrics')
    expect(outcomeOf({ state: 'failed', code: 'insufficient_history' })).toBe('insufficient_history')
    expect(outcomeOf({ state: 'failed', code: 'dependency_unavailable' })).toBe('provider_unavailable')
    expect(outcomeOf({ state: 'failed', code: 'dependency_timeout' })).toBe('provider_unavailable')
    expect(outcomeOf({ state: 'failed', code: 'unsupported_container' })).toBe('unsupported')
    expect(outcomeOf({ state: 'failed', code: 'something_new' })).toBe('failed')
    expect(outcomeOf({ state: 'failed' })).toBe('failed')
    expect(outcomeOf({ state: 'running' })).toBeUndefined()
  })
})
