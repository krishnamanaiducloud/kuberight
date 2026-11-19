import { render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { vi } from 'vitest'
import { Shell } from './Shell'

function renderShell(status: number, body: unknown) {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: status < 400, status, headers: new Headers(), json: async () => body }))
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<QueryClientProvider client={client}><MemoryRouter><Shell><p>content</p></Shell></MemoryRouter></QueryClientProvider>)
}

describe('Shell sign-in', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    sessionStorage.clear()
  })

  it('asks for no token when a sign-in proxy session authenticates the dashboard', async () => {
    renderShell(200, { version: 'test', mode: 'recommend' })
    expect(await screen.findByText('Recommend mode')).toBeInTheDocument()
    // The token control is labelled by its tooltip; its text says what it does.
    expect(screen.queryByText(/^(Connect|Update access)$/)).not.toBeInTheDocument()
  })

  it('offers the token dialog when the server requires a bearer token', async () => {
    renderShell(401, { error: { code: 'unauthorized', message: 'a bearer token is required' } })
    expect((await screen.findByText('Connect')).closest('button')).toBeInTheDocument()
  })

  it('keeps the control to change a token that is already set', async () => {
    sessionStorage.setItem('kuberight.token', 'a-token')
    renderShell(200, { version: 'test', mode: 'recommend' })
    await waitFor(() => expect(screen.getByText('Update access').closest('button')).toBeInTheDocument())
  })
})
