import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { vi } from 'vitest'
import { Shell } from './Shell'

function renderShell(response: Promise<unknown>) {
  const fetchMock = vi.fn().mockReturnValue(response)
  vi.stubGlobal('fetch', fetchMock)
  // The Shell sets its own retry policy for the mode; defaults would hide it.
  const client = new QueryClient()
  render(<QueryClientProvider client={client}><MemoryRouter><Shell><p>content</p></Shell></MemoryRouter></QueryClientProvider>)
  return fetchMock
}

describe('Shell without sign-in', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('shows the mode and offers no sign-in of any kind', async () => {
    const fetchMock = renderShell(Promise.resolve({ ok: true, status: 200, headers: new Headers(), json: async () => ({ version: 'test', mode: 'recommend' }) }))
    expect(await screen.findByText('Recommend mode')).toBeInTheDocument()
    expect(screen.queryByText(/connect|sign in|token|unconfirmed/i)).not.toBeInTheDocument()
    expect(new Headers(fetchMock.mock.calls[0][1].headers).get('Authorization')).toBeNull()
  })

  it('says the API is being asked while it has not answered', () => {
    renderShell(new Promise(() => {}))
    expect(screen.getByText('Checking mode')).toBeInTheDocument()
    expect(screen.queryByText(/unconfirmed/i)).not.toBeInTheDocument()
  })
})
