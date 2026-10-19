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
  post: <T>(path: string, body: unknown, idempotencyKey?: string) => request<T>(path, {
    method: 'POST',
    body: JSON.stringify(body),
    headers: idempotencyKey ? { 'Idempotency-Key': idempotencyKey } : undefined,
  }),
}
