import { render, screen } from '@testing-library/react'
import { UsageMeter } from './UsageMeter'

const format = (value: number) => `${value}m`

describe('UsageMeter', () => {
  it('describes average usage as a share of the request', () => {
    render(<UsageMeter label="shop CPU" requested={2000} average={500} p95={800} format={format} />)
    expect(screen.getByText('25% of request used on average')).toBeInTheDocument()
    expect(screen.getByLabelText('shop CPU: requested 2000m, average 500m, P95 800m')).toBeInTheDocument()
  })

  it('states over-request usage in words, not only color', () => {
    render(<UsageMeter label="hot CPU" requested={500} average={400} p95={900} format={format} />)
    expect(screen.getByText('P95 exceeds request')).toBeInTheDocument()
  })

  it('never shows missing usage as zero', () => {
    render(<UsageMeter label="quiet CPU" requested={500} format={format} />)
    expect(screen.getByText('No usage reported')).toBeInTheDocument()
  })
})
