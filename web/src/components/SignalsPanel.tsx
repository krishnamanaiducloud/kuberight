import type { ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Alert, Box, Card, CardContent, Chip, LinearProgress, Stack, Table, TableBody, TableCell, TableHead, TableRow, Tooltip, Typography } from '@mui/material'
import ErrorOutline from '@mui/icons-material/ErrorOutline'
import InfoOutlined from '@mui/icons-material/InfoOutlined'
import WarningAmberOutlined from '@mui/icons-material/WarningAmberOutlined'
import { api, ApiError, type SignalEvent, type SignalsResponse, type Subject, type UsageSeries } from '../api'
import { seriesColors, statusColors } from '../chartColors'
import { formatBytes, formatDate, formatMillicores, spikeLabel } from '../format'
import { LineChart, type ChartMarker, type ChartReference } from './LineChart'

const eventStyle: Record<SignalEvent['kind'], { tone: ChartMarker['tone']; label: string }> = {
  oom_kill: { tone: 'critical', label: 'OOM kill' },
  restart: { tone: 'warning', label: 'Restart' },
  rollout: { tone: 'neutral', label: 'Rollout' },
}

const insightIcon = {
  critical: <ErrorOutline sx={{ color: statusColors.critical }} aria-hidden />,
  warning: <WarningAmberOutlined sx={{ color: statusColors.warning }} aria-hidden />,
  info: <InfoOutlined color="info" aria-hidden />,
}

const correlationLabels: Array<[keyof NonNullable<SignalsResponse['correlations']>, string]> = [
  ['cpuThrottling', 'CPU ↔ throttling'],
  ['throttlingLatency', 'Throttling ↔ latency'],
  ['cpuLatency', 'CPU ↔ latency'],
  ['memoryLatency', 'Memory ↔ latency'],
  ['cpuReplicas', 'CPU ↔ replicas'],
]

const points = (series?: UsageSeries, scale = 1) => series?.points.map((point) => ({ time: Date.parse(point.timestamp), value: point.value * scale })) ?? []
const percent = (value: number) => `${value.toFixed(value < 10 ? 1 : 0)}%`
const seconds = (value: number) => (value < 1 ? `${Math.round(value * 1000)} ms` : `${value.toFixed(2)} s`)
const count = (value: number) => value.toFixed(value % 1 === 0 ? 0 : 1)

function SignalChart({ title, caption, children }: { title: string; caption?: string; children: ReactNode }) {
  return (
    <Card>
      <CardContent>
        <Typography variant="subtitle1" sx={{ fontWeight: 750 }}>{title}</Typography>
        {caption && <Typography variant="caption" color="text.secondary">{caption}</Typography>}
        {children}
      </CardContent>
    </Card>
  )
}

/**
 * Correlated signals for one container: usage, throttling, replicas, and
 * optional latency on one time axis, with restarts, OOM kills, and rollouts
 * marked on every chart, plus the findings derived from them.
 */
export function SignalsPanel({ subject, usageWindow }: { subject: Subject; usageWindow: string }) {
  const query = useQuery({
    queryKey: ['signals', subject, usageWindow],
    queryFn: ({ signal }) => api.get<SignalsResponse>(`/api/v1/metrics/signals?${new URLSearchParams({ ...subject, window: usageWindow })}`, { signal }),
  })
  const data = query.data
  const signals = data?.signals
  if (query.isLoading) return <LinearProgress aria-label="Loading signals" />
  if (query.error) return <Alert severity="error">{query.error instanceof ApiError ? query.error.message : 'Signals could not be loaded.'}</Alert>
  if (!data || !signals) return <Alert severity="info">{data?.status.reason ?? 'No signals are available.'}</Alert>

  const domain: [number, number] = [Date.parse(signals.start), Date.parse(signals.end)]
  const markers: ChartMarker[] = signals.events.map((event) => ({
    time: Date.parse(event.timestamp), tone: eventStyle[event.kind].tone,
    label: `${eventStyle[event.kind].label}${event.count && event.count > 1 ? ` ×${event.count}` : ''} at ${formatDate(event.timestamp)}`,
  }))
  const current = data.current
  const cpuReferences: ChartReference[] = [
    ...(current?.cpuRequestMilli ? [{ label: 'Request', value: current.cpuRequestMilli }] : []),
    ...(current?.cpuLimitMilli ? [{ label: 'Limit', value: current.cpuLimitMilli }] : []),
  ]
  const memoryReferences: ChartReference[] = [
    ...(current?.memoryRequestBytes ? [{ label: 'Request', value: current.memoryRequestBytes }] : []),
    ...(current?.memoryLimitBytes ? [{ label: 'Limit', value: current.memoryLimitBytes }] : []),
  ]
  const correlations = correlationLabels.filter(([key]) => data.correlations?.[key] !== undefined)
  const shared = { domain, markers, height: 200 }

  return (
    <Stack gap={2}>
      {(data.insights ?? []).length > 0 && (
        <Box sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr', md: 'repeat(2, 1fr)' }, gap: 1.5 }}>
          {data.insights!.map((insight) => (
            <Card key={insight.title} sx={{ borderLeft: '3px solid', borderLeftColor: insight.severity === 'info' ? 'info.main' : statusColors[insight.severity] }}>
              <CardContent sx={{ py: 1.5, '&:last-child': { pb: 1.5 } }}>
                <Stack direction="row" gap={1} alignItems="center">
                  {insightIcon[insight.severity]}
                  <Typography sx={{ fontWeight: 750 }}>{insight.title}</Typography>
                </Stack>
                <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5 }}>{insight.detail}</Typography>
              </CardContent>
            </Card>
          ))}
        </Box>
      )}

      <Stack direction="row" gap={1} flexWrap="wrap" alignItems="center">
        {correlations.map(([key, label]) => (
          <Tooltip key={key} title="Pearson correlation over shared timestamps: 1 moves together, −1 moves opposite, 0 unrelated">
            <Chip size="small" variant="outlined" label={`${label}: r = ${data.correlations![key]!.toFixed(2)}`} color={Math.abs(data.correlations![key]!) >= 0.5 ? 'warning' : 'default'} />
          </Tooltip>
        ))}
        {data.spikes && spikeLabel(data.spikes) && <Chip size="small" variant="outlined" color={data.spikes.classification === 'rare' ? 'default' : 'warning'} label={spikeLabel(data.spikes)} />}
        {!data.latencyConfigured && <Typography variant="caption" color="text.secondary">Set KUBERIGHT_LATENCY_QUERY to correlate application latency.</Typography>}
        <Box sx={{ flexGrow: 1 }} />
        <Stack direction="row" gap={1.5} aria-label="Event legend">
          {Object.values(eventStyle).map((style) => (
            <Stack key={style.label} direction="row" gap={0.5} alignItems="center">
              <Box sx={{ width: 3, height: 14, bgcolor: statusColors[style.tone], borderRadius: 1 }} />
              <Typography variant="caption" color="text.secondary">{style.label}</Typography>
            </Stack>
          ))}
        </Stack>
      </Stack>

      <SignalChart title="CPU" caption="Busiest replica, millicores">
        <LineChart title="CPU" series={[{ name: 'CPU', color: seriesColors[0], points: points(signals.cpu) }]} references={cpuReferences} format={formatMillicores} {...shared} />
      </SignalChart>
      {signals.throttling && (
        <SignalChart title="CPU throttling" caption="Share of CFS periods throttled">
          <LineChart title="CPU throttling" series={[{ name: 'Throttled', color: seriesColors[0], points: points(signals.throttling, 100) }]} references={[{ label: '5% threshold', value: 5 }]} format={percent} {...shared} />
        </SignalChart>
      )}
      <SignalChart title="Memory working set" caption="Busiest replica">
        <LineChart title="Memory working set" series={[{ name: 'Memory', color: seriesColors[0], points: points(signals.memory) }]} references={memoryReferences} format={formatBytes} {...shared} />
      </SignalChart>
      {signals.replicas && (
        <SignalChart title="Replicas" caption="Pods reporting usage">
          <LineChart title="Replicas" series={[{ name: 'Replicas', color: seriesColors[0], points: points(signals.replicas) }]} format={count} {...shared} />
        </SignalChart>
      )}
      {signals.latency && (
        <SignalChart title="Latency" caption="From KUBERIGHT_LATENCY_QUERY">
          <LineChart title="Latency" series={[{ name: 'Latency', color: seriesColors[0], points: points(signals.latency) }]} format={seconds} {...shared} />
        </SignalChart>
      )}

      {signals.events.length > 0 && (
        <Card>
          <CardContent>
            <Typography variant="subtitle1" sx={{ fontWeight: 750, mb: 1 }}>Events</Typography>
            <Table size="small" aria-label="Container events">
              <TableHead><TableRow><TableCell>Time</TableCell><TableCell>Event</TableCell><TableCell>Detail</TableCell></TableRow></TableHead>
              <TableBody>
                {signals.events.map((event) => (
                  <TableRow key={`${event.timestamp}-${event.kind}`}>
                    <TableCell>{formatDate(event.timestamp)}</TableCell>
                    <TableCell>
                      <Stack direction="row" gap={0.75} alignItems="center">
                        <Box sx={{ width: 3, height: 14, bgcolor: statusColors[eventStyle[event.kind].tone], borderRadius: 1 }} />
                        {eventStyle[event.kind].label}{event.count && event.count > 1 ? ` ×${event.count}` : ''}
                      </Stack>
                    </TableCell>
                    <TableCell>{event.detail ?? '—'}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}
      {signals.warnings.length > 0 && <Typography variant="caption" color="text.secondary">{signals.warnings.join(' · ')}</Typography>}
    </Stack>
  )
}
