import { api, ApiError, tokenStore } from './api'
import { vi } from 'vitest'

describe('API client', () => {
  it('uses a session-only bearer token and surfaces request IDs', async () => {
    tokenStore.set('opaque-test-token')
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 403,
      headers: new Headers({ 'X-Request-ID': 'request-123' }),
      json: async () => ({ error: { message: 'forbidden' } }),
    })
    vi.stubGlobal('fetch', fetchMock)
    await expect(api.get('/api/v1/clusters')).rejects.toMatchObject({ status: 403, requestId: 'request-123' } satisfies Partial<ApiError>)
    expect(new Headers(fetchMock.mock.calls[0][1].headers).get('Authorization')).toBe('Bearer opaque-test-token')
    tokenStore.set('')
    vi.unstubAllGlobals()
  })
})
