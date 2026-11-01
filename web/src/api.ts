export type ApiStatus = {
  available: boolean
  reason?: string
  asOf?: string
  source?: string
}

export type ApiRecord = Record<string, unknown>

export type CollectionEnvelope = {
  items: ApiRecord[]
  pagination?: {
    page: number
    pageSize: number
    total: number
  }
  status?: ApiStatus
  requestId?: string
  summary?: { currency?: string }
}

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly requestId?: string,
  ) {
    super(message)
  }
}

export const tokenStore = {
  get: () => sessionStorage.getItem('kuberight.token') ?? '',
  set: (token: string) => {
    if (token) sessionStorage.setItem('kuberight.token', token)
    else sessionStorage.removeItem('kuberight.token')
  },
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers)
  headers.set('Accept', 'application/json')
  if (init?.body) headers.set('Content-Type', 'application/json')
  const token = tokenStore.get()
  if (token) headers.set('Authorization', `Bearer ${token}`)

  const response = await fetch(path, { ...init, headers, credentials: 'same-origin' })
  const requestId = response.headers.get('X-Request-ID') ?? undefined
  const body = await response.json().catch(() => ({})) as Record<string, unknown>
  if (!response.ok) {
    const problem = body.error as Record<string, unknown> | string | undefined
    const message = typeof problem === 'string'
      ? problem
      : String(problem?.message ?? body.message ?? `Request failed (${response.status})`)
    throw new ApiError(message, response.status, requestId)
  }
  return body as T
}

export const api = {
  health: () => request<{ status: string }>('/healthz'),
  get: <T>(path: string, init?: RequestInit) => request<T>(path, init),
  post: <T>(path: string, body: unknown, idempotencyKey?: string, signal?: AbortSignal) => request<T>(path, {
    method: 'POST',
    body: JSON.stringify(body),
    headers: idempotencyKey ? { 'Idempotency-Key': idempotencyKey } : undefined,
    signal,
  }),
}

/** A random key so a retried submission cannot create a duplicate record. */
export function newIdempotencyKey(): string {
  return `ui-${crypto.randomUUID()}`
}

export type ReviewStatus = 'pending' | 'approved' | 'rejected'

export type Percentiles = { p50: number; p90: number; p95: number; p99: number; max: number }

export type RecommendationRecord = {
  id: string
  cluster?: string
  namespace: string
  kind?: string
  workload: string
  container: string
  replicas?: number
  cpuRequestMilli: number
  memoryRequestBytes: number
  cpuLimitMilli: number
  memoryLimitBytes: number
  currentCpuRequestMilli: number
  currentMemoryRequestBytes: number
  currentCpuLimitMilli: number
  currentMemoryLimitBytes: number
  cpuStatisticsMilli: Percentiles
  memoryStatisticsBytes: Percentiles
  confidence: 'low' | 'medium' | 'high'
  confidenceScore: number
  risk: 'low' | 'medium' | 'high'
  burstable: boolean
  cpuThrottlingRatio?: number
  trends?: Trends
  backtest?: Backtest
  dataQuality: { cpuSampleCount: number; memorySampleCount: number; minimumRequired: number; sufficient: boolean; issues: string[] }
  estimatedCpuChangePercent?: number
  estimatedMemoryChangePercent?: number
  reason: string
  warnings: string[]
  applied: boolean
  createdAt: string
  createdBy?: string
  source: 'request' | 'prometheus'
  reviewStatus: ReviewStatus
  reviewedBy?: string
  reviewedAt?: string
  reviewComment?: string
}

export type ContainerSpec = {
  name: string
  cpuRequestMilli: number
  cpuLimitMilli: number
  memoryRequestBytes: number
  memoryLimitBytes: number
}

export type WorkloadRecord = {
  id: string
  namespace: string
  kind: string
  name: string
  containers?: ContainerSpec[]
}

export type Subject = { namespace: string; kind: string; workload: string; container: string }

export type UsagePoint = { timestamp: string; value: number }

export type UsageSeries = {
  resource: 'cpu' | 'memory'
  unit: 'millicores' | 'bytes'
  points: UsagePoint[]
  quality: { issues: string[] | null; coverage: number; expectedSamples: number; acceptedSamples: number }
}

export type HistoryResponse = {
  items: UsageSeries[]
  status: ApiStatus
  step?: string
  warnings?: string[]
  current?: ContainerSpec
}

export type ForecastResponse = {
  resource: 'cpu' | 'memory'
  unit: 'millicores' | 'bytes'
  step: string
  history: UsagePoint[]
  forecast: Array<{ timestamp: string; value: number; lower: number; upper: number }>
  confidenceLevel: number
  warning: string
  diagnostics: {
    model: string
    modelVersion: string
    fallbackUsed: boolean
    fallbackReasons: string[]
    confidence: number
    warning: string
  }
}

export type SavingsSummary = {
  estimatedMonthlySavings: number
  approvedMonthlySavings: number
  currency: string
  recommendations: number
  truncated: boolean
  basis: string
}

export type DependencyStatus = {
  name: string
  configured: boolean
  healthy: boolean
  critical: boolean
  detail: string
}

export type Policy = {
  cpuSafetyMargin: number
  memorySafetyMargin: number
  limitSafetyMargin: number
  minimumCpuMilli: number
  minimumMemoryBytes: number
  maximumCpuMilli: number
  maximumMemoryBytes: number
  maximumReduction: number
  minimumSamples: number
  highConfidenceSamples: number
  cpuPercentile: number
  memoryPercentile: number
}

export type PolicyProfile = { name: string; description: string; default: boolean; policy: Policy }

/** A recommendation calculated with dryRun: not stored, no ID. */
export type RecommendationPreview = {
  dryRun: true
  recommendation: Omit<RecommendationRecord, 'id' | 'createdAt' | 'source' | 'reviewStatus'> & { cpuThrottlingRatio?: number; effectivePolicy: Policy }
  window: string
  step: string
  events: { available: boolean; restarts: number; oomKills: number; throttlingAvailable: boolean; throttlingRatio: number }
}

export type EfficiencyRow = {
  namespace: string
  workloads: number
  pods: number
  containersWithoutRequests: number
  usageAvailable: boolean
  cpuRequestedMilli: number
  cpuAverageMilli?: number
  cpuP95Milli?: number
  cpuEfficiency?: number
  cpuReclaimableMilli?: number
  memoryRequestedBytes: number
  memoryAverageBytes?: number
  memoryP95Bytes?: number
  memoryEfficiency?: number
  memoryReclaimableBytes?: number
  requestedMonthlyCost?: number
  reclaimableMonthlyCost?: number
}

export type EfficiencyResponse = {
  items: EfficiencyRow[]
  status: ApiStatus
  summary?: {
    namespaces: number
    workloads: number
    containersWithoutRequests: number
    cpuRequestedMilli: number
    cpuAverageMilli: number
    cpuReclaimableMilli: number
    cpuEfficiency?: number
    memoryRequestedBytes: number
    memoryAverageBytes: number
    memoryReclaimableBytes: number
    memoryEfficiency?: number
    reclaimableMonthlyCost?: number
    currency?: string
    window: string
    basis: string
  }
}

export type Exceedance = { overRequest?: number; overLimit?: number }
export type Backtest = { cpuCurrent: Exceedance; cpuRecommended: Exceedance; memoryCurrent: Exceedance; memoryRecommended: Exceedance }
export type TrendSummary = { slopePerDay: number; growthPerDayPercent: number; rSquared: number; significant: boolean; projectedUplift: number }
export type Trends = { cpu: TrendSummary; memory: TrendSummary }

export type SignalEvent = { timestamp: string; kind: 'restart' | 'oom_kill' | 'rollout'; count?: number; detail?: string }
export type Insight = { severity: 'info' | 'warning' | 'critical'; title: string; detail: string }

export type SignalsResponse = {
  status: ApiStatus
  signals?: {
    start: string
    end: string
    step: string
    cpu: UsageSeries
    memory: UsageSeries
    throttling?: UsageSeries
    replicas?: UsageSeries
    latency?: UsageSeries
    events: SignalEvent[]
    warnings: string[]
  }
  current?: ContainerSpec
  correlations?: { cpuThrottling?: number; cpuReplicas?: number; cpuLatency?: number; throttlingLatency?: number; memoryLatency?: number }
  insights?: Insight[]
  latencyConfigured?: boolean
}

export type CapacityResponse = {
  status: ApiStatus
  nodes?: { total: number; schedulable: number }
  allocatable?: { cpuMilli: number; memoryBytes: number }
  current?: { nodesNeeded: number; unplacedPods: number; cpuRequestedMilli: number; memoryRequestedBytes: number }
  recommended?: { nodesNeeded: number; unplacedPods: number; cpuRequestedMilli: number; memoryRequestedBytes: number }
  reclaimableNodes?: number
  containersWithRecommendations?: number
  autoscaling?: {
    karpenter: boolean
    clusterAutoscaler: boolean
    nodePools: Array<{ name: string; consolidationPolicy: string; consolidateAfter?: string; disruptionBudgets: string[] }>
    warnings: string[]
  }
  realization?: string
  assumptions?: string[]
}

export type WorkloadEfficiencyRow = {
  kind: string
  name: string
  replicas: number
  podsObserved: number
  containersWithoutRequests: number
  cpuRequestPerPodMilli: number
  cpuAveragePerPodMilli?: number
  cpuP95PerPodMilli?: number
  cpuEfficiency?: number
  cpuReclaimableMilli?: number
  memoryRequestPerPodBytes: number
  memoryAveragePerPodBytes?: number
  memoryP95PerPodBytes?: number
  memoryEfficiency?: number
  memoryReclaimableBytes?: number
  reclaimableMonthlyCost?: number
}
