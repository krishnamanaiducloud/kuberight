import type { RecommendationRecord } from './api'

/** What one container's analysis concluded. A skipped analysis is never a failure of the workload. */
export type AnalysisOutcome =
  | 'created' | 'no_change' | 'held' | 'insufficient_history' | 'missing_metrics'
  | 'incomplete_attribution' | 'provider_unavailable' | 'unsupported' | 'failed'

export const analysisOutcomes: Record<AnalysisOutcome, { label: string; color: 'success' | 'info' | 'warning' | 'error' | 'default' }> = {
  created: { label: 'Recommendation created', color: 'success' },
  no_change: { label: 'Already right-sized', color: 'success' },
  held: { label: 'Held: no safe change yet', color: 'info' },
  insufficient_history: { label: 'Insufficient usage history', color: 'info' },
  missing_metrics: { label: 'Missing metrics', color: 'warning' },
  incomplete_attribution: { label: 'Incomplete workload attribution', color: 'warning' },
  provider_unavailable: { label: 'Provider unavailable', color: 'error' },
  unsupported: { label: 'Unsupported workload', color: 'default' },
  failed: { label: 'Failed', color: 'error' },
}

const errorOutcomes: Record<string, AnalysisOutcome> = {
  metric_attribution_incomplete: 'incomplete_attribution',
  no_usage_data: 'missing_metrics',
  insufficient_history: 'insufficient_history',
  dependency_unavailable: 'provider_unavailable',
  dependency_timeout: 'provider_unavailable',
  integration_not_configured: 'provider_unavailable',
  unsupported_container: 'unsupported',
  workload_out_of_scope: 'unsupported',
}

const statusOutcomes: Record<string, AnalysisOutcome> = {
  recommend: 'created', no_change: 'no_change', hold: 'held', insufficient_data: 'insufficient_history', unsupported: 'unsupported',
}

export function outcomeOf(target: { state: string; code?: string; record?: RecommendationRecord }): AnalysisOutcome | undefined {
  if (target.state === 'failed') return (target.code && errorOutcomes[target.code]) || 'failed'
  if (target.state !== 'done') return undefined
  return statusOutcomes[target.record?.assessment?.status ?? 'recommend'] ?? 'created'
}
