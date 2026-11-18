import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type PointerEvent } from 'react'
import { Box, Stack, Typography } from '@mui/material'
import { statusColors } from '../chartColors'

const textPrimary = '#eef8f6'
const textSecondary = '#a9bec8'
const gridColor = 'rgba(148,188,196,.14)'

export type ChartPoint = { time: number; value: number }
export type ChartSeries = { name: string; color: string; points: ChartPoint[] }
export type ChartBand = { color: string; points: Array<{ time: number; lower: number; upper: number }> }
export type ChartReference = { label: string; value: number }
export type ChartMarker = { time: number; label: string; tone: 'critical' | 'warning' | 'neutral' }

type LineChartProps = {
  title: string
  series: ChartSeries[]
  band?: ChartBand
  references?: ChartReference[]
  format: (value: number) => string
  height?: number
  /** Fixes the time axis so stacked charts line up. */
  domain?: [number, number]
  /** Vertical event markers, such as restarts and rollouts. */
  markers?: ChartMarker[]
}

const margin = { top: 12, right: 16, bottom: 28, left: 64 }

function useWidth() {
  const ref = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(720)
  useEffect(() => {
    const element = ref.current
    if (!element || typeof ResizeObserver === 'undefined') return
    const observer = new ResizeObserver(([entry]) => setWidth(Math.max(280, Math.floor(entry.contentRect.width))))
    observer.observe(element)
    return () => observer.disconnect()
  }, [])
  return [ref, width] as const
}

function niceMaximum(value: number) {
  if (value <= 0) return 1
  const magnitude = 10 ** Math.floor(Math.log10(value))
  const normalized = value / magnitude
  const step = normalized <= 1 ? 1 : normalized <= 2 ? 2 : normalized <= 5 ? 5 : 10
  return step * magnitude
}

function timeLabel(time: number, span: number) {
  const date = new Date(time)
  return span > 2 * 86_400_000
    ? date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
    : date.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
}

/**
 * A single-unit line chart with a crosshair tooltip that reads every series
 * at the hovered time. Reference lines are dashed, neutral, and directly
 * labeled so requests and limits never compete with measured series.
 */
export function LineChart({ title, series, band, references = [], format, height = 260, domain, markers = [] }: LineChartProps) {
  const [containerRef, width] = useWidth()
  const [active, setActive] = useState<number | null>(null)

  const times = useMemo(() => {
    const all = new Set<number>()
    series.forEach((item) => item.points.forEach((point) => all.add(point.time)))
    return [...all].sort((left, right) => left - right)
  }, [series])

  const plotWidth = width - margin.left - margin.right
  const plotHeight = height - margin.top - margin.bottom
  const minTime = domain?.[0] ?? times[0] ?? 0
  const maxTime = domain?.[1] ?? times[times.length - 1] ?? 1
  const span = Math.max(1, maxTime - minTime)
  const maxValue = niceMaximum(Math.max(
    ...series.flatMap((item) => item.points.map((point) => point.value)),
    ...(band?.points.map((point) => point.upper) ?? []),
    ...references.map((reference) => reference.value),
    0,
  ) * 1.08)
  const x = (time: number) => margin.left + ((time - minTime) / span) * plotWidth
  const y = (value: number) => margin.top + plotHeight - (value / maxValue) * plotHeight
  const path = (points: ChartPoint[]) => points.map((point, index) => `${index ? 'L' : 'M'}${x(point.time).toFixed(1)},${y(point.value).toFixed(1)}`).join('')
  const bandPath = band && band.points.length > 0
    ? `${band.points.map((point, index) => `${index ? 'L' : 'M'}${x(point.time).toFixed(1)},${y(point.upper).toFixed(1)}`).join('')}${[...band.points].reverse().map((point) => `L${x(point.time).toFixed(1)},${y(point.lower).toFixed(1)}`).join('')}Z`
    : undefined

  const pick = (event: PointerEvent<SVGRectElement>) => {
    const bounds = event.currentTarget.getBoundingClientRect()
    const time = minTime + ((event.clientX - bounds.left) / bounds.width) * span
    let nearest = 0
    times.forEach((candidate, index) => {
      if (Math.abs(candidate - time) < Math.abs(times[nearest] - time)) nearest = index
    })
    setActive(nearest)
  }
  const onKeyDown = (event: KeyboardEvent<SVGSVGElement>) => {
    if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return
    event.preventDefault()
    setActive((current) => {
      const start = current ?? (event.key === 'ArrowLeft' ? times.length : -1)
      return Math.min(times.length - 1, Math.max(0, start + (event.key === 'ArrowRight' ? 1 : -1)))
    })
  }

  const activeTime = active === null ? undefined : times[active]
  const readout = activeTime === undefined ? [] : series.flatMap((item) => {
    const point = item.points.find((candidate) => candidate.time === activeTime)
    return point ? [{ name: item.name, color: item.color, value: point.value }] : []
  })
  const activeBand = activeTime === undefined ? undefined : band?.points.find((point) => point.time === activeTime)
  const tooltipLeft = activeTime === undefined ? 0 : Math.min(Math.max(x(activeTime) + 12, 0), width - 232)
  // A label sits above its line unless an earlier label is within 14px,
  // in which case it moves below so close references stay legible.
  const labelY = (index: number) => {
    const lineY = y(references[index].value)
    const crowded = references.slice(0, index).some((other) => Math.abs(y(other.value) - lineY) < 14)
    return crowded ? lineY + 14 : lineY - 6
  }
  const yTicks = [0, 0.25, 0.5, 0.75, 1].map((fraction) => fraction * maxValue)
  const xFractions = width < 520 ? [0, 0.5, 1] : [0, 0.25, 0.5, 0.75, 1]
  const xTicks = times.length ? xFractions.map((fraction) => minTime + fraction * span) : []

  if (times.length === 0) {
    return <Typography color="text.secondary" sx={{ py: 4 }}>No {title.toLowerCase()} samples were returned for this window.</Typography>
  }

  return (
    <Box>
      {series.length >= 2 && (
        <Stack direction="row" gap={2} sx={{ mb: 1 }} aria-label={`${title} legend`}>
          {series.map((item) => (
            <Stack key={item.name} direction="row" alignItems="center" gap={0.75}>
              <Box sx={{ width: 14, height: 3, borderRadius: 2, bgcolor: item.color }} />
              <Typography variant="caption" color="text.secondary">{item.name}</Typography>
            </Stack>
          ))}
          {band && (
            <Stack direction="row" alignItems="center" gap={0.75}>
              <Box sx={{ width: 14, height: 10, borderRadius: 0.5, bgcolor: band.color, opacity: 0.25 }} />
              <Typography variant="caption" color="text.secondary">Confidence interval</Typography>
            </Stack>
          )}
        </Stack>
      )}
      <Box ref={containerRef} sx={{ position: 'relative', width: '100%' }}>
        <svg
          width={width}
          height={height}
          role="img"
          aria-label={`${title} chart. Use the left and right arrow keys to read values.`}
          tabIndex={0}
          onKeyDown={onKeyDown}
          onBlur={() => setActive(null)}
          style={{ display: 'block', outline: 'none' }}
        >
          {yTicks.map((tick) => (
            <g key={tick}>
              <line x1={margin.left} x2={width - margin.right} y1={y(tick)} y2={y(tick)} stroke={gridColor} />
              <text x={margin.left - 8} y={y(tick)} dy="0.32em" textAnchor="end" fontSize={11} fill={textSecondary}>{format(tick)}</text>
            </g>
          ))}
          {xTicks.map((tick, index) => (
            <text key={tick} x={x(tick)} y={height - 8} textAnchor={index === 0 ? 'start' : index === xTicks.length - 1 ? 'end' : 'middle'} fontSize={11} fill={textSecondary}>{timeLabel(tick, span)}</text>
          ))}
          {bandPath && <path d={bandPath} fill={band!.color} opacity={0.18} />}
          {markers.filter((marker) => marker.time >= minTime && marker.time <= maxTime).map((marker) => (
            <g key={`${marker.time}-${marker.label}`}>
              <line x1={x(marker.time)} x2={x(marker.time)} y1={margin.top} y2={margin.top + plotHeight} stroke={statusColors[marker.tone]} strokeWidth={1.5} opacity={0.85} />
              <circle cx={x(marker.time)} cy={margin.top + 4} r={4} fill={statusColors[marker.tone]} stroke="#0d1b28" strokeWidth={2}><title>{marker.label}</title></circle>
            </g>
          ))}
          {references.map((reference, index) => (
            <g key={reference.label}>
              <line x1={margin.left} x2={width - margin.right} y1={y(reference.value)} y2={y(reference.value)} stroke={textSecondary} strokeWidth={1.5} strokeDasharray="6 4" />
              <text x={width - margin.right - 4} y={labelY(index)} textAnchor="end" fontSize={11} fill={textSecondary}>{`${reference.label} ${format(reference.value)}`}</text>
            </g>
          ))}
          {series.map((item) => (
            <path key={item.name} d={path(item.points)} fill="none" stroke={item.color} strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />
          ))}
          {activeTime !== undefined && (
            <g>
              <line x1={x(activeTime)} x2={x(activeTime)} y1={margin.top} y2={margin.top + plotHeight} stroke={textSecondary} strokeWidth={1} />
              {readout.map((item) => (
                <circle key={item.name} cx={x(activeTime)} cy={y(item.value)} r={4.5} fill={item.color} stroke="#0d1b28" strokeWidth={2} />
              ))}
            </g>
          )}
          <rect
            x={margin.left}
            y={margin.top}
            width={Math.max(plotWidth, 0)}
            height={Math.max(plotHeight, 0)}
            fill="transparent"
            onPointerMove={pick}
            onPointerLeave={() => setActive(null)}
          />
        </svg>
        {activeTime !== undefined && readout.length > 0 && (
          <Box role="status" sx={{ position: 'absolute', top: 8, left: tooltipLeft, width: 220, p: 1.25, borderRadius: 1.5, bgcolor: '#091621', border: '1px solid', borderColor: 'divider', pointerEvents: 'none' }}>
            <Typography variant="caption" color="text.secondary">{new Date(activeTime).toLocaleString()}</Typography>
            {readout.map((item) => (
              <Stack key={item.name} direction="row" alignItems="center" gap={1}>
                <Box sx={{ width: 8, height: 8, borderRadius: '50%', bgcolor: item.color, flexShrink: 0 }} />
                <Typography sx={{ fontWeight: 800, color: textPrimary }}>{format(item.value)}</Typography>
                <Typography variant="caption" color="text.secondary" noWrap>{item.name}</Typography>
              </Stack>
            ))}
            {activeBand && <Typography variant="caption" color="text.secondary">Interval {format(activeBand.lower)} – {format(activeBand.upper)}</Typography>}
          </Box>
        )}
      </Box>
      <Box component="details" sx={{ mt: 1, color: 'text.secondary', fontSize: 13 }}>
        <summary style={{ cursor: 'pointer' }}>Show {title.toLowerCase()} data table</summary>
        <Box sx={{ maxHeight: 240, overflow: 'auto', mt: 1 }}>
          <table style={{ width: '100%', borderCollapse: 'collapse' }}>
            <thead><tr><th style={{ textAlign: 'left' }}>Time</th>{series.map((item) => <th key={item.name} style={{ textAlign: 'right' }}>{item.name}</th>)}</tr></thead>
            <tbody>
              {times.map((time) => (
                <tr key={time}>
                  <td>{new Date(time).toLocaleString()}</td>
                  {series.map((item) => {
                    const point = item.points.find((candidate) => candidate.time === time)
                    return <td key={item.name} style={{ textAlign: 'right' }}>{point ? format(point.value) : '—'}</td>
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </Box>
      </Box>
    </Box>
  )
}
