import { fireEvent, render, screen } from '@testing-library/react'
import { seriesColors } from '../chartColors'
import { LineChart } from './LineChart'

const points = [0, 1, 2].map((index) => ({ time: Date.UTC(2026, 9, 1, index), value: 100 + index * 50 }))

describe('LineChart', () => {
  it('draws one path per series, labels references, and reads values by keyboard', () => {
    const { container } = render(
      <LineChart title="CPU" series={[{ name: 'Usage', color: seriesColors[0], points }]} references={[{ label: 'Request', value: 500 }]} format={(value) => `${value}m`} />,
    )
    expect(container.querySelectorAll('path[stroke="#3987e5"]')).toHaveLength(1)
    expect(screen.getByText('Request 500m')).toBeInTheDocument()
    // A single series needs no legend.
    expect(screen.queryByLabelText('CPU legend')).not.toBeInTheDocument()

    fireEvent.keyDown(screen.getByRole('img', { name: /CPU chart/ }), { key: 'ArrowRight' })
    expect(screen.getByRole('status')).toHaveTextContent('100m')
  })

  it('shows a legend for two series and an empty state without data', () => {
    render(
      <LineChart title="Memory" series={[{ name: 'Observed', color: seriesColors[0], points }, { name: 'Forecast', color: seriesColors[1], points }]} format={String} />,
    )
    expect(screen.getByLabelText('Memory legend')).toHaveTextContent('Forecast')
    render(<LineChart title="Disk" series={[]} format={String} />)
    expect(screen.getByText(/No disk samples/)).toBeInTheDocument()
  })
})

describe('LineChart references', () => {
  it('moves a crowded reference label below its line', () => {
    render(<LineChart title="CPU" series={[{ name: 'Usage', color: seriesColors[0], points }]} references={[{ label: 'Request', value: 1000 }, { label: 'Limit', value: 1050 }]} format={(value) => `${value}m`} />)
    const request = Number(screen.getByText('Request 1000m').getAttribute('y'))
    const limit = Number(screen.getByText('Limit 1050m').getAttribute('y'))
    expect(Math.abs(request - limit)).toBeGreaterThanOrEqual(12)
  })
})
