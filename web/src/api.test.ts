import { api, ApiError } from './api'
import { vi } from 'vitest'

describe('API client', () => {
  it('sends no credentials and surfaces request IDs', async () => {
    sessionStorage.setItem('kuberight.token', 'left-over-from-an-older-release')
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 403,
      headers: new Headers({ 'X-Request-ID': 'request-123' }),
      json: async () => ({ error: { message: 'forbidden' } }),
    })
    vi.stubGlobal('fetch', fetchMock)
    await expect(api.get('/api/v1/clusters')).rejects.toMatchObject({ status: 403, requestId: 'request-123' } satisfies Partial<ApiError>)
    expect(new Headers(fetchMock.mock.calls[0][1].headers).get('Authorization')).toBeNull()
    sessionStorage.clear()
    vi.unstubAllGlobals()
  })
})
